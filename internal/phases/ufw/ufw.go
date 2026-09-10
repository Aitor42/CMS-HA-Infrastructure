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
	_, _, exitCode, err := p.pool.RunCommand(ctx, routerIP, "/opt/puppetlabs/bin/puppet agent -t || [ $? -eq 2 ]")
	if exitCode != 0 && exitCode != 2 {
		return fmt.Errorf("puppet agent failed on router node (exit code %d): %v", exitCode, err)
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
		wanIface = "enp2s0" // fallback
	}
	
	logging.Info("Uploading and injecting NAT rules...")
	tmplContent, err := cms.TemplatesFS.ReadFile("templates/ufw/nat-rules")
	if err != nil {
		return fmt.Errorf("failed to read NAT rules template: %w", err)
	}

	natRules := strings.ReplaceAll(string(tmplContent), "${WAN_IF}", wanIface)
	natRules = strings.ReplaceAll(natRules, "${WAN_IFACE}", wanIface)
	
	if err := p.pool.CopyContent(ctx, routerIP, []byte(natRules), "/tmp/nat.rules", 0600); err != nil {
		return fmt.Errorf("failed to upload NAT rules: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.pool.RunCommand(cleanupCtx, routerIP, "rm -f /tmp/nat.rules")
	}()

	injectCmd := `grep -q "*nat" /etc/ufw/before.rules || sed -i -e '/\*filter/r /tmp/nat.rules' -e '1N' /etc/ufw/before.rules`
	if _, _, _, err := p.pool.RunCommand(ctx, routerIP, injectCmd); err != nil {
		return fmt.Errorf("failed to inject NAT rules: %w", err)
	}
	
	logging.Info("Reloading UFW on Router...")
	if _, _, _, err := p.pool.RunCommand(ctx, routerIP, "ufw reload"); err != nil {
		return fmt.Errorf("failed to reload ufw on router: %w", err)
	}
	
	logging.Info("Running Puppet agent on all other nodes in parallel...")
	var otherIPs []string
	for _, m := range p.cfg.Nodes.Masters { otherIPs = append(otherIPs, m.IP) }
	for _, w := range p.cfg.Nodes.Workers { otherIPs = append(otherIPs, w.IP) }
	for _, c := range p.cfg.Nodes.CMSFrontends { otherIPs = append(otherIPs, c.IP) }
	otherIPs = append(otherIPs, p.cfg.Nodes.LB.IP)
	otherIPs = append(otherIPs, p.cfg.Nodes.Monitor.IP)
	
	res := p.pool.RunParallel(ctx, otherIPs, "puppet agent -t || [ $? -eq 2 ]")
	for _, r := range res {
		if r.Err != nil {
			logging.Warn("puppet agent failed on %s", r.Host)
		}
	}
	
	logging.Info("Verifying UFW status on Router...")
	if _, _, _, err := p.pool.RunCommand(ctx, routerIP, "ufw status | grep -i active"); err != nil {
		return fmt.Errorf("ufw is not active on router: %w", err)
	}
	
	logging.Success("UFW Setup completed successfully.")
	return nil
}
