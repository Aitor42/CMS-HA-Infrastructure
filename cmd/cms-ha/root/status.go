package root

import (
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/utils"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Check status",
}

func init() {
	rootCmd.AddCommand(statusCmd)

	sshCmd := &cobra.Command{
		Use: "ssh",
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
			return utils.CheckSSH(cmd.Context(), cfg, pool, lv)
		},
	}
	statusCmd.AddCommand(sshCmd)
}
