package root

import (
    "context"

    "github.com/spf13/cobra"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/traffic"
)

var trafficCmd = &cobra.Command{
    Use:   "traffic",
    Short: "Traffic generator",
    Run: func(cmd *cobra.Command, args []string) {
        cfg, err := loadConfig()
        if err != nil { handleError(err); return }
        
        external, _ := cmd.Flags().GetBool("external")
        target, _ := cmd.Flags().GetString("target")
        duration, _ := cmd.Flags().GetInt("duration")
        concurrency, _ := cmd.Flags().GetInt("concurrency")
        withDB, _ := cmd.Flags().GetBool("with-db")
        
        mode := "internal"
        if external { mode = "external" }
        opts := traffic.Options{Mode: mode, TargetIP: target, Duration: duration, Concurrency: concurrency, WithDB: withDB, Verbose: verbose}
        t := traffic.New(cfg, opts)
        if err := t.Run(context.Background()); err != nil { handleError(err) }
    },
}

func init() {
    rootCmd.AddCommand(trafficCmd)
    trafficCmd.Flags().Bool("internal", false, "")
    trafficCmd.Flags().Bool("external", false, "")
    trafficCmd.Flags().String("target", "", "Target IP")
    trafficCmd.Flags().Int("duration", 60, "")
    trafficCmd.Flags().Int("concurrency", 5, "")
    trafficCmd.Flags().Bool("with-db", false, "")
}
