package root

import (
    "context"

    "github.com/spf13/cobra"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/utils"
)

var testCmd = &cobra.Command{
    Use:   "test",
    Short: "Test commands",
}

func init() {
    rootCmd.AddCommand(testCmd)
    
    failoverCmd := &cobra.Command{
        Use: "failover",
        Run: func(cmd *cobra.Command, args []string) {
            cfg, err := loadConfig()
            if err != nil { handleError(err); return }
            pool, err := newSSHPool(cfg)
            if err != nil { handleError(err); return }
            defer pool.Close()
            lv := newLibvirtClient(cfg)
            
            ft := utils.NewFailoverTester(cfg, pool, lv)
            skipRestore, _ := cmd.Flags().GetBool("skip-restore")
            opts := utils.FailoverOpts{SkipRestore: skipRestore}
            
            if err := ft.Run(context.Background(), opts); err != nil {
                handleError(err)
            }
        },
    }
    failoverCmd.Flags().Bool("skip-restore", false, "Skip restore after failover")
    testCmd.AddCommand(failoverCmd)
}
