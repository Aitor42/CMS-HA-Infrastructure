package root

import (
    "encoding/base64"
    "fmt"
    "os"
    "strings"
    "time"

    "github.com/spf13/cobra"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
)

var backupCmd = &cobra.Command{
    Use:   "backup",
    Short: "Backup utilities",
}

func init() {
    rootCmd.AddCommand(backupCmd)
    
    dbCmd := &cobra.Command{
        Use: "db",
        Run: func(cmd *cobra.Command, args []string) {
            cfg, err := loadConfig()
            if err != nil { handleError(err); return }
            pool, err := newSSHPool(cfg)
            if err != nil { handleError(err); return }
            defer pool.Close()
            ctx := cmd.Context()
            if len(cfg.Nodes.Masters) == 0 {
                handleError(fmt.Errorf("no master nodes defined in configuration"))
                return
            }
            master1 := cfg.Nodes.Masters[0].IP
            b64Pass := base64.StdEncoding.EncodeToString([]byte(cfg.Database.Password))
            backupCmdStr := fmt.Sprintf(`kubectl exec -n cms $(kubectl get pod -n cms -l app=mariadb -o jsonpath='{.items[0].metadata.name}') -- sh -c 'MYSQL_PWD="$(echo "%s" | base64 -d)" mysqldump -u%s %s'`, b64Pass, cfg.Database.User, cfg.Database.Name)
            stdout, stderr, code, err := pool.RunCommand(ctx, master1, backupCmdStr)
            if err != nil || code != 0 {
                handleError(fmt.Errorf("mysqldump failed on %s (exit %d): %v\nStderr: %s", master1, code, err, stderr))
                return
            }
            if len(strings.TrimSpace(stdout)) == 0 {
                handleError(fmt.Errorf("mysqldump produced empty output: %s", stderr))
                return
            }
            backupFile := fmt.Sprintf("backup_%s.sql", time.Now().Format("20060102_150405"))
            if err := os.WriteFile(backupFile, []byte(stdout), 0600); err != nil {
                handleError(fmt.Errorf("failed to write backup file: %w", err))
                return
            }
            logging.Success("Database backup saved to %s", backupFile)
        },
    }
    backupCmd.AddCommand(dbCmd)
}
