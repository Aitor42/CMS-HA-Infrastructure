package root

import (
	"fmt"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/traffic"
	"github.com/spf13/cobra"
)

var trafficCmd = &cobra.Command{
	Use:   "traffic",
	Short: "k6 traffic generator",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}

		internal, _ := cmd.Flags().GetBool("internal")
		external, _ := cmd.Flags().GetBool("external")
		target, _ := cmd.Flags().GetString("target")
		duration, _ := cmd.Flags().GetInt("duration")
		concurrency, _ := cmd.Flags().GetInt("concurrency")
		withDB, _ := cmd.Flags().GetBool("with-db")
		k6Image, _ := cmd.Flags().GetString("k6-image")

		if internal && external {
			return fmt.Errorf("--internal and --external are mutually exclusive")
		}
		// External mode is the default; --internal selects the other mode and
		// combining both flags is rejected above.
		mode := "external"
		if internal {
			mode = "internal"
		}
		opts := traffic.Options{Mode: mode, TargetIP: target, Duration: duration, Concurrency: concurrency, WithDB: withDB, Verbose: verbose, K6Image: k6Image}
		t := traffic.New(cfg, opts)
		return t.Run(cmd.Context())
	},
}

func init() {
	rootCmd.AddCommand(trafficCmd)
	trafficCmd.Flags().Bool("internal", false, "Generate traffic from the internal network only")
	trafficCmd.Flags().Bool("external", false, "Generate traffic through the load balancer (default)")
	trafficCmd.Flags().String("target", "", "Target IP")
	trafficCmd.Flags().Int("duration", 60, "Test duration in seconds")
	trafficCmd.Flags().Int("concurrency", 5, "Number of k6 virtual users")
	trafficCmd.Flags().Bool("with-db", false, "Include database queries in the test")
	trafficCmd.Flags().String("k6-image", "", "Override the pinned k6 container image (default from config.yaml)")
}
