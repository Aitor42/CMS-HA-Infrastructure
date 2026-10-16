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
		Use: "failover",
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

			ft := utils.NewFailoverTester(cfg, pool, lv)
			skipRestore, _ := cmd.Flags().GetBool("skip-restore")
			opts := utils.FailoverOpts{SkipRestore: skipRestore}

			return ft.Run(cmd.Context(), opts)
		},
	}
	failoverCmd.Flags().Bool("skip-restore", false, "Skip restore after failover")
	testCmd.AddCommand(failoverCmd)
}
