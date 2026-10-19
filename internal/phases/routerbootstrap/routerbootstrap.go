// Package routerbootstrap gives the perimeter router its gateway role before
// any node has to install packages from the internet.
//
// Every phase after this one installs software on the lab's nodes (Puppet
// agents, DRBD, K3s, Nginx, WordPress, Prometheus…), and none of them can reach
// a package repository unless the router is already routing and NATing. The
// legacy Bash flow had an explicit `bootstrap_router` step for exactly this
// reason; the v2 orchestrator was missing it, so the Puppet phase — the first
// one that downloads anything on the clients — failed with `exit code 4`
// (wget: network failure) on every node.
//
// Three things have to be true before that first download:
//
//  1. the router has deterministic addressing: static gateway addresses on both
//     lab networks and a WAN uplink that owns the default route. The shared
//     client autoinstall DHCPs *every* interface, which on a three-NIC router
//     means three competing default routes, one of them via the router itself;
//     templates/ufw/netplan-router.yaml fixes that and this phase installs it;
//  2. `net.ipv4.ip_forward` is on (role::router persists it too, but that
//     Puppet run happens *after* the agents are installed);
//  3. the NAT policy is in place: both lab networks are MASQUERADEd out of the
//     WAN, and the rules are spliced into /etc/ufw/before.rules so the UFW
//     enable that the Puppet phase triggers cannot drop them.
//
// The phase finishes by proving the whole path — router → internet and
// client → router → internet — with the very URL the next phase downloads, so a
// networking problem is reported here, with diagnostics, instead of surfacing
// as an opaque failure ten minutes into the Puppet phase.
package routerbootstrap

import (
	"context"
	"fmt"
	"strings"
	"time"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/ufw"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

const (
	// netplanPath is the overlay this phase installs on the router.
	netplanPath = "/etc/netplan/99-cms-router.yaml"
	// netplanStaging is where the rendered file is uploaded before it is
	// validated and moved into /etc/netplan.
	netplanStaging = "/tmp/cms-router-netplan.yaml"
	// netplanBackupDir keeps whatever the installer wrote, so a validation
	// failure can be rolled back instead of leaving the router without a
	// network configuration.
	netplanBackupDir = "/root/cms-netplan-backup"
	// probeURL is the artefact the Puppet phase downloads on every node, so a
	// probe of it fails exactly when that phase would.
	probeURL = "https://apt.puppet.com/puppet8-release-noble.deb"
	// probeTimeoutSeconds bounds each download attempt: without it a blackholed
	// route costs minutes per node before failing.
	probeTimeoutSeconds = 20
	// probeAttempts absorbs a transient DNS/HTTP hiccup.
	probeAttempts = 3
)

// Phase implements the router bootstrap.
type Phase struct {
	cfg  *config.Config
	pool *ssh.Pool
}

// NewPhase creates a new Router Bootstrap phase.
func NewPhase(cfg *config.Config, pool *ssh.Pool) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

// Name returns the phase name.
func (p *Phase) Name() string { return "Router Bootstrap" }

// Description returns the phase description.
func (p *Phase) Description() string {
	return "Give the router its gateway role (addresses, forwarding, NAT) before any node needs the internet"
}

// Run runs the bootstrap.
func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()

	router := p.cfg.Nodes.Router
	if router.IP == "" {
		return fmt.Errorf("no router node configured (nodes.router.ip is empty)")
	}

	logging.Info("Waiting for SSH on the router (%s)...", router.IP)
	if err := p.pool.WaitForSSH(ctx, router.IP, 3*time.Minute); err != nil {
		return fmt.Errorf("router %s did not become reachable: %w", router.IP, err)
	}

	if err := p.ensureAddresses(ctx, router); err != nil {
		return err
	}
	if err := p.enableForwarding(ctx, router.IP); err != nil {
		return err
	}
	if err := p.enableNAT(ctx, router); err != nil {
		return err
	}
	if err := p.verifyInternet(ctx, router.IP); err != nil {
		return err
	}

	logging.Success("Router is forwarding both lab networks to the WAN.")
	return nil
}

