package root

import (
    "fmt"
    "os"
    "github.com/spf13/cobra"
    "github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
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
            keyPath, _ := cmd.Flags().GetString("key")
            if err := config.EncryptConfig(configPath, keyPath); err != nil { handleError(err) }
        },
    }
    encryptCmd.Flags().String("key", "", "Public key path")
    secretsCmd.AddCommand(encryptCmd)

    decryptCmd := &cobra.Command{
        Use: "decrypt",
        Run: func(cmd *cobra.Command, args []string) {
            keyPath, _ := cmd.Flags().GetString("key")
            decrypted, err := config.DecryptConfig(configPath, keyPath)
            if err != nil { handleError(err); return }
            fmt.Printf("%+v\n", decrypted)
        },
    }
    decryptCmd.Flags().String("key", "", "Private key path")
    secretsCmd.AddCommand(decryptCmd)

    genKeyCmd := &cobra.Command{
        Use: "generate-key",
        Run: func(cmd *cobra.Command, args []string) {
            pub, priv, err := config.GenerateKey()
            if err != nil { handleError(err); return }
            fmt.Printf("Public Key: %s\nPrivate Key: %s\n", pub, priv)
            os.WriteFile("public.key", []byte(pub), 0644)
            os.WriteFile("private.key", []byte(priv), 0600)
            fmt.Println("Keys saved to public.key and private.key")
        },
    }
    secretsCmd.AddCommand(genKeyCmd)
}
