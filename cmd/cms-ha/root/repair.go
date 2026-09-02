package root

import (
    "context"

    "github.com/spf13/cobra"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/utils"
)

var repairCmd = &cobra.Command{
    Use:   "repair",
    Short: "Repair infrastructure",
}

func init() {
    rootCmd.AddCommand(repairCmd)
    
    k8sCmd := &cobra.Command{
        Use: "k8s",
        Run: func(cmd *cobra.Command, args []string) {
            cfg, err := loadConfig()
            if err != nil { handleError(err); return }
            pool, err := newSSHPool(cfg)
            if err != nil { handleError(err); return }
            defer pool.Close()
            if err := utils.RepairK8s(context.Background(), cfg, pool); err != nil { handleError(err) }
        },
    }
    repairCmd.AddCommand(k8sCmd)

    clocksCmd := &cobra.Command{
        Use: "clocks",
        Run: func(cmd *cobra.Command, args []string) {
            cfg, err := loadConfig()
            if err != nil { handleError(err); return }
            pool, err := newSSHPool(cfg)
            if err != nil { handleError(err); return }
            defer pool.Close()
            if err := utils.SyncClocks(context.Background(), cfg, pool); err != nil { handleError(err) }
        },
    }
    repairCmd.AddCommand(clocksCmd)
}
