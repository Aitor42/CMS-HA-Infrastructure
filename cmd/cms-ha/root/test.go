package root

import (
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/utils"
	"github.com/spf13/cobra"
)

var testCmd = &cobra.Command{
	Use:   "test",
	Short: "Test commands",
}

func init() {
	rootCmd.AddCommand(testCmd)

	failoverCmd := &cobra.Command{
		Use:   "failover",
		Short: "Run the HA failover test (DRBD, CMS frontend, K3s worker, LB VIP)",
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
			lv := newLibvirtClient(cfg)

			ft := utils.NewFailoverTester(cfg, pool, lv)
			skipRestore, _ := cmd.Flags().GetBool("skip-restore")
			opts := utils.FailoverOpts{SkipRestore: skipRestore}

			return ft.Run(cmd.Context(), opts)
		},
	}
	failoverCmd.Flags().Bool("skip-restore", false, "Skip restore after failover")
	testCmd.AddCommand(failoverCmd)

	drillsCmd := &cobra.Command{
		Use:   "drills",
		Short: "Exercise the backup/restore drill, TLS renewal and alert hygiene",
		Long: `Run the operational paths a deploy and verify never touch: the daily
MariaDB backup and restore CronJobs, the load-balancer certificate renewal and
the assertion that no alert is firing on a healthy lab.`,
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

			skipBackup, _ := cmd.Flags().GetBool("skip-backup")
			skipTLS, _ := cmd.Flags().GetBool("skip-tls")
			return utils.RunDrills(cmd.Context(), cfg, pool, utils.DrillsOptions{
				SkipBackup: skipBackup,
				SkipTLS:    skipTLS,
			})
		},
	}
	drillsCmd.Flags().Bool("skip-backup", false, "Skip the backup/restore drill")
	drillsCmd.Flags().Bool("skip-tls", false, "Skip the TLS renewal drill")
	testCmd.AddCommand(drillsCmd)
}
