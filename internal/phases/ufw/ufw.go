package ufw

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/catalog"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

const (
	// ufwActiveCheck is deliberately anchored: `grep -i active` also matches
	// "Status: inactive", which made an inactive router firewall look healthy.
	ufwActiveCheck = "ufw status | grep -qx 'Status: active'"
	ipForwardCheck = "cat /proc/sys/net/ipv4/ip_forward"
	natTableDump   = "iptables -t nat -S"
)

// Phase implements the UFW setup phase.
type Phase struct {
	cfg  *config.Config
	pool ssh.Executor
}

// NewPhase creates a new UFW Phase.
func NewPhase(cfg *config.Config, pool ssh.Executor) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

// Description returns the phase description.
func (p *Phase) Description() string { return catalog.UFW.Description }

// Name returns the phase name.
func (p *Phase) Name() string { return catalog.UFW.Name }

// Run re-applies the router's NAT policy and verifies the firewall is active
// on every node.
func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()

	routerIP := p.cfg.Nodes.Router.IP
	if routerIP == "" {
		return fmt.Errorf("no router node configured (nodes.router.ip is empty)")
	}

	// Every node's firewall catalog (rules, policies, default-deny) was applied
	// once by the Puppet phase (04), which fails the deploy when a node does not
	// converge. Nothing here re-runs the agent: this phase finishes the parts
	// that are not Puppet state (the NAT rules below) and then proves the
	// firewall is up everywhere.
	logging.Info("Discovering WAN interface by MAC on Router...")
	wanIface, err := WANIface(ctx, p.pool, routerIP, p.cfg.Nodes.Router.MACWAN)
	if err != nil {
		return err
	}
	logging.Info("WAN interface on router: %s", wanIface)

	natSpec := NATSpec{
		WANIface:     wanIface,
		InternalCIDR: p.cfg.Network.Internal.CIDR,
		MainCIDR:     p.cfg.Network.Main.CIDR,
		VIP:          p.cfg.LBVIP(),
	}

	// The Router Bootstrap phase already injected these rules early (and
	// loaded them into the live nat table) so the nodes could reach the
	// internet while they were being configured; re-injecting here is
	// idempotent and keeps the phase correct when it is run on its own.
	logging.Info("Uploading and injecting NAT rules...")
	if _, err := ConfigureNAT(ctx, p.pool, routerIP, natSpec, false); err != nil {
		return err
	}

	logging.Info("Reloading UFW on Router...")
	if out, stderr, code, err := p.pool.RunCommand(ctx, routerIP, "ufw reload"); err != nil || code != 0 {
		return fmt.Errorf("failed to reload ufw on router (exit %d): %s %v", code, strings.TrimSpace(stderr+"\n"+out), err)
	}

	logging.Info("Verifying UFW status on Router...")
	if _, stderr, code, err := p.pool.RunCommand(ctx, routerIP, ufwActiveCheck); err != nil || code != 0 {
		return fmt.Errorf("ufw is not active on router (exit %d): %s %v", code, strings.TrimSpace(stderr), err)
	}
	if err := p.verifyRouterPolicy(ctx, routerIP); err != nil {
		return err
	}
	if err := p.verifyForwarding(ctx, routerIP); err != nil {
		return err
	}
	if err := p.verifyNAT(ctx, routerIP, natSpec); err != nil {
		return err
	}

	logging.Info("Verifying UFW is active on every other node in parallel...")
	otherIPs := p.nodeIPsToVerify()
	var inactive []string
	for _, r := range p.pool.RunParallel(ctx, otherIPs, ufwActiveCheck) {
		if r.Err != nil || r.ExitCode != 0 {
			inactive = append(inactive, fmt.Sprintf("%s (exit %d): %v", r.Host, r.ExitCode, r.Err))
		}
	}
	if len(inactive) > 0 {
		return fmt.Errorf("UFW is not active on %d/%d nodes:\n%s",
			len(inactive), len(otherIPs), strings.Join(inactive, "\n"))
	}

	logging.Success("UFW Setup completed successfully.")
	return nil
}

