package root

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/initvms"
	"github.com/spf13/cobra"
)

var (
	cleanupVMsOnly      bool
	cleanupNetworksOnly bool
	cleanupYes          bool
)

var cleanupCmd = &cobra.Command{
	Use:     "cleanup",
	Aliases: []string{"destroy"},
	Short:   "Destroy all VMs, disk images, and virtual networks",
	Long:    "Stops and undefines all VMs managed by the cluster, removes their disk images from vm_storage, and tears down the internal and main libvirt virtual networks.",
	RunE: func(cmd *cobra.Command, args []string) error {
		if cleanupVMsOnly && cleanupNetworksOnly {
			return fmt.Errorf("--vms-only and --networks-only are mutually exclusive")
		}
		if !cleanupYes {
			if err := confirmDestructiveCleanup(cmd); err != nil {
				return err
			}
		}

		// Teardown uses the relaxed loader: it must be able to remove whatever
		// exists even if config.yaml no longer describes a complete cluster.
		cfg, err := loadConfigTeardown()
		if err != nil {
			return err
		}
		lv := newLibvirtClient(cfg)

		opts := initvms.CleanupOptions{
			VMsOnly:      cleanupVMsOnly,
			NetworksOnly: cleanupNetworksOnly,
		}
		return initvms.Cleanup(cmd.Context(), cfg, lv, opts)
	},
}

// confirmDestructiveCleanup asks the user to type "yes" before proceeding.
// It aborts on any other input, including EOF from a non-interactive run, so
// that a piped or scripted invocation cannot wipe the infrastructure by accident.
func confirmDestructiveCleanup(cmd *cobra.Command) error {
	fmt.Fprintln(cmd.ErrOrStderr(), "⚠ This will permanently destroy all cluster VMs, their disk images, and virtual networks.")
	fmt.Fprint(cmd.ErrOrStderr(), "Type 'yes' to continue: ")

	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return fmt.Errorf("cleanup aborted: confirmation required (pass --yes to skip in non-interactive runs)")
	}
	if strings.TrimSpace(strings.ToLower(line)) != "yes" {
		return fmt.Errorf("cleanup aborted by user")
	}
	return nil
}

func init() {
	cleanupCmd.Flags().BoolVar(&cleanupVMsOnly, "vms-only", false, "Destroy only VMs and their disks, keeping virtual networks")
	cleanupCmd.Flags().BoolVar(&cleanupNetworksOnly, "networks-only", false, "Destroy only virtual networks")
	cleanupCmd.Flags().BoolVarP(&cleanupYes, "yes", "y", false, "Skip the destructive-action confirmation prompt")
	rootCmd.AddCommand(cleanupCmd)
}
