package kubernetes

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/catalog"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// apiRetryBudget bounds how long an idempotent kubectl command keeps waiting
// for the control plane. k3s restarted under memory pressure took ~15s to
// serve again; three minutes covers that with a wide margin without turning a
// genuinely broken cluster into a very long hang.
const apiRetryBudget = 3 * time.Minute

// Phase implements the Kubernetes setup phase.
type Phase struct {
	cfg  *config.Config
	pool ssh.Executor
}

// NewPhase creates a new Kubernetes Phase.
func NewPhase(cfg *config.Config, pool ssh.Executor) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

// Description returns the phase description.
func (p *Phase) Description() string { return catalog.Kubernetes.Description }

// Name returns the phase name.
func (p *Phase) Name() string { return catalog.Kubernetes.Name }

// Run joins the control-plane servers and applies the kustomize manifests,
// including the database and backup resources.
func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()

	if err := p.validateConfig(); err != nil {
		return err
	}

	master1 := p.cfg.Nodes.Masters[0]
	if err := p.bootstrapControlPlane(ctx, master1); err != nil {
		return err
	}
	if err := p.joinControlPlane(ctx, master1); err != nil {
		return err
	}
	if err := p.joinWorkers(ctx, master1); err != nil {
		return err
	}
	if err := p.waitForNodesReady(ctx, master1); err != nil {
		return err
	}
	if err := p.waitForEtcdQuorum(ctx, master1); err != nil {
		return err
	}
	p.labelPrimary(ctx, master1)
	if err := p.deployWorkloads(ctx, master1); err != nil {
		return err
	}

	logging.Success("Kubernetes Setup completed successfully.")
	return nil
}

// validateConfig rejects the node counts and credentials that would make the
// rest of the phase fail halfway through a cluster join.
func (p *Phase) validateConfig() error {
	if len(p.cfg.Nodes.Masters) < 3 {
		return fmt.Errorf("k3s HA requires at least 3 master nodes for etcd quorum (got %d)", len(p.cfg.Nodes.Masters))
	}
	if p.cfg.Database.Password == "" || p.cfg.Database.RootPassword == "" || p.cfg.Database.ExporterPassword == "" || p.cfg.WordPress.AdminPassword == "" {
		return fmt.Errorf("database.password, database.root_password, database.exporter_password, and wordpress.admin_password must be set in config.yaml; cms-ha encrypts them automatically")
	}
	for name, secret := range map[string]string{
		"database.password": p.cfg.Database.Password, "database.root_password": p.cfg.Database.RootPassword,
		"database.exporter_password": p.cfg.Database.ExporterPassword, "wordpress.admin_password": p.cfg.WordPress.AdminPassword,
	} {
		if strings.ContainsAny(secret, "\r\n") {
			return fmt.Errorf("%s cannot contain line breaks", name)
		}
	}
	return nil
}

// bootstrapControlPlane initialises the first server, waits for its API, reads
// the join token and puts it on every node that will join.
func (p *Phase) bootstrapControlPlane(ctx context.Context, master1 config.NodeDetail) error {
	logging.Info("Installing K3s on Master 1 (cluster-init)...")
	initCmd := fmt.Sprintf("curl -sfL https://get.k3s.io | INSTALL_K3S_CHANNEL=\"v1.29\" INSTALL_K3S_EXEC=\"server --cluster-init --secrets-encryption --etcd-expose-metrics --node-external-ip %s --flannel-iface enp1s0\" sh -", master1.IP)
	if err := runChecked(ctx, p.pool, master1.IP, initCmd, "install K3s on master1"); err != nil {
		return err
	}

	logging.Info("Waiting for API server on Master 1...")
	err := retry.DoContext(ctx, retry.Config{MaxAttempts: 150, Interval: 2 * time.Second, Timeout: 5 * time.Minute}, func(attemptCtx context.Context) error {
		_, _, _, err := p.pool.RunCommand(attemptCtx, master1.IP, "kubectl get nodes")
		return err
	})
	if err != nil {
		return fmt.Errorf("k3s API server on master1 not ready: %w", err)
	}

	logging.Info("Extracting K3s token from Master 1...")
	token, err := p.readJoinToken(ctx, master1)
	if err != nil {
		return err
	}
	return p.distributeJoinToken(ctx, token)
}

