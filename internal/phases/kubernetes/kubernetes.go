package kubernetes

import (
	"context"
	"encoding/base64"
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
	
	if len(p.cfg.Nodes.Masters) < 2 {
		return fmt.Errorf("k3s requires at least 2 master nodes in this setup")
	}

	master1 := p.cfg.Nodes.Masters[0]
	master2 := p.cfg.Nodes.Masters[1]
	
	logging.Info("Installing K3s on Master 1 (cluster-init)...")
	initCmd := fmt.Sprintf("curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC=\"server --cluster-init --node-external-ip %s --flannel-iface enp1s0\" sh -", master1.IP)
	if _, _, _, err := p.pool.RunCommand(ctx, master1.IP, initCmd); err != nil {
		return fmt.Errorf("failed to init k3s on master1: %w", err)
	}
	
	logging.Info("Waiting for API server on Master 1...")
	err := retry.Do(ctx, retry.Config{MaxAttempts: 30, Interval: 10 * time.Second, Timeout: 5 * time.Minute}, func() error {
		_, _, _, err := p.pool.RunCommand(ctx, master1.IP, "kubectl get nodes")
		return err
	})
	if err != nil {
		return fmt.Errorf("k3s API server on master1 not ready: %w", err)
	}
	
	logging.Info("Extracting K3s token from Master 1...")
	var token string
	err = retry.Do(ctx, retry.Config{MaxAttempts: 20, Interval: 3 * time.Second, Timeout: 60 * time.Second}, func() error {
		tok, err := p.pool.RunScript(ctx, master1.IP, "cat /var/lib/rancher/k3s/server/node-token")
		if err != nil || strings.TrimSpace(tok) == "" {
			return fmt.Errorf("node-token not ready yet")
		}
		token = strings.TrimSpace(tok)
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to get k3s token: %w", err)
	}
	
	logging.Info("Installing K3s on Master 2 (join)...")
	joinCmd := fmt.Sprintf("curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC=\"server --server https://%s:6443 --token %s --node-external-ip %s --flannel-iface enp1s0\" sh -", master1.IP, token, master2.IP)
	if _, _, _, err := p.pool.RunCommand(ctx, master2.IP, joinCmd); err != nil {
		return fmt.Errorf("failed to join k3s on master2: %w", err)
	}
	
	logging.Info("Installing K3s Agents on Workers...")
	agentJoinCmd := fmt.Sprintf("curl -sfL https://get.k3s.io | K3S_URL=https://%s:6443 K3S_TOKEN=%s INSTALL_K3S_EXEC=\"--flannel-iface enp1s0\" sh -", master1.IP, token)
	var workerIPs []string
	for _, w := range p.cfg.Nodes.Workers {
		workerIPs = append(workerIPs, w.IP)
	}
	
	res := p.pool.RunParallel(ctx, workerIPs, agentJoinCmd)
	for _, r := range res {
		if r.Err != nil {
			return fmt.Errorf("failed to join k3s agent on %s: %w", r.Host, r.Err)
		}
	}
	
	logging.Info("Waiting for all nodes to be Ready...")
	err = retry.Do(ctx, retry.Config{MaxAttempts: 30, Interval: 10 * time.Second, Timeout: 5 * time.Minute}, func() error {
		out, _ := p.pool.RunScript(ctx, master1.IP, "kubectl get nodes --no-headers | grep -v NotReady | grep Ready | wc -l")
		count := strings.TrimSpace(out)
		if count != "4" { // 2 masters + 2 workers
			return fmt.Errorf("expected 4 Ready nodes, got %s", count)
		}
		return nil
	})
	if err != nil {
		logging.Warn("Nodes might not be fully ready: %v", err)
	}
	
	logging.Info("Labeling Master 1 with DRBD primary status...")
	_, _, _, err = p.pool.RunCommand(ctx, master1.IP, fmt.Sprintf("kubectl label node %s drbd-status=primary --overwrite", master1.Name))
	if err != nil {
		logging.Warn("Failed to label node: %v", err)
	}
	
	logging.Info("Applying Kubernetes Manifests...")
	// We should extract manifests from embed and copy to master1
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
		"mariadb-service.yaml",
		"mariadb-statefulset.yaml",
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
		
		if err := p.pool.CopyContent(ctx, master1.IP, []byte(cfgContent), fmt.Sprintf("%s/%s", manifestsDir, mFile), 0644); err != nil {
			return fmt.Errorf("failed to copy manifest %s: %w", mFile, err)
		}
	}
	
	// Apply in order (all except init-db-job)
	for i := 0; i < len(manifestFiles)-1; i++ {
		mFile := manifestFiles[i]
		logging.Info("Applying %s...", mFile)
		err = retry.Do(ctx, retry.Config{MaxAttempts: 6, Interval: 10 * time.Second, Timeout: 60 * time.Second}, func() error {
			_, _, _, err := p.pool.RunCommand(ctx, master1.IP, fmt.Sprintf("kubectl apply -f %s/%s", manifestsDir, mFile))
			return err
		})
		if err != nil {
			return fmt.Errorf("failed to apply %s: %w", mFile, err)
		}
	}
	
	logging.Info("Waiting for MariaDB pod to be Running...")
	err = retry.Do(ctx, retry.Config{MaxAttempts: 30, Interval: 10 * time.Second, Timeout: 5 * time.Minute}, func() error {
		out, _ := p.pool.RunScript(ctx, master1.IP, "kubectl get pods -n cms -l app=mariadb -o jsonpath='{.items[0].status.phase}'")
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
	time.Sleep(10 * time.Second) // allow job to be created
	err = retry.Do(ctx, retry.Config{MaxAttempts: 30, Interval: 10 * time.Second, Timeout: 5 * time.Minute}, func() error {
		out, _ := p.pool.RunScript(ctx, master1.IP, "kubectl get job init-wordpress-db -n cms -o jsonpath='{.status.succeeded}'")
		if strings.TrimSpace(out) != "1" {
			return fmt.Errorf("job not completed")
		}
		return nil
	})
	
	logging.Info("Cleaning up temporary manifests...")
	p.pool.RunCommand(ctx, master1.IP, fmt.Sprintf("rm -rf %s", manifestsDir))
	
	logging.Success("Kubernetes Setup completed successfully.")
	return nil
}
