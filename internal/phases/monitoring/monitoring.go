package monitoring

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// Phase implements the Monitoring setup phase
type Phase struct {
	cfg  *config.Config
	pool *ssh.Pool
}

// NewPhase creates a new Monitoring Phase
func NewPhase(cfg *config.Config, pool *ssh.Pool) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

// Description returns the phase description
func (p *Phase) Description() string {
	return "Sets up Prometheus and Grafana monitoring"
}

// Name returns the phase name
func (p *Phase) Name() string {
	return "Monitoring Setup"
}

// Run runs the setup
func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()
	
	monitorIP := p.cfg.Nodes.Monitor.IP
	
	logging.Info("Running Puppet agent on Monitor node...")
	_, _, exitCode, err := p.pool.RunCommand(ctx, monitorIP, "puppet agent -t")
	if exitCode != 0 && exitCode != 2 {
		return fmt.Errorf("puppet agent failed on monitor node (exit code %d): %v", exitCode, err)
	}
	
	logging.Info("Running Puppet agent on exporter nodes in parallel...")
	var exporterIPs []string
	for _, m := range p.cfg.Nodes.Masters { exporterIPs = append(exporterIPs, m.IP) }
	for _, w := range p.cfg.Nodes.Workers { exporterIPs = append(exporterIPs, w.IP) }
	for _, c := range p.cfg.Nodes.CMSFrontends { exporterIPs = append(exporterIPs, c.IP) }
	exporterIPs = append(exporterIPs, p.cfg.Nodes.LB.IP)
	exporterIPs = append(exporterIPs, p.cfg.Nodes.Router.IP)
	
	res := p.pool.RunParallel(ctx, exporterIPs, "puppet agent -t")
	for _, r := range res {
		if r.ExitCode != 0 && r.ExitCode != 2 {
			logging.Warn("puppet agent failed on %s (exit code %d)", r.Host, r.ExitCode)
		}
	}
	
	logging.Info("Verifying Prometheus and Grafana services...")
	verifyCmd := "systemctl is-active prometheus && systemctl is-active grafana-server"
	err = retry.Do(ctx, retry.Config{MaxAttempts: 10, Interval: 5 * time.Second, Timeout: 60 * time.Second}, func() error {
		_, _, _, err := p.pool.RunCommand(ctx, monitorIP, verifyCmd)
		return err
	})
	if err != nil {
		return fmt.Errorf("monitoring services not active: %w", err)
	}
	
	logging.Info("Testing Prometheus API reachability...")
	err = retry.Do(ctx, retry.Config{MaxAttempts: 10, Interval: 5 * time.Second, Timeout: 60 * time.Second}, func() error {
		out, err := p.pool.RunScript(ctx, monitorIP, "curl -s http://localhost:9090/api/v1/status/buildinfo")
		if err != nil {
			return err
		}
		if !strings.Contains(out, "version") {
			return fmt.Errorf("prometheus API invalid response: %s", out)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("prometheus API test failed: %w", err)
	}
	
	logging.Success("Monitoring Setup completed successfully.")
	return nil
}
