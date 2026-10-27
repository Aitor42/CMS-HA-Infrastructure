package root

import (
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/backup"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/catalog"
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
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/routerbootstrap"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/ufw"
	"github.com/spf13/cobra"
)

var phaseCmd = &cobra.Command{
	Use:   "phase",
	Short: "Run a specific phase",
}

// dryRunPhase validates the configuration and reports the phase without
// creating an SSH pool or touching libvirt. The read-only loader is deliberate:
// a simulation must not auto-encrypt config.yaml.
func dryRunPhase(name string, load func() (*config.Config, error)) (bool, error) {
	if !dryRun {
		return false, nil
	}
	if _, err := load(); err != nil {
		return false, err
	}
	logging.Info("Dry run: phase %s would run; no commands were executed", name)
	return true, nil
}

func isPhaseCommand(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c == phaseCmd {
			return true
		}
	}
	return false
}

func init() {
	rootCmd.AddCommand(phaseCmd)

	initVMsCmd := &cobra.Command{
		Use:   catalog.InitVMs.CLI,
		Short: catalog.InitVMs.Short,
		RunE: func(cmd *cobra.Command, args []string) error {
			jumpstartOnly, _ := cmd.Flags().GetBool("jumpstart-only")
			nodesOnly, _ := cmd.Flags().GetBool("nodes-only")
			cleanup, _ := cmd.Flags().GetBool("cleanup")
			recreate, _ := cmd.Flags().GetBool("recreate")
			jumpstartImage, _ := cmd.Flags().GetString("jumpstart-image")
			fastProvisioning, _ := cmd.Flags().GetBool("fast-provisioning")

			// --cleanup only removes what is present, so it must tolerate an
			// incomplete config exactly like `cms-ha cleanup`/`destroy`.
			load := loadConfig
			if cleanup {
				load = loadConfigPartial
			}
			readOnlyLoad := loadConfigReadOnly
			if cleanup {
				readOnlyLoad = loadConfigPartialReadOnly
			}
			if done, err := dryRunPhase("init-vms", readOnlyLoad); done || err != nil {
				return err
			}
			cfg, err := load()
			if err != nil {
				return err
			}
			pool, err := newSSHPool(cfg)
			if err != nil {
				return err
			}
			defer pool.Close()
			lv := newLibvirtClient(cfg)

			opts := initvms.Options{
				JumpstartOnly:    jumpstartOnly,
				NodesOnly:        nodesOnly,
				Cleanup:          cleanup,
				Recreate:         recreate,
				JumpstartImage:   jumpstartImage,
				FastProvisioning: fastProvisioning,
			}

			p := initvms.NewPhaseWithOpts(cfg, pool, lv, opts)
			return p.Run(cmd.Context())
		},
	}
	initVMsCmd.Flags().Bool("jumpstart-only", false, "Deploy only the Jumpstart VM (skips client nodes)")
	initVMsCmd.Flags().Bool("nodes-only", false, "Deploy only client nodes (reuses the existing Jumpstart)")
	initVMsCmd.Flags().Bool("cleanup", false, "Destroy all VMs and networks")
	initVMsCmd.Flags().Bool("recreate", false, "Destroy and recreate the Jumpstart VM")
	initVMsCmd.Flags().String("jumpstart-image", "", "Jumpstart install path: iso (default) or cloud (opt-in fast path)")
	initVMsCmd.Flags().Bool("fast-provisioning", false, "Install client nodes from the cloud image (opt-in fast path; PXE stays the default)")
	phaseCmd.AddCommand(initVMsCmd)

	// Helper for phases without libvirt
	addPhaseCmd := func(use, short string, phaseFactory func(cmd *cobra.Command) error, aliases ...string) {
		cmd := &cobra.Command{
			Use:     use,
			Short:   short,
			Aliases: aliases,
			RunE: func(cmd *cobra.Command, args []string) error {
				if done, err := dryRunPhase(use, loadConfigReadOnly); done || err != nil {
					return err
				}
				return phaseFactory(cmd)
			},
		}
		phaseCmd.AddCommand(cmd)
	}

	addPhaseCmd(catalog.Cobbler.CLI, catalog.Cobbler.Short, func(cmd *cobra.Command) error {
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
	}, catalog.Cobbler.Aliases...)

	addPhaseCmd(catalog.RegisterNodes.CLI, catalog.RegisterNodes.Short, func(cmd *cobra.Command) error {
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
	}, catalog.RegisterNodes.Aliases...)

	addPhaseCmd(catalog.RepairSSH.CLI, catalog.RepairSSH.Short, func(cmd *cobra.Command) error {
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
	}, catalog.RepairSSH.Aliases...)

	addPhaseCmd(catalog.RouterBootstrap.CLI, catalog.RouterBootstrap.Short, func(cmd *cobra.Command) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		pool, err := newSSHPool(cfg)
		if err != nil {
			return err
		}
		defer pool.Close()
		p := routerbootstrap.NewPhase(cfg, pool)
		return p.Run(cmd.Context())
	}, catalog.RouterBootstrap.Aliases...)

	addPhaseCmd(catalog.Puppet.CLI, catalog.Puppet.Short, func(cmd *cobra.Command) error {
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
	}, catalog.Puppet.Aliases...)

	addPhaseCmd(catalog.DRBD.CLI, catalog.DRBD.Short, func(cmd *cobra.Command) error {
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
	}, catalog.DRBD.Aliases...)

	addPhaseCmd(catalog.Backup.CLI, catalog.Backup.Short, func(cmd *cobra.Command) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		p := backup.NewPhase(cfg)
		return p.Run(cmd.Context())
	}, catalog.Backup.Aliases...)

	addPhaseCmd(catalog.Kubernetes.CLI, catalog.Kubernetes.Short, func(cmd *cobra.Command) error {
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
	}, catalog.Kubernetes.Aliases...)

	addPhaseCmd(catalog.Nginx.CLI, catalog.Nginx.Short, func(cmd *cobra.Command) error {
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
	}, catalog.Nginx.Aliases...)

	addPhaseCmd(catalog.Monitoring.CLI, catalog.Monitoring.Short, func(cmd *cobra.Command) error {
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
	}, catalog.Monitoring.Aliases...)

	addPhaseCmd(catalog.UFW.CLI, catalog.UFW.Short, func(cmd *cobra.Command) error {
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
	}, catalog.UFW.Aliases...)

	addPhaseCmd(catalog.PKI.CLI, catalog.PKI.Short, func(cmd *cobra.Command) error {
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
	}, catalog.PKI.Aliases...)
}
