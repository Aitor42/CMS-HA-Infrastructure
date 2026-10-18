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
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/routerbootstrap"
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
		Use:   "init-vms",
		Short: "Initialize libvirt networks and VMs",
		RunE: func(cmd *cobra.Command, args []string) error {
			jumpstartOnly, _ := cmd.Flags().GetBool("jumpstart-only")
			nodesOnly, _ := cmd.Flags().GetBool("nodes-only")
			cleanup, _ := cmd.Flags().GetBool("cleanup")
			recreate, _ := cmd.Flags().GetBool("recreate")

			// --cleanup only removes what is present, so it must tolerate an
			// incomplete config exactly like `cms-ha cleanup`/`destroy`.
			load := loadConfig
			if cleanup {
				load = loadConfigPartial
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
	addPhaseCmd := func(use, short string, phaseFactory func(cmd *cobra.Command) error, aliases ...string) {
		cmd := &cobra.Command{
			Use:     use,
			Short:   short,
			Aliases: aliases,
			RunE: func(cmd *cobra.Command, args []string) error {
				return phaseFactory(cmd)
			},
		}
		phaseCmd.AddCommand(cmd)
	}

	addPhaseCmd("setup-cobbler", "Install and configure the Cobbler provisioning server", func(cmd *cobra.Command) error {
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

	addPhaseCmd("register-nodes", "Register client nodes with Cobbler", func(cmd *cobra.Command) error {
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

	addPhaseCmd("repair-ssh", "Repair SSH host keys across all nodes", func(cmd *cobra.Command) error {
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

	addPhaseCmd("bootstrap-router", "Give the router its gateway role (addresses, forwarding, NAT)", func(cmd *cobra.Command) error {
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
	}, "router-bootstrap")

	addPhaseCmd("setup-puppet", "Install and configure the Puppet server", func(cmd *cobra.Command) error {
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

	addPhaseCmd("setup-drbd", "Configure DRBD replication on the master nodes", func(cmd *cobra.Command) error {
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

	addPhaseCmd("setup-backup", "Configure the S3 (Garage) backup stack", func(cmd *cobra.Command) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		p := backup.NewPhase(cfg)
		return p.Run(cmd.Context())
	}, "backup")

	addPhaseCmd("setup-kubernetes", "Install and configure the K3s cluster", func(cmd *cobra.Command) error {
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

	addPhaseCmd("setup-nginx-wordpress", "Configure Nginx, WordPress and the CMS frontends", func(cmd *cobra.Command) error {
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

	addPhaseCmd("setup-monitoring", "Install and configure Prometheus and Grafana", func(cmd *cobra.Command) error {
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

	addPhaseCmd("setup-ufw", "Configure UFW firewall rules", func(cmd *cobra.Command) error {
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

	addPhaseCmd("setup-ca", "Set up the internal certificate authority (PKI)", func(cmd *cobra.Command) error {
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
