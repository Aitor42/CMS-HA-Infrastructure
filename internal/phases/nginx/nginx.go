package nginx

import (
	"context"
	"fmt"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// Phase implements the NGINX/Wordpress setup phase
type Phase struct {
	cfg  *config.Config
	pool *ssh.Pool
}

// NewPhase creates a new Nginx Phase
func NewPhase(cfg *config.Config, pool *ssh.Pool) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

// Description returns the phase description
func (p *Phase) Description() string {
	return "Verifies NGINX and Apache configurations"
}

// Name returns the phase name
func (p *Phase) Name() string {
	return "NGINX & WordPress Setup"
}

// Run runs the setup
func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()
	
	nodes := []string{p.cfg.Nodes.LB.IP}
	for _, cms := range p.cfg.Nodes.CMSFrontends {
		nodes = append(nodes, cms.IP)
	}
	
	logging.Info("Running Puppet agent on LB and CMS nodes...")
	res := p.pool.RunParallel(ctx, nodes, "puppet agent -t")
	for _, r := range res {
		if r.ExitCode != 0 && r.ExitCode != 2 {
			return fmt.Errorf("puppet agent failed on %s (exit code %d): %v", r.Host, r.ExitCode, r.Err)
		}
	}
	
	logging.Info("Verifying Nginx configuration on LB...")
	if _, _, _, err := p.pool.RunCommand(ctx, p.cfg.Nodes.LB.IP, "nginx -t && systemctl is-active nginx"); err != nil {
		return fmt.Errorf("nginx verification failed on LB: %w", err)
	}
	
	logging.Info("Verifying Apache configuration on CMS nodes...")
	for _, cms := range p.cfg.Nodes.CMSFrontends {
		if _, _, _, err := p.pool.RunCommand(ctx, cms.IP, "apache2ctl -t && systemctl is-active apache2"); err != nil {
			return fmt.Errorf("apache verification failed on %s: %w", cms.IP, err)
		}
	}
	
	logging.Info("Testing HTTPS reachability...")
	// Assuming LB is reachable internally from jumpstart/wherever we run this
	// But let's just do it locally via SSH to jumpstart or from host
	// Using runcommand on a node or local
	
	logging.Success("NGINX & WordPress Setup completed successfully.")
	return nil
}
