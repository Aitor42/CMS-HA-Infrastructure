package ufw

import (
	"context"
	"fmt"
	"strings"
	"time"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// Phase implements the UFW setup phase
type Phase struct {
	cfg  *config.Config
	pool *ssh.Pool
}

// NewPhase creates a new UFW Phase
func NewPhase(cfg *config.Config, pool *ssh.Pool) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

// Description returns the phase description
func (p *Phase) Description() string {
	return "Sets up UFW rules and NAT"
}

// Name returns the phase name
func (p *Phase) Name() string {
	return "UFW Setup"
}

// Run runs the setup
func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()

	routerIP := p.cfg.Nodes.Router.IP

	logging.Info("Running Puppet agent on Router node...")
	if _, _, err := phases.RunPuppetAgent(ctx, p.pool, routerIP, "router node"); err != nil {
		return err
	}

	logging.Info("Discovering WAN interface by MAC on Router...")
	wanMAC := p.cfg.Nodes.Router.MACWAN
	if wanMAC == "" {
		wanMAC = "52:54:00:10:00:02"
	}
	wanCmd := fmt.Sprintf("ip -o link | awk '/%s/ {print $2}' | sed 's/://'", wanMAC)
	wanIface, _, _, err := p.pool.RunCommand(ctx, routerIP, wanCmd)
	wanIface = strings.TrimSpace(wanIface)
	if err != nil || wanIface == "" {
		// Fallback: detect interface carrying default gateway
		defIface, _, _, _ := p.pool.RunCommand(ctx, routerIP, "ip route show default | awk '{print $5}' | head -n1")
		wanIface = strings.TrimSpace(defIface)
		if wanIface == "" {
			wanIface = "enp3s0"
		}
	}

	logging.Info("Uploading and injecting NAT rules...")
	tmplContent, err := cms.TemplatesFS.ReadFile("templates/ufw/nat-rules")
	if err != nil {
		return fmt.Errorf("failed to read NAT rules template: %w", err)
	}

	natRules := strings.NewReplacer(
		"${WAN_IF}", wanIface,
		"${WAN_IFACE}", wanIface,
	).Replace(string(tmplContent))

	if err := p.pool.CopyContent(ctx, routerIP, []byte(natRules), "/tmp/nat.rules", 0600); err != nil {
		return fmt.Errorf("failed to upload NAT rules: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.pool.RunCommand(cleanupCtx, routerIP, "rm -f /tmp/nat.rules /tmp/before.rules.bak")
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
	if _, _, code, err := p.pool.RunCommand(ctx, routerIP, injectCmd); err != nil || code != 0 {
		return fmt.Errorf("failed to inject NAT rules (exit %d): %v; router before.rules was restored from backup", code, err)
	}

	logging.Info("Reloading UFW on Router...")
	if _, _, _, err := p.pool.RunCommand(ctx, routerIP, "ufw reload"); err != nil {
		return fmt.Errorf("failed to reload ufw on router: %w", err)
	}

	logging.Info("Running Puppet agent on all other nodes in parallel...")
	var otherIPs []string
	for _, node := range p.cfg.AllNodes() {
		if node.IP != "" && node.IP != routerIP && node.IP != p.cfg.Nodes.Jumpstart.IP {
			otherIPs = append(otherIPs, node.IP)
		}
	}

	// Best-effort: a puppet-agent hiccup on a secondary node is logged, not
	// fatal. The router's own firewall is verified strictly below.
	if err := phases.RunPuppetAgentParallel(ctx, p.pool, otherIPs); err != nil {
		logging.Warn("puppet agent issues on other nodes (continuing): %v", err)
	}

	logging.Info("Verifying UFW status on Router...")
	if _, _, _, err := p.pool.RunCommand(ctx, routerIP, "ufw status | grep -i active"); err != nil {
		return fmt.Errorf("ufw is not active on router: %w", err)
	}

	logging.Success("UFW Setup completed successfully.")
	return nil
}