// ensureAddresses gives the router deterministic addressing: static gateway
// addresses on the internal and main networks and a WAN uplink that keeps DHCP
// but owns the default route.
func (p *Phase) ensureAddresses(ctx context.Context, router config.NodeDetail) error {
	logging.Info("Discovering router interfaces by MAC...")
	intName, err := p.ifaceByMAC(ctx, router.IP, router.MACInternal)
	if err != nil {
		return fmt.Errorf("internal interface: %w", err)
	}
	mainName, err := p.ifaceByMAC(ctx, router.IP, router.MACMain)
	if err != nil {
		return fmt.Errorf("main interface: %w", err)
	}
	wanName, err := p.ifaceByMAC(ctx, router.IP, router.MACWAN)
	if err != nil {
		return fmt.Errorf("WAN interface: %w", err)
	}
	logging.Info("Router interfaces: internal=%s main=%s wan=%s", intName, mainName, wanName)

	intAddr := withPrefix(router.IP, p.cfg.Network.Internal.CIDR, "24")
	mainAddr := withPrefix(p.cfg.Network.Main.Gateway, p.cfg.Network.Main.CIDR, "24")
	if mainAddr == "" {
		return fmt.Errorf("cannot determine the router's main-network address (network.main.gateway)")
	}

	tmplContent, err := cms.TemplatesFS.ReadFile("templates/ufw/netplan-router.yaml")
	if err != nil {
		return fmt.Errorf("failed to read router netplan template: %w", err)
	}
	rendered := strings.NewReplacer(
		"${INT_NAME}", intName,
		"${MAIN_NAME}", mainName,
		"${WAN_NAME}", wanName,
		"${INT_MAC}", router.MACInternal,
		"${MAIN_MAC}", router.MACMain,
		"${WAN_MAC}", router.MACWAN,
		"${INT_ADDR}", intAddr,
		"${MAIN_ADDR}", mainAddr,
	).Replace(string(tmplContent))

	if err := p.pool.CopyContent(ctx, router.IP, []byte(rendered), netplanStaging, 0600); err != nil {
		return fmt.Errorf("failed to upload the router netplan: %w", err)
	}

	// This overlay has to be the only netplan file on the router. netplan emits
	// one systemd-networkd unit per definition and networkd picks the
	// *lexicographically first* unit that matches a link, so the installer's
	// `all-en` (`Match Name=en*`) unit shadows these per-MAC static addresses
	// (reproduced in a container: both units are generated and `all-en` sorts
	// first; measured on a live router: it kept a dynamic 192.168.20.50 and
	// never got 192.168.20.1, so the main network had no gateway).
	//
	// The whole tree is backed up first and restored if the overlay does not
	// parse, so a bad render cannot lock the router out of its own management
	// address. `netplan apply` reconfigures the interfaces this SSH session
	// rides on, so it runs detached and the result is verified from the
	// router's state afterwards.
	script := netplanBootstrapScript(netplanStaging, netplanBackupDir, netplanPath)

	if out, stderr, code, err := p.pool.RunCommand(ctx, router.IP, script); err != nil || code != 0 {
		return fmt.Errorf("failed to apply the router netplan (exit %d): %s %v", code, strings.TrimSpace(stderr), err)
	} else {
		if !strings.Contains(out, "CMS_NETPLAN_APPLY_STARTED") {
			return fmt.Errorf("unexpected output while applying the router netplan: %s", strings.TrimSpace(out))
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "CMS_NETPLAN_RETIRED:") {
				logging.Info("Netplan: %s", strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "CMS_NETPLAN_RETIRED:")))
			}
		}
	}

	if err := p.pool.WaitForSSH(ctx, router.IP, 2*time.Minute); err != nil {
		return fmt.Errorf("router became unreachable after applying its netplan: %w", err)
	}

	// Verify that the addresses are really there: without them the main network
	// has no gateway and every later phase fails. networkd applies the new
	// configuration asynchronously, so poll instead of sampling once; on
	// failure, put the installer's configuration back so the router is left in
	// a working state.
	if err := p.waitForAddresses(ctx, router.IP, []string{intAddr, mainAddr}); err != nil {
		rollback := netplanRestoreScript(netplanBackupDir, netplanPath)
		if _, _, code, rerr := p.pool.RunCommand(ctx, router.IP, rollback); rerr != nil || code != 0 {
			logging.Warn("could not restore the router's previous netplan (exit %d): %v", code, rerr)
		} else {
			logging.Warn("Restored the router's previous netplan configuration.")
		}
		return fmt.Errorf("%w\n--- netplan log ---\n%s--- netplan configuration ---\n%s--- addresses ---\n%s",
			err,
			p.readLog(ctx, router.IP, "/tmp/cms-netplan.log"),
			p.captureCommand(ctx, router.IP, "netplan get"),
			p.captureCommand(ctx, router.IP, "ip -4 -o addr show"),
		)
	}
	logging.Success("Router addressing: %s (internal) and %s (main), WAN %s on DHCP", intAddr, mainAddr, wanName)

	return nil
}

