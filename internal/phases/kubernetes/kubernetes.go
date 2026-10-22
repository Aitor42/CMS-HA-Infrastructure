package kubernetes

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
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
func (p *Phase) Description() string {
	return "Sets up Kubernetes cluster (K3s) and deploys MariaDB"
}

// Name returns the phase name.
func (p *Phase) Name() string {
	return "Kubernetes Setup"
}

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
func (p *Phase) deployWorkloads(ctx context.Context, master1 config.NodeDetail) error {
	manifestsDir, cleanup, err := p.prepareManifests(ctx, master1)
	if err != nil {
		return err
	}
	defer cleanup()

	backupsOn, secretPath, backupSecretPath, backupConfigPath, err := p.uploadGeneratedManifests(ctx, master1, manifestsDir)
	if err != nil {
		return err
	}
	if err := p.applyGeneratedManifests(ctx, master1, manifestsDir, secretPath, backupSecretPath, backupConfigPath, backupsOn); err != nil {
		return err
	}
	if err := p.applyKustomizeRoots(ctx, master1, manifestsDir, backupsOn); err != nil {
		return err
	}
	if backupsOn {
		p.syncBackupSchedules(ctx, master1)
	}
	if err := p.waitForMariaDB(ctx, master1); err != nil {
		return err
	}
	return p.applyInitJob(ctx, master1, manifestsDir)
}

// prepareManifests extracts the kustomize tree (base, backups, jobs,
// monitoring and the overlays) to master1, preserving the layout so relative
// references (e.g. ../../base) resolve.
func (p *Phase) prepareManifests(ctx context.Context, master1 config.NodeDetail) (string, func(), error) {
	logging.Info("Applying Kubernetes Manifests (kustomize)...")
	manifestsDir := "/tmp/k3s-manifests"
	if err := runChecked(ctx, p.pool, master1.IP, fmt.Sprintf("install -d -m 0700 %s", manifestsDir), "create private manifest directory"); err != nil {
		return "", nil, err
	}
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _, _, _ = p.pool.RunCommand(cleanupCtx, master1.IP, fmt.Sprintf("rm -rf %s", manifestsDir))
	}

	if err := copyKustomizeTree(ctx, p.pool, master1.IP, manifestsDir); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("failed to copy the kustomize tree: %w", err)
	}

	generatedDir := fmt.Sprintf("%s/generated", manifestsDir)
	if err := runChecked(ctx, p.pool, master1.IP, fmt.Sprintf("install -d -m 0700 %s", generatedDir), "create generated manifest directory"); err != nil {
		cleanup()
		return "", nil, err
	}
	return manifestsDir, cleanup, nil
}

// uploadGeneratedManifests generates the MariaDB and backup credentials from
// config.yaml and uploads them. All files are generated before the first apply,
// so a malformed value fails before any object is created.
func (p *Phase) uploadGeneratedManifests(ctx context.Context, master1 config.NodeDetail, manifestsDir string) (bool, string, string, string, error) {
	generatedDir := fmt.Sprintf("%s/generated", manifestsDir)
	secretManifest, err := makeMariaDBSecret(p.cfg.Database.RootPassword, p.cfg.Database.Password, p.cfg.Database.ExporterPassword)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to generate MariaDB Secret manifest: %w", err)
	}
	secretPath := fmt.Sprintf("%s/mariadb-secret.yaml", generatedDir)
	if err := p.pool.CopyContent(ctx, master1.IP, secretManifest, secretPath, 0600); err != nil {
		return false, "", "", "", fmt.Errorf("failed to copy generated MariaDB Secret: %w", err)
	}

	// Backups live off the DRBD device (Garage S3 on the hypervisor). The jobs
	// read the endpoint/bucket from a ConfigMap and the credentials from a
	// Secret, both generated here so config.yaml stays the single source of
	// truth. When backup.engine is empty the whole feature is skipped.
	backupsOn := p.cfg.BackupEnabled()
	backupSecretPath := ""
	backupConfigPath := ""
	if backupsOn {
		backupSecret, err := makeBackupSecret(p.cfg.BackupAccessKey(), p.cfg.Backup.SecretKey)
		if err != nil {
			return false, "", "", "", fmt.Errorf("failed to generate backup Secret manifest: %w", err)
		}
		backupSecretPath = fmt.Sprintf("%s/backup-secret.yaml", generatedDir)
		if err := p.pool.CopyContent(ctx, master1.IP, backupSecret, backupSecretPath, 0600); err != nil {
			return false, "", "", "", fmt.Errorf("failed to copy generated backup Secret: %w", err)
		}

		backupConfig, err := makeBackupConfigMap(p.cfg.BackupS3Endpoint(), p.cfg.BackupBucket(), p.cfg.BackupS3Region(), p.cfg.BackupRetentionDays())
		if err != nil {
			return false, "", "", "", fmt.Errorf("failed to generate backup ConfigMap manifest: %w", err)
		}
		backupConfigPath = fmt.Sprintf("%s/backup-config.yaml", generatedDir)
		if err := p.pool.CopyContent(ctx, master1.IP, backupConfig, backupConfigPath, 0644); err != nil {
			return false, "", "", "", fmt.Errorf("failed to copy generated backup ConfigMap: %w", err)
		}
	}
	return backupsOn, secretPath, backupSecretPath, backupConfigPath, nil
}

