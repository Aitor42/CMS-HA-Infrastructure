package root

import (
	"context"

	"github.com/spf13/cobra"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/lint"
)

var lintCmd = &cobra.Command{
	Use:   "lint",
	Short: "Lint configurations",
	Run: func(cmd *cobra.Command, args []string) {
		all, _       := cmd.Flags().GetBool("all")
		goLint, _    := cmd.Flags().GetBool("go")
		shell, _     := cmd.Flags().GetBool("shell")
		yaml, _      := cmd.Flags().GetBool("yaml")
		puppetF, _   := cmd.Flags().GetBool("puppet")
		k8s, _       := cmd.Flags().GetBool("k8s")
		terraform, _ := cmd.Flags().GetBool("terraform")

		// If no specific flag, default to all
		if !goLint && !shell && !yaml && !puppetF && !k8s && !terraform {
			all = true
		}

		opts := lint.Options{
			All:       all,
			Go:        goLint,
			Shell:     shell,
			YAML:      yaml,
			Puppet:    puppetF,
			K8s:       k8s,
			Terraform: terraform,
		}
		if err := lint.RunLints(context.Background(), opts); err != nil {
			handleError(err)
		}
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
	lintCmd.Flags().Bool("terraform", false, "Lint Terraform configs")
}
