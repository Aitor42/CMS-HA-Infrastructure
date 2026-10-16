package root

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/spf13/cobra"
)

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Backup utilities",
}

func init() {
	rootCmd.AddCommand(backupCmd)

	dbCmd := &cobra.Command{
		Use: "db",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			pool, err := newSSHPool(cfg)
			if err != nil {
				return err
			}
			defer pool.Close()
			ctx := cmd.Context()
			if len(cfg.Nodes.Masters) == 0 {
				return fmt.Errorf("no master nodes defined in configuration")
			}
			var masterIP string
			for _, m := range cfg.Nodes.Masters {
				if m.IP != "" {
					// Bounded per-probe budget so one hung kubectl/SSH master
					// does not stall the backup indefinitely.
					probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
					_, _, code, err := pool.RunCommand(probeCtx, m.IP, "kubectl get nodes >/dev/null 2>&1")
					cancel()
					if code == 0 && err == nil {
						masterIP = m.IP
						break
					}
				}
			}
			if masterIP == "" {
				masterIP = cfg.Nodes.Masters[0].IP
			}
			if masterIP == "" {
				return fmt.Errorf("no master node has a usable IP address with a reachable Kubernetes API")
			}
			dbUser := cfg.Database.User
			if dbUser == "" {
				dbUser = "wp_user"
			}
			dbName := cfg.Database.Name
			if dbName == "" {
				dbName = "wordpress"
			}
			if !sqlIdentifier.MatchString(dbUser) || !sqlIdentifier.MatchString(dbName) {
				return fmt.Errorf("database.user and database.name must contain only letters, digits, or underscores")
			}
			podCmd := "kubectl get pod -n cms -l app=mariadb --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}'"
			podName, _, code, err := pool.RunCommand(ctx, masterIP, podCmd)
			podName = strings.TrimSpace(podName)
			if err != nil || code != 0 || !kubernetesPodName.MatchString(podName) {
				return fmt.Errorf("no running MariaDB pod found in namespace cms on %s (exit %d): %v", masterIP, code, err)
			}

			// Stream the password from the Kubernetes Secret over stdin. Encoding
			// it into this SSH command would only obfuscate it and expose it in ps.
			backupCmdStr := fmt.Sprintf(`kubectl get secret mariadb-secret -n cms -o jsonpath='{.data.MYSQL_WP_PASSWORD}' | base64 -d | kubectl exec -i -n cms %s -- sh -c 'MYSQL_PWD="$(cat)" mysqldump --single-transaction --quick -u%s %s'`, podName, dbUser, dbName)
			tempFile, err := os.CreateTemp(".", ".cms-ha-backup-*.sql")
			if err != nil {
				return fmt.Errorf("create temporary backup file: %w", err)
			}
			tempPath := tempFile.Name()
			cleanupTemp := func() {
				_ = tempFile.Close()
				_ = os.Remove(tempPath)
			}

			stderr, code, err := pool.RunCommandToWriter(ctx, masterIP, backupCmdStr, tempFile)
			if err != nil || code != 0 {
				cleanupTemp()
				return fmt.Errorf("mysqldump failed on %s (exit %d): %v\nStderr: %s", masterIP, code, err, stderr)
			}
			info, err := tempFile.Stat()
			if err != nil {
				cleanupTemp()
				return fmt.Errorf("inspect temporary backup file: %w", err)
			}
			if info.Size() == 0 {
				cleanupTemp()
				return fmt.Errorf("mysqldump produced empty output: %s", stderr)
			}
			if err := tempFile.Sync(); err != nil {
				cleanupTemp()
				return fmt.Errorf("flush backup to disk: %w", err)
			}
			if err := tempFile.Close(); err != nil {
				cleanupTemp()
				return fmt.Errorf("close temporary backup file: %w", err)
			}
			now := time.Now()
			backupFile := fmt.Sprintf("backup_%s_%09d.sql", now.Format("20060102_150405"), now.Nanosecond())
			if err := os.Rename(tempPath, backupFile); err != nil {
				cleanupTemp()
				return fmt.Errorf("publish database backup: %w", err)
			}
			logging.Success("Database backup saved to %s", backupFile)
			return nil
		},
	}
	backupCmd.AddCommand(dbCmd)
}

var sqlIdentifier = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
var kubernetesPodName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