// applyGeneratedManifests creates the namespace and credentials before any
// workload can start against a missing Secret.
func (p *Phase) applyGeneratedManifests(ctx context.Context, master1 config.NodeDetail, manifestsDir, secretPath, backupSecretPath, backupConfigPath string, backupsOn bool) error {
	if err := runCheckedRetry(ctx, p.pool, master1.IP, fmt.Sprintf("kubectl apply -f %s/base/namespace.yaml", manifestsDir), "apply Kubernetes namespace"); err != nil {
		return err
	}
	if err := runCheckedRetry(ctx, p.pool, master1.IP, fmt.Sprintf("kubectl apply -f %s", secretPath), "apply MariaDB Secret"); err != nil {
		return err
	}
	_, _, _, _ = p.pool.RunCommand(ctx, master1.IP, fmt.Sprintf("rm -f %s", secretPath))

	if backupsOn {
		if err := runCheckedRetry(ctx, p.pool, master1.IP, fmt.Sprintf("kubectl apply -f %s", backupConfigPath), "apply backup ConfigMap"); err != nil {
			return err
		}
		if err := runCheckedRetry(ctx, p.pool, master1.IP, fmt.Sprintf("kubectl apply -f %s", backupSecretPath), "apply backup Secret"); err != nil {
			return err
		}
		_, _, _, _ = p.pool.RunCommand(ctx, master1.IP, fmt.Sprintf("rm -f %s", backupSecretPath))
	}
	return nil
}

func (p *Phase) applyKustomizeRoots(ctx context.Context, master1 config.NodeDetail, manifestsDir string, backupsOn bool) error {
	// Core platform via the lab kustomize overlay (base + overlays).
	logging.Info("Applying base manifests with kustomize (kubectl apply -k overlays/lab)...")
	err := retry.DoContext(ctx, retry.Config{MaxAttempts: 6, Interval: 5 * time.Second, Timeout: 60 * time.Second}, func(attemptCtx context.Context) error {
		return runChecked(attemptCtx, p.pool, master1.IP, fmt.Sprintf("kubectl apply -k %s/overlays/lab", manifestsDir), "apply base kustomize overlay")
	})
	if err != nil {
		return fmt.Errorf("failed to apply base kustomize overlay: %w", err)
	}

	// Least-privilege scrape RBAC for Prometheus (ServiceAccount + read-only
	// ClusterRole/binding). It lives in its own kustomize root because it
	// targets kube-system, which the other roots rewrite via `namespace:`.
	if err := runCheckedRetry(ctx, p.pool, master1.IP, fmt.Sprintf("kubectl apply -k %s/monitoring", manifestsDir), "apply Prometheus scrape RBAC"); err != nil {
		return err
	}

	// Off-DRBD backup stack (only when configured).
	if backupsOn {
		if err := runCheckedRetry(ctx, p.pool, master1.IP, fmt.Sprintf("kubectl apply -k %s/backups", manifestsDir), "apply backup kustomize root"); err != nil {
			return err
		}
	}
	return nil
}

// syncBackupSchedules keeps the CronJob schedules in sync with config.yaml;
// the schedules baked into the manifests are only fallbacks. Schedules are
// validated in config.Validate() to be shell-safe.
func (p *Phase) syncBackupSchedules(ctx context.Context, master1 config.NodeDetail) {
	schedules := map[string]string{
		"mariadb-backup":       p.cfg.BackupSchedule(),
		"mariadb-restore-test": p.cfg.BackupRestoreTestSchedule(),
	}
	for name, sched := range schedules {
		patch := fmt.Sprintf(`{"spec":{"schedule":"%s"}}`, sched)
		cmd := fmt.Sprintf("kubectl patch cronjob %s -n cms --type merge -p '%s'", name, patch)
		if out, errOut, code, err := p.pool.RunCommand(ctx, master1.IP, cmd); err != nil || code != 0 {
			logging.Warn("Could not sync the schedule of CronJob %s (exit %d): %v %s%s",
				name, code, err, strings.TrimSpace(out), strings.TrimSpace(errOut))
		}
	}
}

