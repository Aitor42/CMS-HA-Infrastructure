package root

import (
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/utils"
	"github.com/spf13/cobra"
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
		Short:   "Start all VMs in dependency order",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfigPartial()
			if err != nil {
				return err
			}
			lv := newLibvirtClient(cfg)
			return utils.StartAllVMs(cmd.Context(), cfg, lv, 5*time.Second)
		},
	}
	vmCmd.AddCommand(startCmd)

	vmStopCmd := &cobra.Command{
		Use:     "stop",
		Aliases: []string{"stop-all"},
		Short:   "Stop all VMs",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfigPartial()
			if err != nil {
				return err
			}
			lv := newLibvirtClient(cfg)
			force, _ := cmd.Flags().GetBool("force")
			timeout, _ := cmd.Flags().GetDuration("timeout")
			opts := utils.StopOptions{
				Force:   force,
				Timeout: timeout,
			}
			return utils.StopAllVMs(cmd.Context(), cfg, lv, opts)
		},
	}
	vmStopCmd.Flags().BoolP("force", "f", false, "Force power off (destroy) VMs that do not stop gracefully within the timeout")
	vmStopCmd.Flags().DurationP("timeout", "t", 30*time.Second, "Maximum duration to wait for VMs to power off")
	vmCmd.AddCommand(vmStopCmd)

	shrinkCmd := &cobra.Command{
		Use:     "shrink",
		Aliases: []string{"shrink-ram"},
		Short:   "Shrink VM RAM to the production sizing",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfigPartial()
			if err != nil {
				return err
			}
			lv := newLibvirtClient(cfg)
			return utils.ShrinkVMRAM(cmd.Context(), cfg, lv)
		},
	}
	vmCmd.AddCommand(shrinkCmd)

	fixBootCmd := &cobra.Command{
		Use:     "fix-boot-order",
		Aliases: []string{"fix-boot"},
		Short:   "Ensure disks boot before the network interface",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfigPartial()
			if err != nil {
				return err
			}
			lv := newLibvirtClient(cfg)

			nodes := cfg.AllNodes()
			names := make([]string, len(nodes))
			for i, n := range nodes {
				names[i] = n.Name
			}
			return utils.FixBootOrder(cmd.Context(), lv, names)
		},
	}
	vmCmd.AddCommand(fixBootCmd)

	installBatchesCmd := &cobra.Command{
		Use:   "install-batches",
		Short: "Install client nodes in batches via PXE",
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

			return utils.InstallByBatches(cmd.Context(), cfg, lv, pool)
		},
	}
	vmCmd.AddCommand(installBatchesCmd)

	recreateCmd := &cobra.Command{
		Use:     "recreate",
		Aliases: []string{"recreate-failed"},
		Short:   "Recreate VMs that failed verification",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfigPartial()
			if err != nil {
				return err
			}
			lv := newLibvirtClient(cfg)
			return utils.RecreateFailedVMs(cmd.Context(), cfg, lv)
		},
	}
	vmCmd.AddCommand(recreateCmd)
}
