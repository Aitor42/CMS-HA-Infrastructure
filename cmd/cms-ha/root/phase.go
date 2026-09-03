package root

import (

    "github.com/spf13/cobra"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/initvms"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/cobbler"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/registernodes"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/repairssh"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/puppet"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/drbd"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/kubernetes"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/nginx"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/monitoring"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/ufw"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/pki"
)

var phaseCmd = &cobra.Command{
    Use:   "phase",
    Short: "Run a specific phase",
}

func init() {
    rootCmd.AddCommand(phaseCmd)

    initVMsCmd := &cobra.Command{
        Use: "init-vms",
        Run: func(cmd *cobra.Command, args []string) {
            cfg, err := loadConfig()
            if err != nil { handleError(err); return }
            pool, err := newSSHPool(cfg)
            if err != nil { handleError(err); return }
            defer pool.Close()
            lv := newLibvirtClient(cfg)
            
            jumpstartOnly, _ := cmd.Flags().GetBool("jumpstart-only")
            nodesOnly, _ := cmd.Flags().GetBool("nodes-only")
            cleanup, _ := cmd.Flags().GetBool("cleanup")
            recreate, _ := cmd.Flags().GetBool("recreate")
            
            opts := initvms.Options{
                JumpstartOnly: jumpstartOnly,
                NodesOnly:     nodesOnly,
                Cleanup:       cleanup,
                Recreate:      recreate,
            }
            
            p := initvms.NewPhaseWithOpts(cfg, pool, lv, opts)
            if err := p.Run(cmd.Context()); err != nil {
                handleError(err)
            }
        },
    }
    initVMsCmd.Flags().Bool("jumpstart-only", false, "")
    initVMsCmd.Flags().Bool("nodes-only", false, "")
    initVMsCmd.Flags().Bool("cleanup", false, "")
    initVMsCmd.Flags().Bool("recreate", false, "")
    phaseCmd.AddCommand(initVMsCmd)

    // Helper for phases without libvirt
    addPhaseCmd := func(use string, phaseFactory func(cmd *cobra.Command) error, aliases ...string) {
        cmd := &cobra.Command{
            Use:     use,
            Aliases: aliases,
            Run: func(cmd *cobra.Command, args []string) {
                if err := phaseFactory(cmd); err != nil {
                    handleError(err)
                }
            },
        }
        phaseCmd.AddCommand(cmd)
    }

    addPhaseCmd("setup-cobbler", func(cmd *cobra.Command) error {
        cfg, err := loadConfig()
        if err != nil { return err }
        pool, err := newSSHPool(cfg)
        if err != nil { return err }
        defer pool.Close()
        p := cobbler.NewPhase(cfg, pool)
        return p.Run(cmd.Context())
    })

    addPhaseCmd("register-nodes", func(cmd *cobra.Command) error {
        cfg, err := loadConfig()
        if err != nil { return err }
        pool, err := newSSHPool(cfg)
        if err != nil { return err }
        defer pool.Close()
        p := registernodes.NewPhase(cfg, pool)
        return p.Run(cmd.Context())
    })

    addPhaseCmd("repair-ssh", func(cmd *cobra.Command) error {
        cfg, err := loadConfig()
        if err != nil { return err }
        pool, err := newSSHPool(cfg)
        if err != nil { return err }
        defer pool.Close()
        p := repairssh.NewPhase(cfg, pool)
        return p.Run(cmd.Context())
    })

    addPhaseCmd("setup-puppet", func(cmd *cobra.Command) error {
        cfg, err := loadConfig()
        if err != nil { return err }
        pool, err := newSSHPool(cfg)
        if err != nil { return err }
        defer pool.Close()
        p := puppet.NewPhase(cfg, pool)
        return p.Run(cmd.Context())
    })

    addPhaseCmd("setup-drbd", func(cmd *cobra.Command) error {
        cfg, err := loadConfig()
        if err != nil { return err }
        pool, err := newSSHPool(cfg)
        if err != nil { return err }
        defer pool.Close()
        p := drbd.NewPhase(cfg, pool)
        return p.Run(cmd.Context())
    })

    addPhaseCmd("setup-kubernetes", func(cmd *cobra.Command) error {
        cfg, err := loadConfig()
        if err != nil { return err }
        pool, err := newSSHPool(cfg)
        if err != nil { return err }
        defer pool.Close()
        p := kubernetes.NewPhase(cfg, pool)
        return p.Run(cmd.Context())
    })

    addPhaseCmd("setup-nginx-wordpress", func(cmd *cobra.Command) error {
        cfg, err := loadConfig()
        if err != nil { return err }
        pool, err := newSSHPool(cfg)
        if err != nil { return err }
        defer pool.Close()
        p := nginx.NewPhase(cfg, pool)
        return p.Run(cmd.Context())
    }, "setup-nginx")

    addPhaseCmd("setup-monitoring", func(cmd *cobra.Command) error {
        cfg, err := loadConfig()
        if err != nil { return err }
        pool, err := newSSHPool(cfg)
        if err != nil { return err }
        defer pool.Close()
        p := monitoring.NewPhase(cfg, pool)
        return p.Run(cmd.Context())
    })

    addPhaseCmd("setup-ufw", func(cmd *cobra.Command) error {
        cfg, err := loadConfig()
        if err != nil { return err }
        pool, err := newSSHPool(cfg)
        if err != nil { return err }
        defer pool.Close()
        p := ufw.NewPhase(cfg, pool)
        return p.Run(cmd.Context())
    })

    addPhaseCmd("setup-ca", func(cmd *cobra.Command) error {
        cfg, err := loadConfig()
        if err != nil { return err }
        pool, err := newSSHPool(cfg)
        if err != nil { return err }
        defer pool.Close()
        p := pki.NewPhase(cfg, pool)
        return p.Run(cmd.Context())
    })
}
