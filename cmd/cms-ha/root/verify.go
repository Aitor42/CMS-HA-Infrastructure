package root

import (
    "context"

    "github.com/spf13/cobra"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/utils"
)

var verifyCmd = &cobra.Command{
    Use:   "verify",
    Short: "Run full infrastructure verification",
    Run: func(cmd *cobra.Command, args []string) {
        cfg, err := loadConfig()
        if err != nil { handleError(err); return }
        pool, err := newSSHPool(cfg)
        if err != nil { handleError(err); return }
        defer pool.Close()
        
        lv := newLibvirtClient(cfg)
        
        v := utils.NewVerifier(cfg, pool, lv)
        if err := v.VerifyAll(context.Background()); err != nil {
            handleError(err)
        }
    },
}

func init() {
    rootCmd.AddCommand(verifyCmd)
}
