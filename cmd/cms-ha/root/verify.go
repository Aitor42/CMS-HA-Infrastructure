package root

import (
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/utils"
	"github.com/spf13/cobra"
)

var verifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Run full infrastructure verification",
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

		v := utils.NewVerifier(cfg, pool, lv)
		return v.VerifyAll(cmd.Context())
	},
}

func init() {
	rootCmd.AddCommand(verifyCmd)
}
