package root

import (
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/lint"
	"github.com/spf13/cobra"
)

var lintCmd = &cobra.Command{
	Use:   "lint",
	Short: "Lint configurations",
	RunE: func(cmd *cobra.Command, args []string) error {
		all, _ := cmd.Flags().GetBool("all")
		goLint, _ := cmd.Flags().GetBool("go")
		shell, _ := cmd.Flags().GetBool("shell")
		yaml, _ := cmd.Flags().GetBool("yaml")
		puppetF, _ := cmd.Flags().GetBool("puppet")
		k8s, _ := cmd.Flags().GetBool("k8s")
		ansibleLint, _ := cmd.Flags().GetBool("ansible")

		// With no specific flag, run every linter.
		if !goLint && !shell && !yaml && !puppetF && !k8s && !ansibleLint {
			all = true
		}

		opts := lint.Options{
			All:     all,
			Go:      goLint,
			Shell:   shell,
			YAML:    yaml,
			Puppet:  puppetF,
			K8s:     k8s,
			Ansible: ansibleLint,
		}
		return lint.RunLints(cmd.Context(), opts)
	},
}

func init() {
	rootCmd.AddCommand(lintCmd)
	lintCmd.Flags().Bool("all", false, "Run all linters")
	lintCmd.Flags().Bool("go", false, "Lint Go code")
	lintCmd.Flags().Bool("shell", false, "Lint shell scripts")
	lintCmd.Flags().Bool("yaml", false, "Lint YAML files")
	lintCmd.Flags().Bool("puppet", false, "Lint Puppet manifests")
	lintCmd.Flags().Bool("k8s", false, "Lint Kubernetes manifests")
	lintCmd.Flags().Bool("ansible", false, "Lint Ansible playbooks and roles")
}
