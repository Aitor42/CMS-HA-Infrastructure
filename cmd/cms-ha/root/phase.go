package root

import (
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/backup"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/cobbler"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/drbd"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/initvms"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/kubernetes"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/monitoring"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/nginx"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/pki"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/puppet"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/registernodes"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/repairssh"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/ufw"
	"github.com/spf13/cobra"
)

var phaseCmd = &cobra.Command{
	Use:   "phase",
	Short: "Run a specific phase",
}

func init() {
	rootCmd.AddCommand(phaseCmd)

	initVMsCmd := &cobra.Command{
		Use: "init-vms",
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
			return p.Run(cmd.Context())
		},
	}
	initVMsCmd.Flags().Bool("jumpstart-only", false, "Deploy only the Jumpstart VM (skips client nodes)")
	initVMsCmd.Flags().Bool("nodes-only", false, "Deploy only client nodes (reuses the existing Jumpstart)")
	initVMsCmd.Flags().Bool("cleanup", false, "Destroy all VMs and networks")
	initVMsCmd.Flags().Bool("recreate", false, "Destroy and recreate the Jumpstart VM")
	phaseCmd.AddCommand(initVMsCmd)

	// Helper for phases without libvirt
	addPhaseCmd := func(use string, phaseFactory func(cmd *cobra.Command) error, aliases ...string) {
		cmd := &cobra.Command{
			Use:     use,
			Aliases: aliases,
			RunE: func(cmd *cobra.Command, args []string) error {
				return phaseFactory(cmd)
			},
		}
		phaseCmd.AddCommand(cmd)
	}

	addPhaseCmd("setup-cobbler", func(cmd *cobra.Command) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		pool, err := newSSHPool(cfg)
		if err != nil {
			return err
		}
		defer pool.Close()
		p := cobbler.NewPhase(cfg, pool)
		return p.Run(cmd.Context())
	}, "cobbler")

	addPhaseCmd("register-nodes", func(cmd *cobra.Command) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		pool, err := newSSHPool(cfg)
		if err != nil {
			return err
		}
		defer pool.Close()
		p := registernodes.NewPhase(cfg, pool)
		return p.Run(cmd.Context())
	}, "register")

	addPhaseCmd("repair-ssh", func(cmd *cobra.Command) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		pool, err := newSSHPool(cfg)
		if err != nil {
			return err
		}
		defer pool.Close()
		p := repairssh.NewPhase(cfg, pool)
		return p.Run(cmd.Context())
	}, "ssh")

	addPhaseCmd("setup-puppet", func(cmd *cobra.Command) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		pool, err := newSSHPool(cfg)
		if err != nil {
			return err
		}
		defer pool.Close()
		p := puppet.NewPhase(cfg, pool)
		return p.Run(cmd.Context())
	}, "puppet")

	addPhaseCmd("setup-drbd", func(cmd *cobra.Command) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		pool, err := newSSHPool(cfg)
		if err != nil {
			return err
		}
		defer pool.Close()
		p := drbd.NewPhase(cfg, pool)
		return p.Run(cmd.Context())
	}, "drbd")

	addPhaseCmd("setup-backup", func(cmd *cobra.Command) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		p := backup.NewPhase(cfg)
		return p.Run(cmd.Context())
	}, "backup")

	addPhaseCmd("setup-kubernetes", func(cmd *cobra.Command) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		pool, err := newSSHPool(cfg)
		if err != nil {
			return err
		}
		defer pool.Close()
		p := kubernetes.NewPhase(cfg, pool)
		return p.Run(cmd.Context())
	}, "kubernetes", "k8s")

	addPhaseCmd("setup-nginx-wordpress", func(cmd *cobra.Command) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		pool, err := newSSHPool(cfg)
		if err != nil {
			return err
		}
		defer pool.Close()
		p := nginx.NewPhase(cfg, pool)
		return p.Run(cmd.Context())
	}, "setup-nginx", "nginx", "wordpress")

	addPhaseCmd("setup-monitoring", func(cmd *cobra.Command) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		pool, err := newSSHPool(cfg)
		if err != nil {
			return err
		}
		defer pool.Close()
		p := monitoring.NewPhase(cfg, pool)
		return p.Run(cmd.Context())
	}, "monitoring")

	addPhaseCmd("setup-ufw", func(cmd *cobra.Command) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		pool, err := newSSHPool(cfg)
		if err != nil {
			return err
		}
		defer pool.Close()
		p := ufw.NewPhase(cfg, pool)
		return p.Run(cmd.Context())
	}, "ufw")

	addPhaseCmd("setup-ca", func(cmd *cobra.Command) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		pool, err := newSSHPool(cfg)
		if err != nil {
			return err
		}
		defer pool.Close()
		p := pki.NewPhase(cfg, pool)
		return p.Run(cmd.Context())
	}, "ca", "pki")
}
