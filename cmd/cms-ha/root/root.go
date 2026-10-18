package root

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
	"github.com/spf13/cobra"
)

var (
	configPath string
	verbose    bool
	dryRun     bool
)

var rootCmd = &cobra.Command{
	Use:           "cms-ha",
	Short:         "CMS-HA Infrastructure CLI",
	Long:          `A unified CLI tool for deploying and managing CMS HA Infrastructure.`,
	SilenceErrors: true,
	SilenceUsage:  true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		logging.SetVerbose(verbose)
		// Only the deploy command implements dry-run; any other command that
		// accepts the flag silently would perform real changes while the
		// user believes they are in simulation mode.
		if dryRun && cmd != deployCmd {
			return fmt.Errorf("--dry-run is only implemented by 'cms-ha deploy'")
		}
		return nil
	},
}

// ExecuteContext executes the root command with the given context.
func ExecuteContext(ctx context.Context) error {
	return rootCmd.ExecuteContext(ctx)
}

// Execute executes the root command with graceful signal handling.
func Execute() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return ExecuteContext(ctx)
}

// GetRootCmd returns the root cobra command for testing and introspection.
func GetRootCmd() *cobra.Command {
	return rootCmd
}

// defaultConfigPath is the fallback config path. It drives which CLI
// invocations get directory-based fallback lookup (see loadConfig below).
const defaultConfigPath = "config.yaml"

func init() {
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", defaultConfigPath, "Path to config file")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose logging")
	rootCmd.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "Enable dry-run mode")
}

// loadConfig loads, validates and returns the configuration.
//
// Directory-based fallback lookup ("../config.yaml") is only applied when the
// default path is used, so the tool still works when run from a subdirectory
// of the repo. A user-provided path that does not exist is an error: silently
// deploying against a different configuration is not acceptable.
func loadConfig() (*config.Config, error) {
	return loadConfigWith(func(cfg *config.Config) error { return cfg.Validate() })
}

// loadConfigPartial is loadConfig without the full deployment validation, for
// commands that operate on whatever resources are present (cleanup/destroy,
// VM lifecycle, status and repair). Those must work even when the topology in
// config.yaml is incomplete or temporarily broken (for example a node was
// removed). Only the fields that steer file deletion, libvirt lookups and node
// identification are still checked, so whatever is actually present can be
// managed or removed.
func loadConfigPartial() (*config.Config, error) {
	return loadConfigWith(func(cfg *config.Config) error { return cfg.ValidatePartial() })
}

func loadConfigWith(validate func(*config.Config) error) (*config.Config, error) {
	expanded := config.ExpandPath(configPath)
	if _, err := os.Stat(expanded); err == nil {
		configPath = expanded
	} else if os.IsNotExist(err) {
		if expanded != config.ExpandPath(defaultConfigPath) {
			return nil, fmt.Errorf("config file not found: %s", expanded)
		}
		candidates := []string{
			"config.yaml",
			"../config.yaml",
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				configPath = c
				break
			}
		}
	}
	cfg, err := config.LoadAutoEncrypt(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("invalid configuration %s: %w", configPath, err)
	}
	return cfg, nil
}

// newSSHPool creates a new SSH connection pool from config.
func newSSHPool(cfg *config.Config) (*ssh.Pool, error) {
	timeout := cfg.SSH.ConnectTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	pool, err := ssh.NewPoolWithOptions(cfg.SSH.PrivateKey, ssh.Options{
		User:           cfg.SSHUser(),
		KnownHostsFile: cfg.SSHKnownHostsFile(),
		ConnectTimeout: timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create SSH pool: %w", err)
	}
	return pool, nil
}

// newLibvirtClient creates a new libvirt client from config.
func newLibvirtClient(cfg *config.Config) *libvirt.Client {
	return libvirt.NewClient(cfg.VM.LibvirtURI)
}
