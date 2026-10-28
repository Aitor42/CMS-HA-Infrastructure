package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// The K3s binary is pinned to an exact release and verified before it is
// installed. The upstream install script still creates the systemd unit and
// runs the server/agent, but INSTALL_K3S_SKIP_DOWNLOAD=true forces it to use
// this already-verified binary instead of resolving a channel at runtime.
const (
	k3sVersion = "v1.29.9+k3s1"
	k3sSHA256  = "1d09adc906c16afbc69963b22070401f3c29a2c336368582f92b4330f0c89197"
)

// k3sInstallCommand downloads the pinned K3s binary, verifies its SHA-256 and
// installs it before invoking the upstream setup script. envPrefix is used for
// the worker's K3S_TOKEN_FILE/K3S_URL install environment; execArgs becomes
// INSTALL_K3S_EXEC. installEnv is placed after the pipe so it is an environment
// assignment for the upstream script, not an `export` line that ends up in the
// generated systemd environment file.
func k3sInstallCommand(execArgs, installEnv string, skipStart bool) string {
	url := "https://github.com/k3s-io/k3s/releases/download/" + strings.ReplaceAll(k3sVersion, "+", "%2B") + "/k3s"
	skip := ""
	if skipStart {
		// The upstream unit uses Type=notify with TimeoutStartSec=0. If the
		// agent cannot authenticate, `systemctl restart` waits forever for a
		// readiness notification. Skip the start here and start it with
		// --no-block below; waitForNodesReady bounds the failure instead.
		skip = "INSTALL_K3S_SKIP_START=true "
	}
	return fmt.Sprintf(`set -euo pipefail
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
curl -fsSL --connect-timeout 15 --max-time 180 --retry 3 --retry-delay 2 -o "$tmp" %q
printf '%%s  %%s\n' %q "$tmp" | sha256sum -c -
install -m 0755 "$tmp" /usr/local/bin/k3s
curl -fsSL --connect-timeout 15 --max-time 60 --retry 3 --retry-delay 2 https://get.k3s.io | %sINSTALL_K3S_SKIP_DOWNLOAD=true %sINSTALL_K3S_EXEC=%q sh -
`, url, k3sSHA256, installEnv, skip, execArgs)
}

// Phase implements the Kubernetes setup phase.
type Phase struct {
	cfg       *config.Config
	pool      ssh.Executor
	joinToken string
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
	if err := p.refreshJoinToken(ctx, master1); err != nil {
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
	if err := p.labelPrimary(ctx, master1); err != nil {
		return err
	}
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
	// Start the server with --no-block too: the upstream unit is Type=notify
	// with TimeoutStartSec=0, so a server that cannot start would otherwise
	// hang systemctl forever. The API wait below is the bounded readiness gate.
	initCmd := k3sInstallCommand(fmt.Sprintf("server --cluster-init --secrets-encryption --etcd-expose-metrics --node-external-ip %s --flannel-iface enp1s0", master1.IP), "", true) + "systemctl daemon-reload\nsystemctl start k3s --no-block\n"
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
	p.joinToken = token
	return p.distributeJoinToken(ctx, token)
}

// refreshJoinToken re-reads the node token after the control-plane servers have
// joined and redistributes it. Server joins can take long enough that a token
// read before they converge is stale on the workers; re-reading makes the
// worker join use the token the API server is actually validating.
func (p *Phase) refreshJoinToken(ctx context.Context, master1 config.NodeDetail) error {
	logging.Info("Refreshing K3s join token after control-plane join...")
	token, err := p.readJoinToken(ctx, master1)
	if err != nil {
		return err
	}
	if p.joinToken != "" && token != p.joinToken {
		logging.Warn("K3s join token changed after control-plane join (was %s, now %s); redistributing",
			tokenFingerprint(p.joinToken), tokenFingerprint(token))
	}
	p.joinToken = token
	return p.distributeJoinToken(ctx, token)
}

// tokenFingerprint returns a short, non-reversible identifier for logs. The
// token itself must never be logged.
func tokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:6])
}

