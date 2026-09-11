package root

import (
    "fmt"
    "os"
    "path/filepath"
    "strings"

    "github.com/spf13/cobra"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
)

var secretsCmd = &cobra.Command{
    Use:   "secrets",
    Short: "Manage secrets",
}

func init() {
    rootCmd.AddCommand(secretsCmd)
    
    encryptCmd := &cobra.Command{
        Use: "encrypt",
        Run: func(cmd *cobra.Command, args []string) {
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
                handleError(fmt.Errorf("public key is required: provide --key or place public.key in current directory or ~/.config/cms-ha/public.key"))
                return
            }
            if err := config.EncryptConfig(configPath, pubKey); err != nil { handleError(err); return }
            logging.Success("Configuration file %s encrypted successfully", configPath)
        },
    }
    encryptCmd.Flags().String("key", "", "Public key or path to public key file")
    secretsCmd.AddCommand(encryptCmd)

    decryptCmd := &cobra.Command{
        Use: "decrypt",
        Run: func(cmd *cobra.Command, args []string) {
            keyPath, _ := cmd.Flags().GetString("key")
            decrypted, err := config.DecryptConfig(configPath, keyPath)
            if err != nil { handleError(err); return }
            logging.Success("Configuration file %s decrypted successfully (validated %d node definitions)", configPath, len(decrypted.AllNodes()))
            fmt.Printf("Database: %s (user: %s, password: [MASKED])\n", decrypted.Database.Name, decrypted.Database.User)
            fmt.Printf("PKI Domain: %s (provisioner: [CONFIGURED])\n", decrypted.PKI.Domain)
        },
    }
    decryptCmd.Flags().String("key", "", "Private key path")
    secretsCmd.AddCommand(decryptCmd)

    genKeyCmd := &cobra.Command{
        Use: "generate-key",
        Run: func(cmd *cobra.Command, args []string) {
            pub, priv, err := config.GenerateKey()
            if err != nil { handleError(err); return }
            if err := os.WriteFile("public.key", []byte(pub), 0600); err != nil {
                handleError(fmt.Errorf("failed to write public key: %w", err))
                return
            }
            if err := os.WriteFile("private.key", []byte(priv), 0600); err != nil {
                handleError(fmt.Errorf("failed to write private key: %w", err))
                return
            }
            fmt.Printf("Public Key: %s\n", pub)
            logging.Success("Keys saved securely to public.key (0600) and private.key (0600)")
        },
    }
    secretsCmd.AddCommand(genKeyCmd)
}
