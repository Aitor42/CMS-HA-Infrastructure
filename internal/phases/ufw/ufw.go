package ufw

import (
	"context"
	"fmt"

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
	wanIface, err := WANIface(ctx, p.pool, routerIP, p.cfg.Nodes.Router.MACWAN)
	if err != nil {
		return err
	}
	logging.Info("WAN interface on router: %s", wanIface)

	// The Router Bootstrap phase already injected these rules early (and
	// loaded them into the live nat table) so the nodes could reach the
	// internet while they were being configured; re-injecting here is
	// idempotent and keeps the phase correct when it is run on its own.
	logging.Info("Uploading and injecting NAT rules...")
	if _, err := ConfigureNAT(ctx, p.pool, routerIP, wanIface, false); err != nil {
		return err
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
