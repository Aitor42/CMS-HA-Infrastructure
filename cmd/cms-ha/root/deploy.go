package root

import (
    "context"

    "github.com/spf13/cobra"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/deploy"
)

var (
    skipVMCreate bool
)

var deployCmd = &cobra.Command{
    Use:   "deploy",
    Short: "Deploy infrastructure",
    Run: func(cmd *cobra.Command, args []string) {
        cfg, err := loadConfig()
        if err != nil { handleError(err); return }
        pool, err := newSSHPool(cfg)
        if err != nil { handleError(err); return }
        defer pool.Close()
        
        lv := newLibvirtClient(cfg)
        
        orchestrator := &deploy.Orchestrator{
            Cfg:     cfg,
            Libvirt: lv,
            SSH:     pool,
        }
        
        opts := deploy.DeployOpts{
            SkipVMCreate: skipVMCreate,
            DryRun:       dryRun, // from root.go
        }
        
        if err := orchestrator.Deploy(context.Background(), opts); err != nil {
            handleError(err)
        }
    },
}

func init() {
    deployCmd.Flags().BoolVar(&skipVMCreate, "skip-vm-create", false, "Skip VM creation")
    rootCmd.AddCommand(deployCmd)
}
