package root

import (
	"encoding/json"
	"fmt"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/spf13/cobra"
)

type configValidation struct {
	Valid          bool   `json:"valid"`
	Config         string `json:"config"`
	Nodes          int    `json:"nodes,omitempty"`
	Masters        int    `json:"masters,omitempty"`
	Workers        int    `json:"workers,omitempty"`
	JumpstartImage string `json:"jumpstart_image,omitempty"`
	Provisioning   string `json:"provisioning,omitempty"`
	Error          string `json:"error,omitempty"`
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Inspect and validate the configuration",
}

func init() {
	rootCmd.AddCommand(configCmd)

	var asJSON bool
	validateCmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate config.yaml without deploying anything",
		Long: "Load the configuration, apply the same full validation the deploy path uses,\n" +
			"and exit non-zero if it is invalid. With --json the result is a stable object\n" +
			"suitable for CI or other automation; no infrastructure is touched.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			result := configValidation{Valid: err == nil, Config: configPath}
			if err != nil {
				result.Error = err.Error()
				if asJSON {
					if encErr := json.NewEncoder(cmd.OutOrStdout()).Encode(result); encErr != nil {
						return fmt.Errorf("encode validation result: %w", encErr)
					}
				}
				return err
			}

			result.Nodes = len(cfg.AllNodes())
			result.Masters = len(cfg.Nodes.Masters)
			result.Workers = len(cfg.Nodes.Workers)
			result.JumpstartImage = cfg.VM.JumpstartImage
			result.Provisioning = cfg.VM.Provisioning
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			logging.Success("Configuration %s is valid (%d nodes: %d masters, %d workers; jumpstart=%s, provisioning=%s)",
				configPath, result.Nodes, result.Masters, result.Workers, result.JumpstartImage, result.Provisioning)
			return nil
		},
	}
	validateCmd.Flags().BoolVar(&asJSON, "json", false, "Emit a machine-readable JSON result")
	configCmd.AddCommand(validateCmd)
}
