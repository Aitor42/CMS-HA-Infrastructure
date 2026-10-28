package ufw

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// DefaultWANIface is the last-resort name used when the WAN uplink cannot be
// discovered on the router: neither by the MAC declared in the configuration
// nor by the interface that currently carries the default route.
const DefaultWANIface = "enp3s0"

// natRulesPath is where the rendered block is staged on the router.
const natRulesPath = "/tmp/nat.rules"

// ifaceNamePattern accepts the names Linux allows for a network interface
// (IFNAMSIZ is 16, including the terminating NUL). The WAN name comes from
// remote command output and is interpolated into a rules file, so reject
// anything that is not a plain interface name.
var ifaceNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.:@-]{1,15}$`)

// NATSpec is the network-specific input for the router's NAT policy. The
// subnets and the load-balancer VIP are configuration values: hardcoding them
// here silently leaves a deployment with custom addressing without DNAT or
// MASQUERADE after the UFW phase succeeds.
type NATSpec struct {
	WANIface     string
	InternalCIDR string
	MainCIDR     string
	VIP          string
}

func (s NATSpec) validate() error {
	if !ifaceNamePattern.MatchString(s.WANIface) {
		return fmt.Errorf("invalid WAN interface name %q", s.WANIface)
	}
	for _, network := range []struct {
		label string
		cidr  string
	}{
		{"internal", s.InternalCIDR},
		{"main", s.MainCIDR},
	} {
		ip, _, err := net.ParseCIDR(network.cidr)
		if err != nil {
			return fmt.Errorf("%s network CIDR %q is invalid: %w", network.label, network.cidr, err)
		}
		if ip.To4() == nil {
			return fmt.Errorf("%s network CIDR %q is not IPv4", network.label, network.cidr)
		}
	}
	ip := net.ParseIP(s.VIP)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("load-balancer VIP %q is not a valid IPv4 address", s.VIP)
	}
	return nil
}

// WANIface discovers the router's WAN uplink: first by the MAC declared in the
// configuration, then by whichever interface holds the default route, and only
// then by the legacy DefaultWANIface name — which is accepted only when the
// router really has such an interface. Rendering MASQUERADE/DNAT rules for an
// interface that is not there leaves the lab without outbound connectivity
// while every step still reports success, so an unresolvable WAN interface is
// an error, not a guess.
func WANIface(ctx context.Context, pool ssh.Executor, routerIP, wanMAC string) (string, error) {
	if wanMAC != "" {
		// The MAC is interpolated into an awk filter: an unvalidated value
		// would break the expression (or be read as part of it).
		if !config.ValidMAC(wanMAC) {
			return "", fmt.Errorf("config declares an invalid WAN MAC address for the router: %q", wanMAC)
		}
		cmd := fmt.Sprintf("ip -o link | awk '/%s/ {print $2}' | sed 's/://'", wanMAC)
		if out, _, _, err := pool.RunCommand(ctx, routerIP, cmd); err == nil {
			if name := strings.TrimSpace(out); name != "" {
				if strings.ContainsAny(name, " \t\r\n") {
					return "", fmt.Errorf("WAN MAC %s matches multiple interfaces: %q", wanMAC, name)
				}
				if !ifaceNamePattern.MatchString(name) {
					return "", fmt.Errorf("WAN MAC %s resolved to an invalid interface name %q", wanMAC, name)
				}
				return name, nil
			}
		}
	}
	if out, _, _, err := pool.RunCommand(ctx, routerIP, "ip route show default | awk '{print $5}' | head -n1"); err == nil {
		if name := strings.TrimSpace(out); name != "" {
			if strings.ContainsAny(name, " \t\r\n") {
				return "", fmt.Errorf("default route is ambiguous: %q", name)
			}
			if !ifaceNamePattern.MatchString(name) {
				return "", fmt.Errorf("default route resolved to an invalid interface name %q", name)
			}
			return name, nil
		}
	}
	if _, _, code, err := pool.RunCommand(ctx, routerIP, "ip link show "+DefaultWANIface); err == nil && code == 0 {
		return DefaultWANIface, nil
	}
	return "", fmt.Errorf("could not determine the router's WAN interface (nodes.router.mac_wan=%q matches nothing, there is no default route and %s does not exist)", wanMAC, DefaultWANIface)
}

// RenderNATRules renders templates/ufw/nat-rules with the discovered WAN
// interface and the deployment's configured subnets/VIP.
func RenderNATRules(spec NATSpec) ([]byte, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	_, internalNet, _ := net.ParseCIDR(spec.InternalCIDR)
	_, mainNet, _ := net.ParseCIDR(spec.MainCIDR)

	tmplContent, err := cms.TemplatesFS.ReadFile("templates/ufw/nat-rules")
	if err != nil {
		return nil, fmt.Errorf("failed to read NAT rules template: %w", err)
	}
	rendered := strings.NewReplacer(
		"${WAN_IF}", spec.WANIface,
		"${WAN_IFACE}", spec.WANIface,
		"${INTERNAL_CIDR}", internalNet.String(),
		"${MAIN_CIDR}", mainNet.String(),
		"${LB_VIP}", spec.VIP,
	).Replace(string(tmplContent))
	if strings.Contains(rendered, "${") {
		return nil, fmt.Errorf("NAT rules template contains an unresolved placeholder")
	}
	return []byte(rendered), nil
}

// ConfigureNAT installs the router's NAT policy.
//
// The rendered block is spliced into /etc/ufw/before.rules so that it survives
// every later `ufw enable`/`ufw reload`: UFW regenerates the whole ruleset on
// both and would drop the MASQUERADE/DNAT rules otherwise. With apply=true the
// very same block is also loaded into the live nat table right away, which is
// what gives the lab's nodes outbound connectivity *before* UFW itself is
// configured by the Puppet phase.
//
// The function is idempotent: the splice starts by deleting any previously
// injected block, so it can run both early (bootstrap) and late (UFW phase).
func ConfigureNAT(ctx context.Context, pool ssh.Executor, routerIP string, spec NATSpec, apply bool) ([]byte, error) {
	natRules, err := RenderNATRules(spec)
	if err != nil {
		return nil, err
	}

	if err := pool.CopyContent(ctx, routerIP, natRules, natRulesPath, 0600); err != nil {
		return nil, fmt.Errorf("failed to upload NAT rules: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		pool.RunCommand(cleanupCtx, routerIP, "rm -f "+natRulesPath+" /tmp/before.rules.bak")
	}()

	// Take a backup first so any corrupt intermediate result (e.g. duplicated
	// nat tables) is rolled back instead of leaving an unusable ufw config on
	// the router. `set -e` plus the ERR trap restore the backup on every
	// failure of the chain, not only on the final sanity check.
	injectCmd := `set -e
