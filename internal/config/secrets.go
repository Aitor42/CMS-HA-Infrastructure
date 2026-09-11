package config

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"gopkg.in/yaml.v3"
)

const encPrefix = "ENC[age,"
const encSuffix = "]"

// GenerateKey generates a new age X25519 keypair.
func GenerateKey() (publicKey string, privateKey string, err error) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return "", "", fmt.Errorf("failed to generate identity: %w", err)
	}
	return identity.Recipient().String(), identity.String(), nil
}

// IsEncrypted checks if a value is age-encrypted.
func IsEncrypted(value string) bool {
	return strings.HasPrefix(value, encPrefix) && strings.HasSuffix(value, encSuffix)
}

// EncryptValue encrypts a string using the provided public key.
func EncryptValue(value, publicKey string) (string, error) {
	recipient, err := age.ParseX25519Recipient(publicKey)
	if err != nil {
		return "", fmt.Errorf("invalid public key: %w", err)
	}

	buf := &bytes.Buffer{}
	w, err := age.Encrypt(buf, recipient)
	if err != nil {
		return "", fmt.Errorf("failed to initialize encryption: %w", err)
	}
	if _, err := io.WriteString(w, value); err != nil {
		return "", fmt.Errorf("failed to write value: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("failed to close encryption: %w", err)
	}

	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())
	return encPrefix + encoded + encSuffix, nil
}

// DecryptValue decrypts a string using the key file at keyPath.
func DecryptValue(encValue, keyPath string) (string, error) {
	if !IsEncrypted(encValue) {
		return encValue, nil
	}

	encoded := strings.TrimSuffix(strings.TrimPrefix(encValue, encPrefix), encSuffix)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("failed to decode base64: %w", err)
	}

	cleanedKeyPath := filepath.Clean(ExpandPath(keyPath))
	keyData, err := os.ReadFile(cleanedKeyPath)
	if err != nil {
		return "", fmt.Errorf("failed to read key file: %w", err)
	}

	identity, err := age.ParseX25519Identity(strings.TrimSpace(string(keyData)))
	if err != nil {
		return "", fmt.Errorf("failed to parse identity: %w", err)
	}

	r, err := age.Decrypt(bytes.NewReader(decoded), identity)
	if err != nil {
		return "", fmt.Errorf("failed to initialize decryption: %w", err)
	}

	out := &bytes.Buffer{}
	if _, err := io.Copy(out, r); err != nil {
		return "", fmt.Errorf("failed to read decrypted data: %w", err)
	}

	return out.String(), nil
}

func processYAMLNode(node *yaml.Node, process func(string) (string, error)) error {
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			valNode := node.Content[i+1]
			
			if keyNode.Value == "password" || keyNode.Value == "root_password" || keyNode.Value == "provisioner_password" {
				if valNode.Kind == yaml.ScalarNode {
					newVal, err := process(valNode.Value)
					if err != nil {
						return err
					}
					valNode.Value = newVal
				}
			} else {
				if err := processYAMLNode(valNode, process); err != nil {
					return err
				}
			}
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			if err := processYAMLNode(child, process); err != nil {
				return err
			}
		}
	case yaml.DocumentNode:
		for _, child := range node.Content {
			if err := processYAMLNode(child, process); err != nil {
				return err
			}
		}
	}
	return nil
}

// EncryptConfig reads the config file, encrypts sensitive fields, and writes it back.
func EncryptConfig(configPath, publicKey string) error {
	cleanedConfigPath := filepath.Clean(ExpandPath(configPath))
	data, err := os.ReadFile(cleanedConfigPath)
	if err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("failed to parse yaml: %w", err)
	}

	err = processYAMLNode(&root, func(s string) (string, error) {
		if IsEncrypted(s) {
			return s, nil
		}
		return EncryptValue(s, publicKey)
	})
	if err != nil {
		return err
	}

	buf := &bytes.Buffer{}
	encoder := yaml.NewEncoder(buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&root); err != nil {
		return fmt.Errorf("failed to encode yaml: %w", err)
	}

	if err := os.WriteFile(cleanedConfigPath, buf.Bytes(), 0600); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}

	return nil
}

// DecryptConfig reads the config with encrypted fields, decrypts them, and returns Config.
func DecryptConfig(configPath, keyPath string) (*Config, error) {
	if keyPath == "" {
		return Load(configPath)
	}
	return LoadWithKey(configPath, keyPath)
}
