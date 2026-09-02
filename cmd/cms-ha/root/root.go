package root

import (
    "fmt"
    "os"
    "time"
    
    "github.com/spf13/cobra"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

var (
    configPath string
    verbose    bool
    dryRun     bool
)

var rootCmd = &cobra.Command{
    Use:   "cms-ha",
    Short: "CMS-HA Infrastructure CLI",
    Long:  `A unified CLI tool for deploying and managing CMS HA Infrastructure.`,
    PersistentPreRun: func(cmd *cobra.Command, args []string) {
        logging.SetVerbose(verbose)
    },
}

func Execute() error {
    return rootCmd.Execute()
}

func init() {
    rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", "config.yaml", "Path to config file")
    rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose logging")
    rootCmd.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "Enable dry-run mode")
}

// loadConfig loads and returns the configuration.
func loadConfig() (*config.Config, error) {
    cfg, err := config.Load(configPath)
    if err != nil {
        return nil, fmt.Errorf("failed to load config: %w", err)
    }
    return cfg, nil
}

// newSSHPool creates a new SSH connection pool from config.
func newSSHPool(cfg *config.Config) (*ssh.Pool, error) {
    timeout := cfg.SSH.ConnectTimeout
    if timeout == 0 {
        timeout = 10 * time.Second
    }
    pool, err := ssh.NewPool(cfg.SSH.PrivateKey, timeout)
    if err != nil {
        return nil, fmt.Errorf("failed to create SSH pool: %w", err)
    }
    return pool, nil
}

// newLibvirtClient creates a new libvirt client from config.
func newLibvirtClient(cfg *config.Config) *libvirt.Client {
    return libvirt.NewClient(cfg.VM.LibvirtURI)
}

// handleError logs an error and exits.
func handleError(err error) {
    logging.Error("%v", err)
    os.Exit(1)
}