func (p *Phase) waitForMariaDB(ctx context.Context, master1 config.NodeDetail) error {
	logging.Info("Waiting for MariaDB pod to be Running...")
	// The StatefulSet controller creates mariadb-0 asynchronously and
	// `kubectl wait` fails outright when no pod matches yet, so wait for the
	// pod object to appear before waiting on its phase.
	err := retry.DoContext(ctx, retry.Config{MaxAttempts: 40, Interval: 3 * time.Second, MaxInterval: 10 * time.Second, Timeout: 2 * time.Minute}, func(attemptCtx context.Context) error {
		return runChecked(attemptCtx, p.pool, master1.IP, "kubectl get pod mariadb-0 -n cms", "wait for the MariaDB pod to be created")
	})
	if err != nil {
		return fmt.Errorf("mariadb pod was never created: %w", err)
	}
	// kubectl wait returns as soon as the pod reaches the Running phase.
	if err := runCheckedRetry(ctx, p.pool, master1.IP, "kubectl wait --for=jsonpath='{.status.phase}'=Running pod mariadb-0 -n cms --timeout=300s", "wait for MariaDB pod"); err != nil {
		return fmt.Errorf("mariadb pod failed to start: %w", err)
	}
	return nil
}

func (p *Phase) applyInitJob(ctx context.Context, master1 config.NodeDetail, manifestsDir string) error {
	logging.Info("Applying init-db-job...")
	// The Job name is fixed, so a resumed deploy has to clear the previous
	// object first. kubectl delete already waits for it to disappear, but the
	// failure was discarded: a delete that did not happen turned into a
	// confusing "object is being deleted" error on the apply instead.
	if err := runCheckedRetry(ctx, p.pool, master1.IP, "kubectl delete job init-wordpress-db -n cms --ignore-not-found=true", "clear previous MariaDB initialization Job"); err != nil {
		return err
	}
	if err := runCheckedRetry(ctx, p.pool, master1.IP, fmt.Sprintf("kubectl apply -k %s/jobs", manifestsDir), "apply MariaDB initialization Job"); err != nil {
		return err
	}

	logging.Info("Waiting for init-db-job completion...")
	// The Job exists as soon as it is applied; wait for its complete condition.
	if err := runCheckedRetry(ctx, p.pool, master1.IP, "kubectl wait --for=condition=complete job/init-wordpress-db -n cms --timeout=300s", "wait for MariaDB initialization Job"); err != nil {
		return fmt.Errorf("init-wordpress-db job did not complete: %w", err)
	}
	return nil
}

// copyKustomizeTree copies the embedded kubernetes/ kustomize tree to the remote
// host, preserving the directory layout so relative resource references
// (e.g. ../../base from an overlay) keep working. The whole tree is packed into
// a single tar and extracted in one SSH session (directories 0700, files 0644)
// instead of one round-trip per directory and per file.
func copyKustomizeTree(ctx context.Context, pool ssh.Executor, host, dest string) error {
	tarball, err := buildKustomizeTar()
	if err != nil {
		return err
	}
	if err := pool.CopyTar(ctx, host, dest, tarball); err != nil {
		return fmt.Errorf("extract kustomize tree: %w", err)
	}
	return nil
}

