package root

import (
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/utils"
	"github.com/spf13/cobra"
)

var repairCmd = &cobra.Command{
	Use:   "repair",
	Short: "Repair infrastructure",
}

func init() {
	rootCmd.AddCommand(repairCmd)

	k8sCmd := &cobra.Command{
		Use:   "k8s",
		Short: "Repair the Kubernetes cluster (k3s quorum and node state)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfigPartial()
			if err != nil {
				return err
			}
			pool, err := newSSHPool(cfg)
			if err != nil {
				return err
			}
			defer pool.Close()
			return utils.RepairK8s(cmd.Context(), cfg, pool)
		},
	}
	repairCmd.AddCommand(k8sCmd)

	clocksCmd := &cobra.Command{
		Use:   "clocks",
		Short: "Sync the clock on all configured nodes",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfigPartial()
			if err != nil {
				return err
			}
			pool, err := newSSHPool(cfg)
			if err != nil {
				return err
			}
			defer pool.Close()
			return utils.SyncClocks(cmd.Context(), cfg, pool)
		},
	}
	repairCmd.AddCommand(clocksCmd)
}
