package monitoring

import (
	"context"
	"fmt"
	"strings"
	"time"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
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
	if _, _, err := phases.RunPuppetAgent(ctx, p.pool, monitorIP, "monitor node"); err != nil {
		return err
	}

	logging.Info("Running Puppet agent on exporter nodes in parallel...")
	var exporterIPs []string
	seen := make(map[string]struct{})
	addExporter := func(ip string) {
		if ip == "" {
			return
		}
		if _, dup := seen[ip]; dup {
			return
		}
		seen[ip] = struct{}{}
		exporterIPs = append(exporterIPs, ip)
	}
	for _, m := range p.cfg.Nodes.Masters {
		addExporter(m.IP)
	}
	for _, w := range p.cfg.Nodes.Workers {
		addExporter(w.IP)
	}
	for _, c := range p.cfg.Nodes.CMSFrontends {
		addExporter(c.IP)
	}
	addExporter(p.cfg.Nodes.Storage.IP)
	for _, lb := range p.cfg.Nodes.LBs {
		addExporter(lb.IP)
	}
	addExporter(p.cfg.Nodes.Router.IP)

	// Tolerant: a missing exporter is a monitoring gap, not a deploy stopper.
	if err := phases.RunPuppetAgentParallel(ctx, p.pool, exporterIPs); err != nil {
		logging.Warn("puppet agent issues on exporter nodes (continuing): %v", err)
	}

	logging.Info("Minting K3s bearer token for kubelet metrics...")
	if len(p.cfg.Nodes.Masters) > 0 && p.cfg.Nodes.Masters[0].IP != "" {
		masterIP := p.cfg.Nodes.Masters[0].IP

		// The Prometheus ServiceAccount and its least-privilege, read-only RBAC
		// (ClusterRole prometheus-metrics-reader + matching binding) are declared
		// in kubernetes/monitoring/ and applied by the kubernetes phase, so no
		// over-privileged cluster-admin binding is ever created here. This phase
		// only mints the bearer token the scraper authenticates with.
		// The token mint is retried: it depends on the ServiceAccount that the
		// kubernetes phase applied, and a single API hiccup should not decide
		// whether kubelet metrics exist. Still non-fatal by design (see the
		// exporter note above): a monitoring gap must not stop the deploy.
		var token string
		mintErr := retry.DoContext(ctx, retry.Config{MaxAttempts: 5, Interval: 5 * time.Second, Timeout: 60 * time.Second}, func(attemptCtx context.Context) error {
			out, _, _, rerr := p.pool.RunCommand(attemptCtx, masterIP, "kubectl create token prometheus -n kube-system --duration=8760h")
			if rerr != nil {
				return rerr
			}
			if strings.TrimSpace(out) == "" {
				return fmt.Errorf("kubectl returned an empty token")
			}
			token = strings.TrimSpace(out)
			return nil
		})
		if mintErr != nil {
			logging.Warn("could not mint the Prometheus bearer token, so kubelet metrics will be missing from this deployment: %v", mintErr)
		} else if token != "" {
			// Token is bounded to 1 year (rotate by re-running the phase) so
			// it is not an effectively permanent credential for the
			// kubelet metrics API.
			if err := p.pool.CopyContent(ctx, monitorIP, []byte(token+"\n"), "/etc/prometheus/k3s.token", 0600); err != nil {
				logging.Warn("failed to update Prometheus token file: %v", err)
			} else if _, _, _, rerr := p.pool.RunCommand(ctx, monitorIP, "chown prometheus:prometheus /etc/prometheus/k3s.token && systemctl reload prometheus"); rerr != nil {
				logging.Warn("failed to reload prometheus with the new token: %v", rerr)
			}
		}
	}

	logging.Info("Deploying Prometheus alert rules...")
	if rules, rerr := cms.TemplatesFS.ReadFile("templates/monitoring/alert-rules.yml"); rerr != nil {
		logging.Warn("failed to read embedded alert rules: %v", rerr)
	} else if cerr := p.pool.CopyContent(ctx, monitorIP, rules, "/etc/prometheus/alert-rules.yml", 0644); cerr != nil {
		logging.Warn("failed to deploy alert rules: %v", cerr)
	} else if _, _, _, rerr := p.pool.RunCommand(ctx, monitorIP, "chown prometheus:prometheus /etc/prometheus/alert-rules.yml && systemctl reload prometheus"); rerr != nil {
		logging.Warn("failed to reload prometheus after deploying alert rules: %v", rerr)
	}

	logging.Info("Verifying Prometheus and Grafana services...")
	verifyCmd := "systemctl is-active prometheus && systemctl is-active grafana-server"
	verifyErr := retry.DoContext(ctx, retry.Config{MaxAttempts: 10, Interval: 5 * time.Second, Timeout: 60 * time.Second}, func(attemptCtx context.Context) error {
		_, _, _, rerr := p.pool.RunCommand(attemptCtx, monitorIP, verifyCmd)
		return rerr
	})
	if verifyErr != nil {
		return fmt.Errorf("monitoring services not active: %w", verifyErr)
	}

	// The blackbox exporter is best-effort: its absence is a monitoring gap,
	// not a deploy stopper (Prometheus simply marks the blackbox job down).
	if _, _, _, err := p.pool.RunCommand(ctx, monitorIP, "systemctl is-active prometheus-blackbox-exporter"); err != nil {
		logging.Warn("blackbox exporter is not active on the monitor node: %v", err)
	}

	logging.Info("Testing Prometheus API reachability...")
	apiErr := retry.DoContext(ctx, retry.Config{MaxAttempts: 10, Interval: 5 * time.Second, Timeout: 60 * time.Second}, func(attemptCtx context.Context) error {
		out, _, _, rerr := p.pool.RunCommand(attemptCtx, monitorIP, "curl -s http://localhost:9090/api/v1/status/buildinfo")
		if rerr != nil {
			return rerr
		}
		if !strings.Contains(out, "version") {
			return fmt.Errorf("prometheus API invalid response: %s", out)
		}
		return nil
	})
	if apiErr != nil {
		return fmt.Errorf("prometheus API test failed: %w", apiErr)
	}

	logging.Info("Checking Prometheus scrape targets health...")
	if out, _, _, err := p.pool.RunCommand(ctx, monitorIP, `curl -s http://localhost:9090/api/v1/targets | grep -o '"health":"up"' | wc -l`); err == nil {
		logging.Info("Prometheus active targets reporting UP: %s", strings.TrimSpace(out))
	}

	logging.Success("Monitoring Setup completed successfully.")
	return nil
}