cp /etc/ufw/before.rules /tmp/before.rules.bak
restore() { cp /tmp/before.rules.bak /etc/ufw/before.rules; }
trap restore ERR
sed -i '/^# NAT Rules/,/^COMMIT/{/^COMMIT/d; d}' /etc/ufw/before.rules
sed -i '/^\*nat/,/^COMMIT/{/^COMMIT/d; d}' /etc/ufw/before.rules
awk '/^\*filter/{while((getline line < "/tmp/nat.rules") > 0) print line} {print}' /etc/ufw/before.rules > /tmp/before.rules.new
mv /tmp/before.rules.new /etc/ufw/before.rules
if [ "$(grep -c '^\*nat' /etc/ufw/before.rules)" != "1" ] || [ "$(grep -c '^\*filter' /etc/ufw/before.rules)" != "1" ]; then
    echo "unexpected nat/filter table layout after injection" >&2
    restore
    exit 1
fi
trap - ERR
rm -f /tmp/before.rules.bak`
	if _, _, code, err := pool.RunCommand(ctx, routerIP, injectCmd); err != nil || code != 0 {
		return nil, fmt.Errorf("failed to inject NAT rules (exit %d): %v; router before.rules was restored from backup", code, err)
	}

	if apply {
		// iptables-restore replaces exactly the tables present in the input
		// (here: *nat), and the block is the same one UFW will apply later.
		if _, _, code, err := pool.RunCommand(ctx, routerIP, "iptables-restore < "+natRulesPath); err != nil || code != 0 {
			return nil, fmt.Errorf("failed to load NAT rules into the live nat table (exit %d): %v", code, err)
		}
	}

	return natRules, nil
}
