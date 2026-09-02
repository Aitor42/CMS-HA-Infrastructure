package root

import (
    "fmt"
    "os"
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
            master1 := cfg.Nodes.Masters[0].IP
            backupCmdStr := fmt.Sprintf(`kubectl exec -n cms $(kubectl get pod -n cms -l app=mariadb -o jsonpath='{.items[0].metadata.name}') -- mysqldump -u%s -p'%s' %s`, cfg.Database.User, cfg.Database.Password, cfg.Database.Name)
            stdout, _, _, err := pool.RunCommand(ctx, master1, backupCmdStr)
            if err != nil { handleError(err); return }
            backupFile := fmt.Sprintf("backup_%s.sql", time.Now().Format("20060102_150405"))
            os.WriteFile(backupFile, []byte(stdout), 0644)
            logging.Success("Database backup saved to %s", backupFile)
        },
    }
    backupCmd.AddCommand(dbCmd)
}