// netplanBootstrapScript is the remote script that makes the rendered overlay
// the router's only netplan configuration. It backs up the whole tree first
// (keeping the original copy), disables cloud-init's network module so a
// competing definition cannot come back after a reboot, reports the retired
// files, and rolls the backup back if `netplan generate` rejects the overlay.
// `netplan apply` is started detached because it reconfigures the interface
// this SSH session rides on; the caller verifies the result afterwards.
func netplanBootstrapScript(staging, backupDir, target string) string {
	return fmt.Sprintf(`set -e
export PATH="$PATH:/usr/sbin:/sbin"
DIR=/etc/netplan
BACKUP=%[2]s
TARGET=%[3]s
mkdir -p "$DIR" "$BACKUP"
if [ ! -f "$BACKUP/.cms-backup-complete" ]; then
    cp -a "$DIR"/. "$BACKUP"/ 2>/dev/null || true
    touch "$BACKUP/.cms-backup-complete"
fi
# cloud-init must not resurrect a competing definition on the next boot
mkdir -p /etc/cloud/cloud.cfg.d
printf 'network: {config: disabled}\n' > /etc/cloud/cloud.cfg.d/99-cms-disable-network.cfg
install -m 600 -o root -g root %[1]s "$TARGET"
RETIRED=$(find "$DIR" -maxdepth 1 -name '*.yaml' ! -name '99-cms-router.yaml' -printf '%%f ')
find "$DIR" -maxdepth 1 -name '*.yaml' ! -name '99-cms-router.yaml' -delete
echo "CMS_NETPLAN_RETIRED: ${RETIRED:-none} (copies kept in $BACKUP)"
if ! netplan generate >/tmp/cms-netplan.log 2>&1; then
    rm -f "$TARGET"
    cp -a "$BACKUP"/. "$DIR"/ 2>/dev/null || true
    netplan apply >/dev/null 2>&1 || true
    echo "netplan generate rejected the router overlay:" >&2
    tail -20 /tmp/cms-netplan.log >&2
    exit 1
fi
nohup netplan apply >>/tmp/cms-netplan.log 2>&1 &
echo CMS_NETPLAN_APPLY_STARTED
`, staging, backupDir, target)
}

// netplanRestoreScript puts the installer's netplan configuration back, so a
// router that did not get its gateway addresses is left in a working state.
func netplanRestoreScript(backupDir, target string) string {
	return fmt.Sprintf(`set -u
export PATH="$PATH:/usr/sbin:/sbin"
rm -f %[2]s
cp -a %[1]s/. /etc/netplan/ 2>/dev/null || true
netplan apply >/dev/null 2>&1 || true
echo CMS_NETPLAN_RESTORED`, backupDir, target)
}

