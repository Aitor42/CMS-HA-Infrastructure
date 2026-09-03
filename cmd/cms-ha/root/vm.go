package root

import (
    "time"

    "github.com/spf13/cobra"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/utils"
)

var vmCmd = &cobra.Command{
    Use:   "vm",
    Short: "VM management",
}

func init() {
    rootCmd.AddCommand(vmCmd)
    
    startCmd := &cobra.Command{
        Use:     "start",
        Aliases: []string{"start-all"},
        Run: func(cmd *cobra.Command, args []string) {
            cfg, err := loadConfig()
            if err != nil { handleError(err); return }
            lv := newLibvirtClient(cfg)
            if err := utils.StartAllVMs(cmd.Context(), cfg, lv, 5*time.Second); err != nil { handleError(err) }
        },
    }
    vmCmd.AddCommand(startCmd)

    shrinkCmd := &cobra.Command{
        Use:     "shrink",
        Aliases: []string{"shrink-ram"},
        Run: func(cmd *cobra.Command, args []string) {
            cfg, err := loadConfig()
            if err != nil { handleError(err); return }
            lv := newLibvirtClient(cfg)
            if err := utils.ShrinkVMRAM(cmd.Context(), cfg, lv); err != nil { handleError(err) }
        },
    }
    vmCmd.AddCommand(shrinkCmd)

    fixBootCmd := &cobra.Command{
        Use:     "fix-boot-order",
        Aliases: []string{"fix-boot"},
        Run: func(cmd *cobra.Command, args []string) {
            cfg, err := loadConfig()
            if err != nil { handleError(err); return }
            lv := newLibvirtClient(cfg)
            
            nodes := cfg.AllNodes()
            names := make([]string, len(nodes))
            for i, n := range nodes { names[i] = n.Name }
            if err := utils.FixBootOrder(cmd.Context(), lv, names); err != nil { handleError(err) }
        },
    }
    vmCmd.AddCommand(fixBootCmd)

    installBatchesCmd := &cobra.Command{
        Use: "install-batches",
        Run: func(cmd *cobra.Command, args []string) {
            cfg, err := loadConfig()
            if err != nil { handleError(err); return }
            pool, err := newSSHPool(cfg)
            if err != nil { handleError(err); return }
            defer pool.Close()
            lv := newLibvirtClient(cfg)
            
            force, _ := cmd.Flags().GetBool("force")
            if err := utils.InstallByBatches(cmd.Context(), cfg, lv, pool, force); err != nil { handleError(err) }
        },
    }
    installBatchesCmd.Flags().Bool("force", false, "")
    vmCmd.AddCommand(installBatchesCmd)

    recreateCmd := &cobra.Command{
        Use:     "recreate",
        Aliases: []string{"recreate-failed"},
        Run: func(cmd *cobra.Command, args []string) {
            cfg, err := loadConfig()
            if err != nil { handleError(err); return }
            lv := newLibvirtClient(cfg)
            if err := utils.RecreateFailedVMs(cmd.Context(), cfg, lv); err != nil { handleError(err) }
        },
    }
    vmCmd.AddCommand(recreateCmd)
}
