package kubernetes

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// Phase implements the Kubernetes setup phase
type Phase struct {
	cfg  *config.Config
	pool *ssh.Pool
}

// NewPhase creates a new Kubernetes Phase
func NewPhase(cfg *config.Config, pool *ssh.Pool) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

// Description returns the phase description
func (p *Phase) Description() string {
	return "Sets up Kubernetes cluster (K3s) and deploys MariaDB"
}

// Name returns the phase name
func (p *Phase) Name() string {
	return "Kubernetes Setup"
}

// Run runs the Kubernetes setup
func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()

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

	master1 := p.cfg.Nodes.Masters[0]
	joiningServers := p.cfg.Nodes.Masters[1:]

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
	var token string
	err = retry.DoContext(ctx, retry.Config{MaxAttempts: 20, Interval: 3 * time.Second, Timeout: 60 * time.Second}, func(attemptCtx context.Context) error {
		tok, _, _, err := p.pool.RunCommand(attemptCtx, master1.IP, "cat /var/lib/rancher/k3s/server/node-token")
		if err != nil || strings.TrimSpace(tok) == "" {
			return fmt.Errorf("node-token not ready yet")
		}
		token = strings.TrimSpace(tok)
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to get k3s token: %w", err)
	}

	logging.Info("Distributing K3s join token to joining nodes (file 0600, not argv)...")
	joinTokenPath := "/root/.k3s-join-token"
	var joiningNodeIPs []string
	for _, m := range joiningServers {
		if m.IP != "" {
			joiningNodeIPs = append(joiningNodeIPs, m.IP)
		}
	}
	for _, w := range p.cfg.Nodes.Workers {
		if w.IP != "" {
			joiningNodeIPs = append(joiningNodeIPs, w.IP)
		}
	}
	for _, joinIP := range joiningNodeIPs {
		if err := p.pool.CopyContent(ctx, joinIP, []byte(token), joinTokenPath, 0600); err != nil {
			return fmt.Errorf("failed to upload k3s token to %s: %w", joinIP, err)
		}
	}

	// Join every additional control-plane server so the embedded etcd cluster
	// reaches quorum. The token travels through --token-file instead of the
	// command line: argv of the remote shell would expose it to every local
	// user via /proc/*/cmdline.
	for _, m := range joiningServers {
		logging.Info("Installing K3s on %s (server join)...", m.Name)
		joinCmd := fmt.Sprintf("curl -sfL https://get.k3s.io | INSTALL_K3S_CHANNEL=\"v1.29\" INSTALL_K3S_EXEC=\"server --server https://%s:6443 --token-file %s --secrets-encryption --etcd-expose-metrics --node-external-ip %s --flannel-iface enp1s0\" sh -", master1.IP, joinTokenPath, m.IP)
		if err := runChecked(ctx, p.pool, m.IP, joinCmd, "join K3s "+m.Name); err != nil {
			return err
		}
	}

	logging.Info("Installing K3s Agents on Workers...")
	// The token is expanded by bash into an exported env var (the assignment
	// literal "$(cat ...)" stays unexpanded in argv), so the value is only
	// visible in the owner-readable /proc/*/environ, never in cmdline.
	agentJoinCmd := fmt.Sprintf("export K3S_TOKEN=\"$(cat %s)\" K3S_URL=https://%s:6443 INSTALL_K3S_CHANNEL=\"v1.29\" INSTALL_K3S_EXEC=\"--flannel-iface enp1s0\"; curl -sfL https://get.k3s.io | sh -", joinTokenPath, master1.IP)
	var workerIPs []string
	for _, w := range p.cfg.Nodes.Workers {
		workerIPs = append(workerIPs, w.IP)
	}

	for _, r := range p.pool.RunParallel(ctx, workerIPs, agentJoinCmd) {
		if r.Err != nil {
			return fmt.Errorf("failed to join k3s agent on %s: %w", r.Host, r.Err)
		}
		if r.ExitCode != 0 {
			return fmt.Errorf("failed to join k3s agent on %s (exit code %d): %s", r.Host, r.ExitCode, strings.TrimSpace(r.Output))
		}
	}

	logging.Info("Waiting for all nodes to be Ready...")
	expectedCount := fmt.Sprintf("%d", len(p.cfg.Nodes.Masters)+len(p.cfg.Nodes.Workers))
	err = retry.DoContext(ctx, retry.Config{MaxAttempts: 100, Interval: 3 * time.Second, MaxInterval: 12 * time.Second, Timeout: 5 * time.Minute}, func(attemptCtx context.Context) error {
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
		logging.Warn("Nodes might not be fully ready: %v", err)
	}
	
	logging.Info("Labeling Master 1 with DRBD primary status...")
	_, _, _, err = p.pool.RunCommand(ctx, master1.IP, fmt.Sprintf("kubectl label node %s drbd-status=primary --overwrite 2>/dev/null || kubectl label node %s drbd-status=primary --overwrite", master1.FQDN, master1.Name))
	if err != nil {
		logging.Warn("Failed to label node: %v", err)
	}
	
	logging.Info("Applying Kubernetes Manifests...")
	// Extract manifests from embed and copy to master1
	manifestsDir := "/tmp/k3s-manifests"
	_, _, _, err = p.pool.RunCommand(ctx, master1.IP, fmt.Sprintf("mkdir -p %s", manifestsDir))
	if err != nil {
		return fmt.Errorf("failed to create manifests dir: %w", err)
	}

	manifestFiles := []string{
		"namespace.yaml",
		"mariadb-secret.yaml",
		"mariadb-pv.yaml",
		"mariadb-pvc.yaml",
		"mariadb-backup-pv.yaml",
		"mariadb-service.yaml",
		"mariadb-statefulset.yaml",
		"mariadb-backup-cronjob.yaml",
		"init-db-job.yaml",
	}

	for _, mFile := range manifestFiles {
		content, err := cms.KubernetesFS.ReadFile("kubernetes/" + mFile)
		if err != nil {
			return fmt.Errorf("failed to read manifest %s: %w", mFile, err)
		}

		cfgContent := string(content)
		if mFile == "mariadb-secret.yaml" {
			if p.cfg.Database.Password != "" {
				b64Pass := base64.StdEncoding.EncodeToString([]byte(p.cfg.Database.Password))
				cfgContent = strings.ReplaceAll(cfgContent, "V3BTM2N1cjNQNHNzIQ==", b64Pass)
			}
			if p.cfg.Database.RootPassword != "" {
				b64Root := base64.StdEncoding.EncodeToString([]byte(p.cfg.Database.RootPassword))
				cfgContent = strings.ReplaceAll(cfgContent, "bXlzcWxyb290cGFzcw==", b64Root)
			}
		}
		
		perm := os.FileMode(0644)
		if mFile == "mariadb-secret.yaml" {
			perm = 0600
		}
		if err := p.pool.CopyContent(ctx, master1.IP, []byte(cfgContent), fmt.Sprintf("%s/%s", manifestsDir, mFile), perm); err != nil {
			return fmt.Errorf("failed to copy manifest %s: %w", mFile, err)
		}
	}
	
	// Apply base manifests (all except init-db-job) in single command
	var applyArgs []string
	for i := 0; i < len(manifestFiles)-1; i++ {
		applyArgs = append(applyArgs, fmt.Sprintf("-f %s/%s", manifestsDir, manifestFiles[i]))
	}
	applyCmd := "kubectl apply " + strings.Join(applyArgs, " ")

	logging.Info("Applying base Kubernetes manifests in batch...")
	err = retry.Do(ctx, retry.Config{MaxAttempts: 6, Interval: 5 * time.Second, Timeout: 60 * time.Second}, func() error {
		_, _, _, err := p.pool.RunCommand(ctx, master1.IP, applyCmd)
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to apply base Kubernetes manifests: %w", err)
	}
	
	logging.Info("Waiting for MariaDB pod to be Running...")
	err = retry.Do(ctx, retry.Config{MaxAttempts: 30, Interval: 10 * time.Second, Timeout: 5 * time.Minute}, func() error {
		out, _, _, _ := p.pool.RunCommand(ctx, master1.IP, "kubectl get pods -n cms -l app=mariadb -o jsonpath='{.items[0].status.phase}'")
		if strings.TrimSpace(out) != "Running" {
			return fmt.Errorf("mariadb not running: %s", out)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("mariadb pod failed to start: %w", err)
	}
	
	logging.Info("Applying init-db-job...")
	p.pool.RunCommand(ctx, master1.IP, "kubectl delete job init-wordpress-db -n cms --ignore-not-found=true")
	_, _, _, err = p.pool.RunCommand(ctx, master1.IP, fmt.Sprintf("kubectl apply -f %s/%s", manifestsDir, "init-db-job.yaml"))
	if err != nil {
		return fmt.Errorf("failed to apply init-db-job: %w", err)
	}
	
	logging.Info("Waiting for init-db-job completion...")
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
	}
	err = retry.Do(ctx, retry.Config{MaxAttempts: 30, Interval: 10 * time.Second, Timeout: 5 * time.Minute}, func() error {
		out, _, _, _ := p.pool.RunCommand(ctx, master1.IP, "kubectl get job init-wordpress-db -n cms -o jsonpath='{.status.succeeded}'")
		if strings.TrimSpace(out) != "1" {
			return fmt.Errorf("job not completed")
		}
		return nil
	})
	if err != nil {
		p.pool.RunCommand(ctx, master1.IP, fmt.Sprintf("rm -rf %s", manifestsDir))
		return fmt.Errorf("init-wordpress-db job did not complete: %w", err)
	}
	
	logging.Info("Cleaning up temporary manifests...")
	p.pool.RunCommand(ctx, master1.IP, fmt.Sprintf("rm -rf %s", manifestsDir))
	
	logging.Success("Kubernetes Setup completed successfully.")
	return nil
}
