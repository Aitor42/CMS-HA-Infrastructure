package root

import (
	"os"
	"path/filepath"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ansible"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/spf13/cobra"
)

var (
	inventoryOut    string
	inventoryStdout bool
)

// inventoryCmd renders the Ansible inventory from config.yaml. The inventory
// is generated (never hand-edited) so config.yaml stays the single source of
// truth for the provisioning layer.
var inventoryCmd = &cobra.Command{
	Use:   "inventory",
	Short: "Generate the Ansible inventory from config.yaml",
	Long: `Render the Ansible inventory consumed by the provisioning playbook.

Paths are resolved relative to the configuration file, so the inventory is
always written inside the repository regardless of the working directory.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		data, err := ansible.Marshal(cfg)
		if err != nil {
			return err
		}

		if inventoryStdout {
			_, err := cmd.OutOrStdout().Write(data)
			return err
		}

		out := inventoryOut
		if !filepath.IsAbs(out) {
			out = filepath.Join(filepath.Dir(configPath), out)
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(out, data, 0o644); err != nil {
			return err
		}
		logging.Success("Ansible inventory written to %s", out)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(inventoryCmd)
	inventoryCmd.Flags().StringVarP(&inventoryOut, "output", "o", ansible.DefaultInventoryPath,
		"Inventory output path (relative to the config file)")
	inventoryCmd.Flags().BoolVar(&inventoryStdout, "stdout", false,
		"Print the inventory to stdout instead of writing a file")
}