// buildKustomizeTar packs the embedded kubernetes/ tree into a tar archive with
// deterministic modes (directories 0700, regular files 0644).
func buildKustomizeTar() ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	err := fs.WalkDir(cms.KubernetesFS, "kubernetes", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, "kubernetes/")
		if rel == "" || rel == "kubernetes" {
			return nil
		}
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Name: rel + "/", Mode: 0700, Typeflag: tar.TypeDir})
		}
		content, err := cms.KubernetesFS.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read embedded manifest %s: %w", p, err)
		}
		hdr := &tar.Header{Name: rel, Mode: 0644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = tw.Write(content)
		return err
	})
	if err != nil {
		_ = tw.Close()
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// countNonEmptyLines counts the entries of a `kubectl get ... --no-headers`
// listing. It lives outside the retry closure so the counting rule (blank lines
// are not members) is unit tested.
func countNonEmptyLines(out string) int {
	count := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

func runChecked(ctx context.Context, pool ssh.Executor, host, command, action string) error {
	stdout, stderr, code, err := pool.RunCommand(ctx, host, command)
	// A non-zero exit status arrives as an error as well, but it carries the
	// command's own output, which is what explains the failure. Only a
	// transport-level failure (code < 0) has nothing to add.
	if err != nil && code < 0 {
		return fmt.Errorf("%s: %w", action, err)
	}
	if code != 0 {
		// kubectl writes several classes of failures to stdout rather than
		// stderr, so keep both streams in the message.
		detail := strings.TrimSpace(stderr)
		if out := strings.TrimSpace(stdout); out != "" {
			if detail != "" {
				detail += " | "
			}
			detail += out
		}
		if detail == "" && err != nil {
			detail = err.Error()
		}
		return fmt.Errorf("%s failed (exit %d): %s", action, code, detail)
	}
	return nil
}

// transientAPIErrors are the failures a healthy but busy control plane
// produces while k3s settles: a restarted service, an etcd election, or the
// API server dropping connections under load. They say nothing about the
// manifest being applied, so they are worth retrying instead of failing a
// deployment that is otherwise fine.
var transientAPIErrors = []string{
	"connection refused",
	"connection reset by peer",
	"no route to host",
	"i/o timeout",
	"TLS handshake timeout",
	"unexpected EOF",
	"Unable to connect to the server",
	"The connection to the server",
	"the server is currently unable to handle the request",
	"ServiceUnavailable",
	"etcdserver: request timed out",
	"etcdserver: leader changed",
	"etcdserver: no leader",
}

// isTransientAPIError reports whether a kubectl failure is the control plane
// being briefly unavailable rather than the command being wrong.
func isTransientAPIError(detail string) bool {
	for _, marker := range transientAPIErrors {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return false
}

// runCheckedRetry is runChecked for idempotent kubectl commands (apply, get,
// delete, wait): a control-plane hiccup is retried for up to apiRetryBudget,
// while a real rejection (a bad manifest, a missing object) fails at once.
func runCheckedRetry(ctx context.Context, pool ssh.Executor, host, command, action string) error {
	deadline := time.Now().Add(apiRetryBudget)
	for attempt := 1; ; attempt++ {
		err := runChecked(ctx, pool, host, command, action)
		if err == nil {
			return nil
		}
		if !isTransientAPIError(err.Error()) || time.Now().After(deadline) {
			return err
		}
		logging.Warn("%s: the control plane is not answering yet (attempt %d); retrying", action, attempt)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// makeMariaDBSecret builds the Kubernetes Secret manifest from runtime config.
// JSON is valid YAML, and encoding the values here avoids storing credentials
// in the embedded Kubernetes manifests.
func makeMariaDBSecret(rootPassword, wordpressPassword, exporterPassword string) ([]byte, error) {
	secret := struct {
		APIVersion string            `json:"apiVersion"`
		Kind       string            `json:"kind"`
		Metadata   map[string]string `json:"metadata"`
		Type       string            `json:"type"`
		Data       map[string]string `json:"data"`
	}{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: map[string]string{
			"name":      "mariadb-secret",
			"namespace": "cms",
		},
		Type: "Opaque",
		Data: map[string]string{
			"MYSQL_ROOT_PASSWORD":     base64.StdEncoding.EncodeToString([]byte(rootPassword)),
			"MYSQL_WP_PASSWORD":       base64.StdEncoding.EncodeToString([]byte(wordpressPassword)),
			"MYSQL_EXPORTER_PASSWORD": base64.StdEncoding.EncodeToString([]byte(exporterPassword)),
		},
	}
	content, err := json.Marshal(secret)
	if err != nil {
		return nil, fmt.Errorf("marshal Secret: %w", err)
	}
	return content, nil
}

// makeBackupSecret renders the S3 credentials Secret consumed by the backup and
// restore-drill CronJobs.
func makeBackupSecret(accessKey, secretKey string) ([]byte, error) {
	secret := struct {
		APIVersion string            `json:"apiVersion"`
		Kind       string            `json:"kind"`
		Metadata   map[string]string `json:"metadata"`
		Type       string            `json:"type"`
		Data       map[string]string `json:"data"`
	}{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: map[string]string{
			"name":      "backup-secret",
			"namespace": "cms",
		},
		Type: "Opaque",
		Data: map[string]string{
			"S3_ACCESS_KEY": base64.StdEncoding.EncodeToString([]byte(accessKey)),
			"S3_SECRET_KEY": base64.StdEncoding.EncodeToString([]byte(secretKey)),
		},
	}
	content, err := json.Marshal(secret)
	if err != nil {
		return nil, fmt.Errorf("marshal backup Secret: %w", err)
	}
	return content, nil
}

// makeBackupConfigMap renders the non-secret backup settings (endpoint, bucket,
// region, retention) consumed by the backup and restore-drill CronJobs.
func makeBackupConfigMap(endpoint, bucket, region string, retentionDays int) ([]byte, error) {
	configMap := struct {
		APIVersion string            `json:"apiVersion"`
		Kind       string            `json:"kind"`
		Metadata   map[string]string `json:"metadata"`
		Data       map[string]string `json:"data"`
	}{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Metadata: map[string]string{
			"name":      "backup-config",
			"namespace": "cms",
		},
		Data: map[string]string{
			"S3_ENDPOINT":    endpoint,
			"S3_BUCKET":      bucket,
			"S3_REGION":      region,
			"RETENTION_DAYS": strconv.Itoa(retentionDays),
		},
	}
	content, err := json.Marshal(configMap)
	if err != nil {
		return nil, fmt.Errorf("marshal backup ConfigMap: %w", err)
	}
	return content, nil
}
