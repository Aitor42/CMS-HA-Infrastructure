package ufw

import (
	"context"
	"fmt"
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

// WANIface discovers the router's WAN uplink: first by the MAC declared in the
// configuration, then by whichever interface holds the default route, and only
// then by the legacy DefaultWANIface name — which is accepted only when the
// router really has such an interface. Rendering MASQUERADE/DNAT rules for an
// interface that is not there leaves the lab without outbound connectivity
// while every step still reports success, so an unresolvable WAN interface is
// an error, not a guess.
func WANIface(ctx context.Context, pool *ssh.Pool, routerIP, wanMAC string) (string, error) {
	if wanMAC != "" {
		// The MAC is interpolated into an awk filter: an unvalidated value
		// would break the expression (or be read as part of it).
		if !config.ValidMAC(wanMAC) {
			return "", fmt.Errorf("config declares an invalid WAN MAC address for the router: %q", wanMAC)
		}
		cmd := fmt.Sprintf("ip -o link | awk '/%s/ {print $2}' | sed 's/://'", wanMAC)
		if out, _, _, err := pool.RunCommand(ctx, routerIP, cmd); err == nil {
			if name := strings.TrimSpace(out); name != "" {
				return name, nil
			}
		}
	}
	if out, _, _, err := pool.RunCommand(ctx, routerIP, "ip route show default | awk '{print $5}' | head -n1"); err == nil {
		if name := strings.TrimSpace(out); name != "" {
			return name, nil
		}
	}
	if _, _, code, err := pool.RunCommand(ctx, routerIP, "ip link show "+DefaultWANIface); err == nil && code == 0 {
		return DefaultWANIface, nil
	}
	return "", fmt.Errorf("could not determine the router's WAN interface (nodes.router.mac_wan=%q matches nothing, there is no default route and %s does not exist)", wanMAC, DefaultWANIface)
}

// RenderNATRules renders templates/ufw/nat-rules with the discovered WAN
// interface name.
func RenderNATRules(wanIface string) ([]byte, error) {
	tmplContent, err := cms.TemplatesFS.ReadFile("templates/ufw/nat-rules")
	if err != nil {
		return nil, fmt.Errorf("failed to read NAT rules template: %w", err)
	}
	rendered := strings.NewReplacer(
		"${WAN_IF}", wanIface,
		"${WAN_IFACE}", wanIface,
	).Replace(string(tmplContent))
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
func ConfigureNAT(ctx context.Context, pool *ssh.Pool, routerIP, wanIface string, apply bool) ([]byte, error) {
	natRules, err := RenderNATRules(wanIface)
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
