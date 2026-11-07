package root

import (
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/deploy"
	"github.com/spf13/cobra"
)

var (
	skipVMCreate     bool
	fromPhase        string
	resume           bool
	stateFile        string
	timingsFile      string
	runID            string
	fastProvisioning bool
)

var deployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Deploy infrastructure",
	RunE: func(cmd *cobra.Command, args []string) error {
		load := loadConfig
		if dryRun {
			load = loadConfigReadOnly
		}
		cfg, err := load()
		if err != nil {
			return err
		}
		pool, err := newSSHPool(cfg)
		if err != nil {
			return err
		}
		defer pool.Close()

		lv := newLibvirtClient(cfg)

		orchestrator := &deploy.Orchestrator{
			Cfg:     cfg,
			Libvirt: lv,
			SSH:     pool,
		}

		opts := deploy.DeployOpts{
			SkipVMCreate:     skipVMCreate,
			DryRun:           dryRun, // from root.go
			FromPhase:        fromPhase,
			Resume:           resume,
			StateFile:        stateFile,
			TimingsFile:      timingsFile,
			RunID:            runID,
			ConfigFile:       configPath,
			FastProvisioning: fastProvisioning,
		}

		return orchestrator.Deploy(cmd.Context(), opts)
	},
}

func init() {
	deployCmd.Flags().BoolVar(&skipVMCreate, "skip-vm-create", false, "Skip VM creation")
	deployCmd.Flags().StringVar(&fromPhase, "from-phase", "", "Run from this phase onwards (skips earlier phases)")
	deployCmd.Flags().BoolVar(&resume, "resume", false, "Skip the phases already completed in the checkpoint file")
	deployCmd.Flags().StringVar(&stateFile, "state-file", deploy.DefaultStateFile, "Checkpoint file recording phase progress")
	deployCmd.Flags().StringVar(&timingsFile, "timings-file", "", "Write per-phase timing data as JSON to this file")
	deployCmd.Flags().StringVar(&runID, "run-id", "", "Identifier for this run (default: generated from UTC time)")
	deployCmd.Flags().BoolVar(&fastProvisioning, "fast-provisioning", false, "Install the jumpstart and client nodes from the cloud image (opt-in fast path; PXE stays the default)")
	rootCmd.AddCommand(deployCmd)
}