func (p *Phase) readJoinToken(ctx context.Context, master1 config.NodeDetail) (string, error) {
	var token string
	err := retry.DoContext(ctx, retry.Config{MaxAttempts: 20, Interval: 3 * time.Second, Timeout: 60 * time.Second}, func(attemptCtx context.Context) error {
		tok, _, _, err := p.pool.RunCommand(attemptCtx, master1.IP, "cat /var/lib/rancher/k3s/server/node-token")
		if err != nil || strings.TrimSpace(tok) == "" {
			return fmt.Errorf("node-token not ready yet")
		}
		token = strings.TrimSpace(tok)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to get k3s token: %w", err)
	}
	return token, nil
}

// distributeJoinToken writes the token through a 0600 file instead of argv so
// it is not exposed in /proc/*/cmdline on the joining nodes.
func (p *Phase) distributeJoinToken(ctx context.Context, token string) error {
	logging.Info("Distributing K3s join token to joining nodes (file 0600, not argv)...")
	const joinTokenPath = "/root/.k3s-join-token"
	for _, joinIP := range p.joiningNodeIPs() {
		if err := p.pool.CopyContent(ctx, joinIP, []byte(token), joinTokenPath, 0600); err != nil {
			return fmt.Errorf("failed to upload k3s token to %s: %w", joinIP, err)
		}
	}
	return nil
}

func (p *Phase) joiningNodeIPs() []string {
	var ips []string
	for _, m := range p.cfg.Nodes.Masters[1:] {
		if m.IP != "" {
			ips = append(ips, m.IP)
		}
	}
	for _, w := range p.cfg.Nodes.Workers {
		if w.IP != "" {
			ips = append(ips, w.IP)
		}
	}
	return ips
}

// joinControlPlane joins every additional server so the embedded etcd cluster
// reaches quorum. The token travels through --token-file instead of the command
// line: argv of the remote shell would expose it to every local user via
// /proc/*/cmdline.
//
// The joins run concurrently: they are independent members of the same embedded
// etcd cluster (k3s adds them as learners and promotes them once caught up).
// Measured on the hosted run, joining the two extra servers in series cost
// 43.8s + 32.8s; the quorum check below is what proves the parallel join
// actually converged.
func (p *Phase) joinControlPlane(ctx context.Context, master1 config.NodeDetail) error {
	const joinTokenPath = "/root/.k3s-join-token"
	joiningServers := p.cfg.Nodes.Masters[1:]
	serverJoinTasks := make(map[string]func(context.Context, ssh.Executor) error, len(joiningServers))
	for _, m := range joiningServers {
		if m.IP == "" {
			continue
		}
		m := m
		serverJoinTasks[m.IP] = func(ctx context.Context, pool ssh.Executor) error {
			logging.Info("Installing K3s on %s (server join)...", m.Name)
			joinCmd := fmt.Sprintf("curl -sfL https://get.k3s.io | INSTALL_K3S_CHANNEL=\"v1.29\" INSTALL_K3S_EXEC=\"server --server https://%s:6443 --token-file %s --secrets-encryption --etcd-expose-metrics --node-external-ip %s --flannel-iface enp1s0\" sh -", master1.IP, joinTokenPath, m.IP)
			return runChecked(ctx, pool, m.IP, joinCmd, "join K3s "+m.Name)
		}
	}
	for _, r := range p.pool.RunParallelFunc(ctx, serverJoinTasks) {
		if r.Err != nil {
			return r.Err
		}
	}
	return nil
}

// joinWorkers installs the k3s agent on every worker in parallel. The token is
// expanded by bash into an exported env var (the assignment literal
// "$(cat ...)" stays unexpanded in argv), so the value is only visible in the
// owner-readable /proc/*/environ, never in cmdline.
func (p *Phase) joinWorkers(ctx context.Context, master1 config.NodeDetail) error {
	logging.Info("Installing K3s Agents on Workers...")
	const joinTokenPath = "/root/.k3s-join-token"
	agentJoinCmd := fmt.Sprintf("export K3S_TOKEN=\"$(cat %s)\" K3S_URL=https://%s:6443 INSTALL_K3S_CHANNEL=\"v1.29\" INSTALL_K3S_EXEC=\"--flannel-iface enp1s0\"; curl -sfL https://get.k3s.io | sh -", joinTokenPath, master1.IP)
	var workerIPs []string
	for _, w := range p.cfg.Nodes.Workers {
		if w.IP != "" {
			workerIPs = append(workerIPs, w.IP)
		}
	}

	for _, r := range p.pool.RunParallel(ctx, workerIPs, agentJoinCmd) {
		if r.Err != nil {
			return fmt.Errorf("failed to join k3s agent on %s: %w", r.Host, r.Err)
		}
		if r.ExitCode != 0 {
			return fmt.Errorf("failed to join k3s agent on %s (exit code %d): %s", r.Host, r.ExitCode, strings.TrimSpace(r.Output))
		}
	}
	return nil
}

func (p *Phase) waitForNodesReady(ctx context.Context, master1 config.NodeDetail) error {
	logging.Info("Waiting for all nodes to be Ready...")
	expectedCount := fmt.Sprintf("%d", len(p.cfg.Nodes.Masters)+len(p.cfg.Nodes.Workers))
	err := retry.DoContext(ctx, retry.Config{MaxAttempts: 100, Interval: 3 * time.Second, MaxInterval: 12 * time.Second, Timeout: 5 * time.Minute}, func(attemptCtx context.Context) error {
		out, _, _, err := p.pool.RunCommand(attemptCtx, master1.IP, "kubectl get nodes --no-headers | grep -v NotReady | grep Ready | wc -l")
		if err != nil {
			return err
		}
		count := strings.TrimSpace(out)
		if count != expectedCount {
			return fmt.Errorf("expected %s Ready nodes, got %s", expectedCount, count)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("K3s nodes did not all become Ready: %w", err)
	}
	return nil
}

// waitForEtcdQuorum verifies the member count, not just node readiness. K3s
// runs etcd inside the server process rather than as kube-system pods, so
// members are counted through the etcd role label (the one behind kubectl's
// ROLES column); that also matches how kubeadm clusters report it.
func (p *Phase) waitForEtcdQuorum(ctx context.Context, master1 config.NodeDetail) error {
	logging.Info("Verifying embedded etcd quorum (%d members required)...", len(p.cfg.Nodes.Masters))
	expectedEtcd := strconv.Itoa(len(p.cfg.Nodes.Masters))
	err := retry.DoContext(ctx, retry.Config{MaxAttempts: 100, Interval: 3 * time.Second, MaxInterval: 12 * time.Second, Timeout: 5 * time.Minute}, func(attemptCtx context.Context) error {
		// Count in Go rather than piping into `wc -l`: a kubectl failure there
		// still exits 0 (the status belongs to wc), so a broken kubeconfig or an
		// unavailable API server would be reported as "got 0 members" instead of
		// the real error - exactly the blind spot that made an earlier failure
		// look like a quorum problem.
		out, errOut, code, err := p.pool.RunCommand(attemptCtx, master1.IP, "kubectl get nodes -l node-role.kubernetes.io/etcd --no-headers")
		if err != nil || code != 0 {
			return fmt.Errorf("kubectl query failed (exit %d): %v %s", code, err, strings.TrimSpace(errOut))
		}
		count := countNonEmptyLines(out)
		if got := strconv.Itoa(count); got != expectedEtcd {
			return fmt.Errorf("expected %s etcd members, got %s", expectedEtcd, got)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("etcd did not reach quorum with %s members: %w", expectedEtcd, err)
	}
	logging.Success("etcd quorum healthy: %s members", expectedEtcd)
	return nil
}

// labelPrimary marks master1 as the DRBD primary. The label is informational:
// losing it must not abort a healthy cluster, so failures are warnings.
func (p *Phase) labelPrimary(ctx context.Context, master1 config.NodeDetail) {
	logging.Info("Labeling Master 1 with DRBD primary status...")
	_, _, _, err := p.pool.RunCommand(ctx, master1.IP, fmt.Sprintf("kubectl label node %s drbd-status=primary --overwrite 2>/dev/null || kubectl label node %s drbd-status=primary --overwrite", master1.FQDN, master1.Name))
	if err != nil {
		logging.Warn("Failed to label node: %v", err)
	}
}

// deployWorkloads uploads the kustomize tree and the generated credentials,
// applies the roots in dependency order, then waits for MariaDB and the
// initialization Job.
