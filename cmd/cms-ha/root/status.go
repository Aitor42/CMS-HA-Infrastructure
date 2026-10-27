package root

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/utils"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Check status",
}

func init() {
	rootCmd.AddCommand(statusCmd)

	var waitSSH time.Duration
	var asJSON bool
	sshCmd := &cobra.Command{
		Use:   "ssh",
		Short: "Check SSH connectivity to all configured nodes",
		Long: "Check SSH connectivity to all configured nodes.\n\n" +
			"With --wait, block until every node accepts a session (or the timeout\n" +
			"expires) before printing the table. This is the event-driven replacement\n" +
			"for polling `status ssh` with a fixed sleep between attempts.",
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
			if waitSSH > 0 {
				if err := utils.WaitForAllSSH(cmd.Context(), cfg, pool, waitSSH); err != nil {
					return err
				}
			}
			lv := newLibvirtClient(cfg)
			if asJSON {
				results := utils.CollectSSHStatus(cmd.Context(), cfg, pool, lv)
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(results); err != nil {
					return fmt.Errorf("encode SSH status: %w", err)
				}
				for _, res := range results {
					if !res.Healthy {
						return fmt.Errorf("one or more VMs are not healthy or unreachable via SSH")
					}
				}
				return nil
			}
			return utils.CheckSSH(cmd.Context(), cfg, pool, lv)
		},
	}
	sshCmd.Flags().DurationVar(&waitSSH, "wait", 0, "Wait up to this long for SSH on every node before checking (e.g. 10m)")
	sshCmd.Flags().BoolVar(&asJSON, "json", false, "Emit the status rows as JSON")
	statusCmd.AddCommand(sshCmd)
}
