package nginx

import (
	"context"
	"fmt"
	"strings"

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

	// The Puppet phase (04) already converged every node and fails the deploy
	// if one does not, so re-running it here would only re-apply an identical
	// catalog. This phase checks the preconditions and converges only the
	// frontends, whose catalog depends on state that exists by now (the
	// database from the Kubernetes phase, the NFS export from the storage
	// node).
	var lbIPs []string
	for _, lb := range p.cfg.Nodes.LBs {
		if lb.IP == "" {
			continue
		}
		lbIPs = append(lbIPs, lb.IP)
	}

	// Sequential on purpose: the catalog installs WordPress (apt + unpack), and
	// two frontends converging at the same time was the original install race.
	logging.Info("Converging CMS frontends sequentially (database and uploads now exist)...")
	for _, cms := range p.cfg.Nodes.CMSFrontends {
		if cms.IP == "" {
			continue
		}
		logging.Info("Converging CMS frontend %s (%s)...", cms.Name, cms.IP)
		if _, _, err := phases.RunPuppetAgent(ctx, p.pool, cms.IP, cms.Name); err != nil {
			return err
		}
	}

	logging.Info("Verifying Nginx and keepalived on LB nodes...")
	resLB := p.pool.RunParallel(ctx, lbIPs, "nginx -t && systemctl is-active nginx && systemctl is-active keepalived")
	for _, r := range resLB {
		if r.Err != nil || r.ExitCode != 0 {
			return fmt.Errorf("nginx/keepalived verification failed on %s (exit %d): %v", r.Host, r.ExitCode, r.Err)
		}
	}

	logging.Info("Verifying Apache configuration on CMS nodes in parallel...")
	var cmsIPs []string
	for _, cms := range p.cfg.Nodes.CMSFrontends {
		if cms.IP != "" {
			cmsIPs = append(cmsIPs, cms.IP)
		}
	}
	resApache := p.pool.RunParallel(ctx, cmsIPs, "apache2ctl -t && systemctl is-active apache2")
	for _, r := range resApache {
		if r.Err != nil || r.ExitCode != 0 {
			return fmt.Errorf("apache verification failed on %s (exit %d): %v", r.Host, r.ExitCode, r.Err)
		}
	}

	if p.cfg.Nodes.Storage.IP != "" {
		logging.Info("Verifying NFS server export on Storage node...")
		const exportCmd = "systemctl is-active nfs-kernel-server && exportfs -s | grep -q /srv/nfs/wp-uploads"
		if _, stderr, code, err := p.pool.RunCommand(ctx, p.cfg.Nodes.Storage.IP, exportCmd); err != nil || code != 0 {
			return fmt.Errorf("storage node %s is not exporting /srv/nfs/wp-uploads (exit %d): %v %s",
				p.cfg.Nodes.Storage.Name, code, err, strings.TrimSpace(stderr))
		}
	}

	logging.Info("Verifying shared NFS uploads mount on CMS nodes in parallel...")
	resMount := p.pool.RunParallel(ctx, cmsIPs, "mountpoint -q /var/www/html/wp-content/uploads")
	for _, r := range resMount {
		if r.Err != nil || r.ExitCode != 0 {
			return fmt.Errorf("nfs mount verification failed on %s (exit %d): %v", r.Host, r.ExitCode, r.Err)
		}
	}

	logging.Info("Testing HTTPS reachability on Load Balancer...")
	out, _, _, err := p.pool.RunCommand(ctx, p.cfg.PrimaryLB().IP, "curl -skI https://127.0.0.1/ | grep 'HTTP/'")
	if err != nil || !strings.Contains(out, "HTTP/") {
		logging.Warn("HTTPS check on LB reported: WordPress may still be initialising")
	} else {
		logging.Success("WordPress reachable via HTTPS on Load Balancer")
	}

	logging.Success("NGINX & WordPress Setup completed successfully.")
	return nil
}
