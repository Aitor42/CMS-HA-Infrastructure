package root

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/spf13/cobra"
)

var secretsCmd = &cobra.Command{
	Use:   "secrets",
	Short: "Manage secrets",
}

func init() {
	rootCmd.AddCommand(secretsCmd)

	encryptCmd := &cobra.Command{
		Use:   "encrypt",
		Short: "Encrypt plaintext secrets in config.yaml in place",
		RunE: func(cmd *cobra.Command, args []string) error {
			keyInput, _ := cmd.Flags().GetString("key")
			pubKey := strings.TrimSpace(keyInput)
			if keyInput != "" {
				if data, err := os.ReadFile(filepath.Clean(keyInput)); err == nil {
					pubKey = strings.TrimSpace(string(data))
				}
			} else {
				if data, err := os.ReadFile("public.key"); err == nil {
					pubKey = strings.TrimSpace(string(data))
				} else if data, err := os.ReadFile(config.ExpandPath("${HOME}/.config/cms-ha/public.key")); err == nil {
					pubKey = strings.TrimSpace(string(data))
				}
			}
			if pubKey == "" {
				return fmt.Errorf("public key is required: provide --key or place public.key in current directory or ~/.config/cms-ha/public.key")
			}
			if err := config.EncryptConfig(configPath, pubKey); err != nil {
				return err
			}
			logging.Success("Configuration file %s encrypted successfully", configPath)
			return nil
		},
	}
	encryptCmd.Flags().String("key", "", "Public key or path to public key file")
	secretsCmd.AddCommand(encryptCmd)

	decryptCmd := &cobra.Command{
		Use:   "decrypt",
		Short: "Decrypt config.yaml secrets in place for editing",
		RunE: func(cmd *cobra.Command, args []string) error {
			keyPath, _ := cmd.Flags().GetString("key")
			decrypted, err := config.DecryptConfig(configPath, keyPath)
			if err != nil {
				return err
			}
			logging.Success("Configuration file %s decrypted successfully (validated %d node definitions)", configPath, len(decrypted.AllNodes()))
			fmt.Fprintf(cmd.OutOrStdout(), "Database: %s (user: %s, password: [MASKED])\n", decrypted.Database.Name, decrypted.Database.User)
			fmt.Fprintf(cmd.OutOrStdout(), "PKI Domain: %s (provisioner: [CONFIGURED])\n", decrypted.PKI.Domain)
			return nil
		},
	}
	decryptCmd.Flags().String("key", "", "Private key path")
	secretsCmd.AddCommand(decryptCmd)

	genKeyCmd := &cobra.Command{
		Use:   "generate-key",
		Short: "Generate an age keypair for config secret encryption",
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			// Regenerating keys invalidates every secret that was encrypted
			// with the previous public key: refuse to clobber them silently.
			for _, keyFile := range []string{"public.key", "private.key"} {
				if _, err := os.Stat(keyFile); err == nil && !force {
					return fmt.Errorf("%s already exists: regenerating the keypair makes all previously encrypted secrets unrecoverable (use --force to overwrite)", keyFile)
				}
			}
			pub, priv, err := config.GenerateKey()
			if err != nil {
				return err
			}
			if err := os.WriteFile("public.key", []byte(pub), 0600); err != nil {
				return fmt.Errorf("failed to write public key: %w", err)
			}
			if err := os.WriteFile("private.key", []byte(priv), 0600); err != nil {
				return fmt.Errorf("failed to write private key: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Public Key: %s\n", pub)
			logging.Success("Keys saved securely to public.key (0600) and private.key (0600)")
			return nil
		},
	}
	genKeyCmd.Flags().Bool("force", false, "Overwrite existing key files (invalidates encrypted secrets)")
	secretsCmd.AddCommand(genKeyCmd)
}
