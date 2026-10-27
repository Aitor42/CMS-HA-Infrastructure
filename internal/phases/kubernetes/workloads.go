package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
)

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