// nodeIPsToVerify returns every configured node except the router. Jumpstart is
// included: it runs role::jumpstart (which includes role::base), so an inactive
// firewall there is a real deployment failure, not an intentional exception.
func (p *Phase) nodeIPsToVerify() []string {
	routerIP := p.cfg.Nodes.Router.IP
	seen := make(map[string]bool)
	var ips []string
	for _, node := range p.cfg.AllNodes() {
		if node.IP == "" || node.IP == routerIP || seen[node.IP] {
			continue
		}
		seen[node.IP] = true
		ips = append(ips, node.IP)
	}
	return ips
}

// verifyRouterPolicy checks the perimeter policies and route rules that make
// the router a firewall rather than a plain forwarding host. A reload can leave
// the daemon active while a rule is missing, so "Status: active" alone is not
// enough.
func (p *Phase) verifyRouterPolicy(ctx context.Context, routerIP string) error {
	out, stderr, code, err := p.pool.RunCommand(ctx, routerIP, "ufw status verbose")
	if err != nil || code != 0 {
		return fmt.Errorf("could not read ufw status on router (exit %d): %s %v", code, strings.TrimSpace(stderr), err)
	}
	required := []string{
		"Default: deny (incoming)",
		// UFW renders all three policies on one line, so only the first is
		// prefixed with "Default:".
		"allow (outgoing)",
		"deny (routed)",
		"Main -> Internet",
		"Internal -> Internet",
		"Internal -> Main",
		"Main -> Internal",
		"DNAT HTTP -> LB",
		"DNAT HTTPS -> LB",
	}
	var missing []string
	for _, want := range required {
		if !strings.Contains(out, want) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("router UFW is missing required policies/rules: %s", strings.Join(missing, ", "))
	}
	return nil
}

// verifyForwarding proves the kernel will actually route packets between the
// lab networks and the WAN. NAT rules without forwarding silently drop traffic.
func (p *Phase) verifyForwarding(ctx context.Context, routerIP string) error {
	out, stderr, code, err := p.pool.RunCommand(ctx, routerIP, ipForwardCheck)
	if err != nil || code != 0 {
		return fmt.Errorf("could not read net.ipv4.ip_forward on router (exit %d): %s %v", code, strings.TrimSpace(stderr), err)
	}
	if got := strings.TrimSpace(out); got != "1" {
		return fmt.Errorf("router is not forwarding IPv4 packets (net.ipv4.ip_forward=%q); NAT would silently drop forwarded traffic", got)
	}
	return nil
}

// verifyNAT checks the live nat table, not only /etc/ufw/before.rules. A
// successful `ufw reload` can still leave the old table if the daemon skipped
// the block, and the phase must not report success in that case.
func (p *Phase) verifyNAT(ctx context.Context, routerIP string, spec NATSpec) error {
	out, stderr, code, err := p.pool.RunCommand(ctx, routerIP, natTableDump)
	if err != nil || code != 0 {
		return fmt.Errorf("could not read the router's live NAT table (exit %d): %s %v", code, strings.TrimSpace(stderr), err)
	}
	_, internalNet, _ := net.ParseCIDR(spec.InternalCIDR)
	_, mainNet, _ := net.ParseCIDR(spec.MainCIDR)
	checks := []struct {
		label  string
		tokens []string
	}{
		{"DNAT HTTP", []string{"-A PREROUTING", "-i " + spec.WANIface, "--dport 80", "DNAT", "--to-destination " + spec.VIP + ":80"}},
		{"DNAT HTTPS", []string{"-A PREROUTING", "-i " + spec.WANIface, "--dport 443", "DNAT", "--to-destination " + spec.VIP + ":443"}},
		{"MASQUERADE internal", []string{"-A POSTROUTING", "-s " + internalNet.String(), "-o " + spec.WANIface, "MASQUERADE"}},
		{"MASQUERADE main", []string{"-A POSTROUTING", "-s " + mainNet.String(), "-o " + spec.WANIface, "MASQUERADE"}},
	}
	var missing []string
	for _, check := range checks {
		if !lineContainsAll(out, check.tokens...) {
			missing = append(missing, check.label)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("router live NAT table is missing %s:\n%s", strings.Join(missing, ", "), strings.TrimSpace(out))
	}
	return nil
}

// lineContainsAll reports whether one line of out contains every token.
func lineContainsAll(out string, tokens ...string) bool {
	for _, line := range strings.Split(out, "\n") {
		matches := true
		for _, token := range tokens {
			if !strings.Contains(line, token) {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}