// enableForwarding turns the router into an L3 hop and persists it.
func (p *Phase) enableForwarding(ctx context.Context, routerIP string) error {
	logging.Info("Enabling IPv4 forwarding on the router...")
	script := `set -e
printf 'net.ipv4.ip_forward=1\n' > /etc/sysctl.d/99-ip-forward.conf
sysctl -q -w net.ipv4.ip_forward=1
sysctl -n net.ipv4.ip_forward`
	out, stderr, code, err := p.pool.RunCommand(ctx, routerIP, script)
	if err != nil || code != 0 {
		return fmt.Errorf("failed to enable IPv4 forwarding on the router (exit %d): %s %v", code, strings.TrimSpace(stderr), err)
	}
	if strings.TrimSpace(out) != "1" {
		return fmt.Errorf("router reports net.ipv4.ip_forward=%q after enabling it", strings.TrimSpace(out))
	}
	return nil
}

// enableNAT installs the router's NAT policy, both persistently (before.rules)
// and in the live nat table, so clients can download packages before UFW is
// configured.
func (p *Phase) enableNAT(ctx context.Context, router config.NodeDetail) error {
	wanIface, err := ufw.WANIface(ctx, p.pool, router.IP, router.MACWAN)
	if err != nil {
		return err
	}
	logging.Info("Installing NAT rules on the router (WAN interface %s)...", wanIface)
	if _, err := ufw.ConfigureNAT(ctx, p.pool, router.IP, wanIface, true); err != nil {
		return err
	}
	return nil
}

// verifyInternet proves the path the next phases depend on: the router itself
// can download the Puppet release package, and so can a client node on the
// internal network (whose traffic has to be forwarded and NATed by the router).
func (p *Phase) verifyInternet(ctx context.Context, routerIP string) error {
	logging.Info("Probing internet access through the router...")

	for _, target := range p.probeTargets(routerIP) {
		var lastErr error
		for attempt := 1; attempt <= probeAttempts; attempt++ {
			if err := p.probeFrom(ctx, target.ip, target.label); err == nil {
				logging.Success("Internet reachable from %s", target.label)
				lastErr = nil
				break
			} else {
				lastErr = err
				if attempt < probeAttempts {
					logging.Warn("Probe %d/%d from %s failed, retrying: %v", attempt, probeAttempts, target.label, err)
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(5 * time.Second):
					}
				}
			}
		}
		if lastErr != nil {
			return fmt.Errorf("no internet access from %s (%s): %w\n%s", target.label, target.ip, lastErr, p.diagnostics(ctx, routerIP, target.ip))
		}
	}
	return nil
}

type probeTarget struct {
	ip    string
	label string
}

// probeTargets returns the nodes that must be able to download packages: the
// router itself (its own uplink) and one client on the internal network, whose
// traffic exercises forwarding and MASQUERADE.
func (p *Phase) probeTargets(routerIP string) []probeTarget {
	targets := []probeTarget{{ip: routerIP, label: "the router (its own WAN uplink)"}}

	for _, node := range p.cfg.InternalNodes() {
		if node.IP == "" || node.IP == routerIP || node.IP == p.cfg.Nodes.Jumpstart.IP {
			continue
		}
		targets = append(targets, probeTarget{ip: node.IP, label: node.Name})
		break
	}
	return targets
}

// probeFrom downloads probeURL on host, with a hard timeout so a blackholed
// route fails fast instead of hanging for minutes.
func (p *Phase) probeFrom(ctx context.Context, host, label string) error {
	logging.Info("Downloading %s from %s...", probeURL, label)
	// wget is what the Puppet phase itself uses; curl is only a fallback so a
	// minimal image still gives a meaningful probe result.
	cmd := fmt.Sprintf(`if command -v wget >/dev/null 2>&1; then
  wget -q -T %[1]d -O /dev/null %[2]s
else
  curl -fsS -m %[1]d -o /dev/null %[2]s
fi
echo CMS_PROBE_OK`, probeTimeoutSeconds, probeURL)
	out, stderr, code, err := p.pool.RunCommand(ctx, host, cmd)
	if err != nil || code != 0 {
		return fmt.Errorf("exit %d: %s %v", code, strings.TrimSpace(stderr), err)
	}
	if !strings.Contains(out, "CMS_PROBE_OK") {
		return fmt.Errorf("unexpected probe output: %s", strings.TrimSpace(out))
	}
	return nil
}

