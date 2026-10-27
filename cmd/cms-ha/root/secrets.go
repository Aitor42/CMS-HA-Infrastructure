package root

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
		Short: "Verify that config.yaml secrets decrypt (read-only)",
		Long: `Load config.yaml in memory with its secrets decrypted and print a short summary.

The file on disk is never modified: plaintext secrets are re-encrypted
automatically on the next load, so writing them out would only weaken the
at-rest protection without changing the validated configuration.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			keyPath, _ := cmd.Flags().GetString("key")
			decrypted, err := config.DecryptConfig(configPath, keyPath)
			if err != nil {
				return err
			}
			logging.Success("Configuration file %s loaded with secrets decrypted (read-only; validated %d node definitions)", configPath, len(decrypted.AllNodes()))
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

	rotateCmd := &cobra.Command{
		Use:   "rotate",
		Short: "Rotate the age identity and re-encrypt config.yaml",
		Long: `Rotate the age identity used for config.yaml and re-encrypt every secret field.

The config file is backed up first. With no --new-key, a new keypair is
generated and installed at ~/.config/cms-ha/age.key (the previous key is kept
as age.key.bak-<timestamp>). With --new-key, an existing private key is used and
the key files are left untouched.

This rotates encryption at rest, not the service passwords themselves. Rotate
database/WordPress/CA credentials with their own procedures after rekeying.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			oldKeyFlag, _ := cmd.Flags().GetString("key")
			newKeyFlag, _ := cmd.Flags().GetString("new-key")

			oldKeyPath, err := resolveAgeKey(oldKeyFlag)
			if err != nil {
				return err
			}
			oldKeyData, err := os.ReadFile(oldKeyPath)
			if err != nil {
				return fmt.Errorf("read current age key: %w", err)
			}
			oldPublic, err := config.PublicKeyFromPrivate(string(oldKeyData))
			if err != nil {
				return fmt.Errorf("parse current age key: %w", err)
			}

			var newPublic, newPrivate string
			generated := strings.TrimSpace(newKeyFlag) == ""
			if generated {
				newPublic, newPrivate, err = config.GenerateKey()
				if err != nil {
					return err
				}
			} else {
				newKeyPath := config.ExpandPath(newKeyFlag)
				if info, statErr := os.Stat(newKeyPath); statErr == nil && info.Mode().Perm()&0o077 != 0 {
					return fmt.Errorf("new age key %s has mode %04o; chmod 600 before use", newKeyPath, info.Mode().Perm())
				}
				data, readErr := os.ReadFile(newKeyPath)
				if readErr != nil {
					return fmt.Errorf("read new age key: %w", readErr)
				}
				newPrivate = strings.TrimSpace(string(data))
				newPublic, err = config.PublicKeyFromPrivate(newPrivate)
				if err != nil {
					return fmt.Errorf("parse new age key: %w", err)
				}
			}
			if newPublic == oldPublic {
				return fmt.Errorf("new age key is identical to the current key; refusing a no-op rotation")
			}

			backupPath := configPath + ".bak-" + rotationStamp()
			configData, err := os.ReadFile(configPath)
			if err != nil {
				return fmt.Errorf("read config before rotation: %w", err)
			}
			if err := os.WriteFile(backupPath, configData, 0600); err != nil {
				return fmt.Errorf("back up config to %s: %w", backupPath, err)
			}

			var pendingKeyPath string
			if generated {
				pendingKeyPath, err = writePendingAgeKey(newPrivate)
				if err != nil {
					return err
				}
			}
			if err := config.RekeyConfig(configPath, oldKeyPath, newPublic); err != nil {
				if pendingKeyPath != "" {
					_ = os.Remove(pendingKeyPath)
				}
				return fmt.Errorf("re-encrypt config (backup at %s): %w", backupPath, err)
			}

			if generated {
				installed, oldBackup, installErr := installPendingAgeKey(pendingKeyPath, newPublic)
				if installErr != nil {
					return fmt.Errorf("config was re-encrypted but installing the new key failed (new key: %s): %w", pendingKeyPath, installErr)
				}
				logging.Success("Rotated age identity to %s", installed)
				if oldBackup != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "Old age key backup: %s\n", oldBackup)
				}
			} else {
				logging.Success("Re-encrypted %s with the public key from %s", configPath, newKeyFlag)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Config backup: %s\n", backupPath)
			return nil
		},
	}
	rotateCmd.Flags().String("key", "", "Current private age key (default: ~/.config/cms-ha/age.key or ./private.key)")
	rotateCmd.Flags().String("new-key", "", "Existing private age key to rotate to (default: generate a new keypair)")
	secretsCmd.AddCommand(rotateCmd)
}

func rotationStamp() string {
	return time.Now().UTC().Format("20060102T150405.000000000Z")
}

func resolveAgeKey(explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return config.ExpandPath(explicit), nil
	}
	for _, candidate := range []string{
		config.ExpandPath("${HOME}/.config/cms-ha/age.key"),
		config.ExpandPath("private.key"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no age key found: pass --key or create ~/.config/cms-ha/age.key")
}

func writePendingAgeKey(privateKey string) (string, error) {
	keyDir := config.ExpandPath("${HOME}/.config/cms-ha")
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		return "", fmt.Errorf("create age key directory: %w", err)
	}
	if err := os.Chmod(keyDir, 0700); err != nil {
		return "", fmt.Errorf("secure age key directory: %w", err)
	}
	f, err := os.CreateTemp(keyDir, ".age.key.pending-*")
	if err != nil {
		return "", fmt.Errorf("create pending age key: %w", err)
	}
	path := f.Name()
	if _, err := f.WriteString(strings.TrimSpace(privateKey) + "\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("write pending age key: %w", err)
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("secure pending age key: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("flush pending age key: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close pending age key: %w", err)
	}
	return path, nil
}

func installPendingAgeKey(pendingPath, publicKey string) (installed, backup string, err error) {
	keyDir := config.ExpandPath("${HOME}/.config/cms-ha")
	defaultKey := filepath.Join(keyDir, "age.key")
	if _, statErr := os.Stat(defaultKey); statErr == nil {
		backup = defaultKey + ".bak-" + rotationStamp()
		if err := os.Rename(defaultKey, backup); err != nil {
			return pendingPath, "", fmt.Errorf("back up old age key to %s: %w", backup, err)
		}
	} else if !os.IsNotExist(statErr) {
		return pendingPath, "", fmt.Errorf("stat old age key: %w", statErr)
	}
	if err := os.Rename(pendingPath, defaultKey); err != nil {
		if backup != "" {
			_ = os.Rename(backup, defaultKey)
		}
		return pendingPath, backup, fmt.Errorf("install new age key: %w", err)
	}
	if err := os.Chmod(defaultKey, 0600); err != nil {
		return defaultKey, backup, fmt.Errorf("secure installed age key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(keyDir, "age.pub"), []byte(publicKey+"\n"), 0600); err != nil {
		return defaultKey, backup, fmt.Errorf("write age public key: %w", err)
	}
	return defaultKey, backup, nil
}
