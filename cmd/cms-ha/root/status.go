package root

import (
    "context"

    "github.com/spf13/cobra"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/utils"
)

var statusCmd = &cobra.Command{
    Use:   "status",
    Short: "Check status",
}

func init() {
    rootCmd.AddCommand(statusCmd)
    
    sshCmd := &cobra.Command{
        Use: "ssh",
        Run: func(cmd *cobra.Command, args []string) {
            cfg, err := loadConfig()
            if err != nil { handleError(err); return }
            pool, err := newSSHPool(cfg)
            if err != nil { handleError(err); return }
            defer pool.Close()
            lv := newLibvirtClient(cfg)
            if err := utils.CheckSSH(context.Background(), cfg, pool, lv); err != nil { handleError(err) }
        },
    }
    statusCmd.AddCommand(sshCmd)
}
