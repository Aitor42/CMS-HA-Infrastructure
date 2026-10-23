package monitoring

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/catalog"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/shell"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// Phase implements the Monitoring setup phase.
type Phase struct {
	cfg  *config.Config
	pool ssh.Executor
}

// NewPhase creates a new Monitoring Phase.
func NewPhase(cfg *config.Config, pool ssh.Executor) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

// Description returns the phase description.
func (p *Phase) Description() string { return catalog.Monitoring.Description }

// Name returns the phase name.
func (p *Phase) Name() string { return catalog.Monitoring.Name }

// Run verifies the exporters, refreshes the Prometheus token, deploys the alert
// rules and checks the Prometheus API.
func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()

	monitorIP := p.cfg.Nodes.Monitor.IP

	// The Puppet phase (04) converged the monitor node and every exporter once,
	// and it fails the deploy when a node does not converge. Re-running the
	// agent here only re-applied an identical catalog, so this phase proves the
	// exporters are alive instead; the scrape-target count at the end of the
	// phase reports their metrics, it does not gate.
	logging.Info("Verifying node exporters installed by the Puppet phase...")
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

	if err := verifyNodeExporters(ctx, p.pool, exporterIPs); err != nil {
		return err
	}

	logging.Info("Minting K3s bearer token for kubelet metrics...")
	if len(p.cfg.Nodes.Masters) > 0 && p.cfg.Nodes.Masters[0].IP != "" {
		masterIP := p.cfg.Nodes.Masters[0].IP

		// The Prometheus ServiceAccount and its read-only RBAC live in
		// kubernetes/monitoring/ and are applied by the kubernetes phase, so no
		// cluster-admin binding is created here: this phase only mints the
		// bearer token the scraper authenticates with. The mint is retried
		// (the ServiceAccount may not exist yet) but stays non-fatal, like the
		// exporter check above: a monitoring gap must not stop the deploy.
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

	logging.Info("Configuring Grafana admin credentials...")
	if err := p.configureGrafanaAdmin(ctx, monitorIP); err != nil {
		return err
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

// configureGrafanaAdmin installs a per-deployment Grafana admin password in a
// root:grafana 0640 EnvironmentFile and restarts the service. The Puppet
// drop-in loads that file, and the CLI reset makes the change effective even
// when the Grafana database already exists. An operator-supplied value wins;
// otherwise the phase generates 256 bits of randomness and only tells the
// operator where to read it.
func (p *Phase) configureGrafanaAdmin(ctx context.Context, monitorIP string) error {
	password := strings.TrimSpace(p.cfg.Monitoring.GrafanaAdminPassword)
	if password == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return fmt.Errorf("generate Grafana admin password: %w", err)
		}
		password = base64.RawURLEncoding.EncodeToString(raw)
	}

	const path = "/etc/grafana/grafana-admin.env"
	content := "GF_SECURITY_ADMIN_PASSWORD=" + password + "\n"
	if err := p.pool.CopyContent(ctx, monitorIP, []byte(content), path, 0640); err != nil {
		return fmt.Errorf("write Grafana admin credential: %w", err)
	}
	// GF_SECURITY_ADMIN_PASSWORD is only honoured when Grafana creates the
	// admin user; reset it explicitly so a re-run on an existing database does
	// not silently keep admin/admin. The env file is still written so a fresh
	// database starts with the same value before the CLI runs.
	cmd := fmt.Sprintf(
		"chown root:grafana %s && chmod 0640 %s && "+
			"for i in $(seq 1 30); do systemctl is-active --quiet grafana-server && break; sleep 1; done; "+
			"GRAFANA_CLI=$(command -v grafana 2>/dev/null || true); "+
			"[ -x \"$GRAFANA_CLI\" ] || GRAFANA_CLI=/usr/sbin/grafana; "+
			"[ -x \"$GRAFANA_CLI\" ] || GRAFANA_CLI=/usr/share/grafana/bin/grafana; "+
			"\"$GRAFANA_CLI\" cli --homepath /usr/share/grafana --config /etc/grafana/grafana.ini "+
			"admin reset-admin-password %s && systemctl restart grafana-server",
		path, path, shell.Quote(password))
	if _, stderr, code, err := p.pool.RunCommand(ctx, monitorIP, cmd); err != nil || code != 0 {
		return fmt.Errorf("install Grafana admin credential (exit %d): %v %s", code, err, strings.TrimSpace(stderr))
	}
	if p.cfg.Monitoring.GrafanaAdminPassword == "" {
		logging.Info("Grafana admin password generated; read it with: sudo cat %s", path)
	}
	return nil
}

// verifyNodeExporters fails when a node whose metrics the dashboards rely on is
// not running the node exporter that role::base installs and enables. A silent
// gap here would only show up as a hole in Grafana.
func verifyNodeExporters(ctx context.Context, pool ssh.Executor, hosts []string) error {
	if len(hosts) == 0 {
		return nil
	}
	var failures []string
	for _, r := range pool.RunParallel(ctx, hosts, "systemctl is-active prometheus-node-exporter") {
		if r.Err != nil || r.ExitCode != 0 {
			failures = append(failures, fmt.Sprintf("%s (exit %d): %v", r.Host, r.ExitCode, r.Err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("node exporter is not active on %d/%d nodes:\n%s",
			len(failures), len(hosts), strings.Join(failures, "\n"))
	}
	return nil
}