func (p *Phase) readJoinToken(ctx context.Context, master1 config.NodeDetail) (string, error) {
	var token string
	err := retry.DoContext(ctx, retry.Config{MaxAttempts: 20, Interval: 3 * time.Second, Timeout: 60 * time.Second}, func(attemptCtx context.Context) error {
		tok, _, _, err := p.pool.RunCommand(attemptCtx, master1.IP, "cat /var/lib/rancher/k3s/server/node-token")
		if err != nil || strings.TrimSpace(tok) == "" {
			return fmt.Errorf("node-token not ready yet")
		}
		token = strings.TrimSpace(tok)
		if strings.ContainsAny(token, " \t\r\n") {
			return fmt.Errorf("node-token contains whitespace; refusing to distribute a corrupt token")
		}
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
			joinCmd := k3sInstallCommand(fmt.Sprintf("server --server https://%s:6443 --token-file %s --secrets-encryption --etcd-expose-metrics --node-external-ip %s --flannel-iface enp1s0", master1.IP, joinTokenPath, m.IP), "", true) + "systemctl daemon-reload\nsystemctl start k3s --no-block\n"
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

// joinWorkers installs the k3s agent on every worker in parallel. The agent
// reads the token from the protected 0600 file via K3S_TOKEN_FILE, so the
// secret never appears in argv or in the systemd environment file.
func (p *Phase) joinWorkers(ctx context.Context, master1 config.NodeDetail) error {
	logging.Info("Installing K3s Agents on Workers...")
	const joinTokenPath = "/root/.k3s-join-token"
	installEnv := fmt.Sprintf("K3S_TOKEN_FILE=%s K3S_URL=https://%s:6443 ", joinTokenPath, master1.IP)
	agentJoinCmd := k3sInstallCommand("--flannel-iface enp1s0", installEnv, true) + "systemctl daemon-reload\nsystemctl start k3s-agent --no-block\n"
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
	expected := p.expectedK3sNodes()
	err := retry.DoContext(ctx, retry.Config{MaxAttempts: 100, Interval: 3 * time.Second, MaxInterval: 12 * time.Second, Timeout: 5 * time.Minute}, func(attemptCtx context.Context) error {
		// Count in Go and check kubectl's exit status. Piping the listing into
		// grep|wc would turn an API failure into "got 0 Ready nodes" and hide
		// the real cause for the full retry budget.
		out, errOut, code, err := p.pool.RunCommand(attemptCtx, master1.IP, "kubectl get nodes --no-headers")
		if err != nil || code != 0 {
			return fmt.Errorf("kubectl get nodes failed (exit %d): %v %s", code, err, strings.TrimSpace(errOut))
		}
		ready := readyNodeNames(out)
		missing := make([]string, 0, len(expected))
		for _, node := range expected {
			if !ready[node.Name] && !ready[node.FQDN] {
				missing = append(missing, node.Name)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("nodes not Ready: %s", strings.Join(missing, ", "))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("K3s nodes did not all become Ready: %w", err)
	}
	return nil
}

// expectedK3sNodes returns the node identities the phase must see Ready. K3s
// uses the VM hostname as the node name, but a node registered with its FQDN is
// equally valid, so readiness accepts either form.
func (p *Phase) expectedK3sNodes() []config.NodeDetail {
	nodes := make([]config.NodeDetail, 0, len(p.cfg.Nodes.Masters)+len(p.cfg.Nodes.Workers))
	nodes = append(nodes, p.cfg.Nodes.Masters...)
	nodes = append(nodes, p.cfg.Nodes.Workers...)
	return nodes
}

// readyNodeNames parses `kubectl get nodes --no-headers` and returns the names
// whose STATUS column starts with Ready. Extra/stale node objects are ignored,
// which makes a resumed deploy tolerant of a node object left behind by an
// earlier failed join.
func readyNodeNames(out string) map[string]bool {
	ready := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		// kubectl renders a cordoned Ready node as Ready,SchedulingDisabled;
		// the Ready condition is still true, so accept the prefix.
		if len(fields) < 2 || !strings.HasPrefix(fields[1], "Ready") {
			continue
		}
		ready[fields[0]] = true
	}
	return ready
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

// labelPrimary marks the node that currently owns the DRBD Primary role. This
// is not informational: the MariaDB StatefulSet and the backup CronJobs use the
// label as a node affinity, so a missing label only surfaces later as a Pending
// pod and a misleading "mariadb pod was never created" timeout. Resolving the
// role instead of assuming master1 also keeps a resumed deploy correct after a
// failover moved the primary to master2.
func (p *Phase) labelPrimary(ctx context.Context, master1 config.NodeDetail) error {
	primary, err := p.findDRBDPrimary(ctx)
	if err != nil {
		return err
	}
	logging.Info("Labeling %s with DRBD primary status...", primary.Name)

	// A node object can retain the label after its DRBD role changed. Clear it
	// from the other masters before setting the current owner, otherwise the
	// MariaDB node affinity matches both a secondary and the primary.
	for _, m := range p.cfg.Nodes.Masters {
		if m.IP == "" || m.IP == primary.IP {
			continue
		}
		cmd := fmt.Sprintf("kubectl label node %s drbd-status- 2>/dev/null || kubectl label node %s drbd-status- 2>/dev/null || true", m.FQDN, m.Name)
		_, _, _, _ = p.pool.RunCommand(ctx, master1.IP, cmd)
	}

	cmd := fmt.Sprintf("kubectl label node %s drbd-status=primary --overwrite 2>/dev/null || kubectl label node %s drbd-status=primary --overwrite", primary.FQDN, primary.Name)
	if err := runCheckedRetry(ctx, p.pool, master1.IP, cmd, "label the DRBD primary node"); err != nil {
		return fmt.Errorf("could not label %s as drbd-status=primary (MariaDB and backup scheduling depend on it): %w", primary.Name, err)
	}
	return nil
}

// findDRBDPrimary asks every DRBD-enabled master for the role of cms_data and
// requires exactly one Primary. Configurations without drbd_disk_gb keep the
// historical master1 behaviour.
func (p *Phase) findDRBDPrimary(ctx context.Context) (config.NodeDetail, error) {
	var candidates []config.NodeDetail
	for _, m := range p.cfg.Nodes.Masters {
		if m.IP != "" && m.DRBDDiskGB > 0 {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 0 {
		if len(p.cfg.Nodes.Masters) == 0 {
			return config.NodeDetail{}, fmt.Errorf("no master nodes configured")
		}
		return p.cfg.Nodes.Masters[0], nil
	}

	var primaries []config.NodeDetail
	for _, m := range candidates {
		out, errOut, code, err := p.pool.RunCommand(ctx, m.IP, "drbdadm role cms_data")
		if err != nil || code != 0 {
			return config.NodeDetail{}, fmt.Errorf("read DRBD role on %s (exit %d): %v %s", m.Name, code, err, strings.TrimSpace(errOut))
		}
		if strings.HasPrefix(strings.TrimSpace(out), "Primary") {
			primaries = append(primaries, m)
		}
	}
	switch len(primaries) {
	case 1:
		return primaries[0], nil
	case 0:
		return config.NodeDetail{}, fmt.Errorf("no DRBD-enabled master reports Primary for cms_data")
	default:
		names := make([]string, 0, len(primaries))
		for _, m := range primaries {
			names = append(names, m.Name)
		}
		return config.NodeDetail{}, fmt.Errorf("multiple DRBD-enabled masters report Primary for cms_data: %s", strings.Join(names, ", "))
	}
}

// deployWorkloads uploads the kustomize tree and the generated credentials,
// applies the roots in dependency order, then waits for MariaDB and the
// initialization Job.