// diagnostics collects the router state that explains a failed probe.
func (p *Phase) diagnostics(ctx context.Context, routerIP, host string) string {
	var b strings.Builder
	b.WriteString("--- router diagnostics ---\n")

	cmds := []struct{ label, cmd string }{
		{"ip route", "ip -4 route"},
		{"ip addr", "ip -4 -o addr show"},
		{"ip_forward", "sysctl -n net.ipv4.ip_forward"},
		{"ufw status", "ufw status verbose 2>&1 | head -20"},
		{"nat table", "iptables -t nat -S POSTROUTING 2>&1 | head -10"},
		{"netplan", "cat /tmp/cms-netplan.log 2>/dev/null | tail -10"},
	}
	for _, c := range cmds {
		out, errOut, _, err := p.pool.RunCommand(ctx, routerIP, c.cmd)
		fmt.Fprintf(&b, "$ %s\n%s%s", c.label, out, errOut)
		if err != nil {
			fmt.Fprintf(&b, "(%v)\n", err)
		}
	}

	if host != routerIP {
		fmt.Fprintf(&b, "--- %s diagnostics ---\n", host)
		out, errOut, _, err := p.pool.RunCommand(ctx, host, "ip -4 route; getent hosts apt.puppet.com || true")
		fmt.Fprintf(&b, "%s%s", out, errOut)
		if err != nil {
			fmt.Fprintf(&b, "(%v)\n", err)
		}
	}

	return b.String()
}

// waitForAddresses polls until every address in addrs is present on the host.
func (p *Phase) waitForAddresses(ctx context.Context, host string, addrs []string) error {
	var missing []string
	for attempt := 0; attempt < 12; attempt++ {
		missing = nil
		for _, addr := range addrs {
			cmd := fmt.Sprintf("ip -4 -o addr show | awk '{print $4}' | grep -qx %s", addr)
			if _, _, code, err := p.pool.RunCommand(ctx, host, cmd); err != nil || code != 0 {
				missing = append(missing, addr)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("router is missing its gateway addresses after 60s: %s", strings.Join(missing, ", "))
}

// readLog returns the (tail of the) contents of remotePath, best effort.
func (p *Phase) readLog(ctx context.Context, host, remotePath string) string {
	out, errOut, _, err := p.pool.RunCommand(ctx, host, "tail -20 "+remotePath+" 2>&1")
	if err != nil {
		return fmt.Sprintf("(could not read %s: %v)\n", remotePath, err)
	}
	return out + errOut
}

// captureCommand runs a read-only command for diagnostics, best effort.
func (p *Phase) captureCommand(ctx context.Context, host, cmd string) string {
	out, errOut, _, err := p.pool.RunCommand(ctx, host, cmd+" 2>&1")
	if err != nil {
		return fmt.Sprintf("(could not run %q: %v)\n", cmd, err)
	}
	return out + errOut
}

// ifaceByMAC returns the name of the interface owned by mac, or an error.
func (p *Phase) ifaceByMAC(ctx context.Context, host, mac string) (string, error) {
	if mac == "" {
		return "", fmt.Errorf("no MAC configured")
	}
	cmd := fmt.Sprintf("ip -o link | awk '/%s/ {print $2}' | sed 's/://'", mac)
	out, _, _, err := p.pool.RunCommand(ctx, host, cmd)
	if err != nil {
		return "", fmt.Errorf("lookup of %s failed: %w", mac, err)
	}
	name := strings.TrimSpace(out)
	if name == "" {
		return "", fmt.Errorf("no interface with MAC %s on %s", mac, host)
	}
	return name, nil
}

// withPrefix turns a bare IPv4 address into an address/prefix pair, taking the
// prefix length from cidr (falling back to fallback for a malformed CIDR).
func withPrefix(ip, cidr, fallback string) string {
	if ip == "" {
		return ""
	}
	prefix := fallback
	if _, after, found := strings.Cut(cidr, "/"); found && strings.TrimSpace(after) != "" {
		prefix = strings.TrimSpace(after)
	}
	return ip + "/" + prefix
}
