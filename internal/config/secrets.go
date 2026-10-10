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

// decrypter decrypts age-encrypted values while reusing a single parsed
// identity, so a config with several encrypted fields reads and parses the age
// key file only once instead of once per field.
type decrypter struct {
	identity age.Identity
}

// newDecrypter reads and parses the age identity at keyPath.
func newDecrypter(keyPath string) (*decrypter, error) {
	cleanedKeyPath := filepath.Clean(ExpandPath(keyPath))
	keyData, err := os.ReadFile(cleanedKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read key file: %w", err)
	}
	identity, err := age.ParseX25519Identity(strings.TrimSpace(string(keyData)))
	if err != nil {
		return nil, fmt.Errorf("failed to parse identity: %w", err)
	}
	return &decrypter{identity: identity}, nil
}

// decrypt decrypts a single "ENC[age,...]" value (returning plaintext as-is).
func (d *decrypter) decrypt(encValue string) (string, error) {
	if !IsEncrypted(encValue) {
		return encValue, nil
	}

	encoded := strings.TrimSuffix(strings.TrimPrefix(encValue, encPrefix), encSuffix)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("failed to decode base64: %w", err)
	}

	r, err := age.Decrypt(bytes.NewReader(decoded), d.identity)
	if err != nil {
		return "", fmt.Errorf("failed to initialize decryption: %w", err)
	}

	out := &bytes.Buffer{}
	// Bound the plaintext size: an "ENC[...]" value is config data, not a
	// data blob; a corrupted/upgrade-sized value should error, not OOM.
	if _, err := io.Copy(out, io.LimitReader(r, maxSecretSize+1)); err != nil {
		return "", fmt.Errorf("failed to read decrypted data: %w", err)
	}
	if out.Len() > maxSecretSize {
		return "", fmt.Errorf("decrypted value exceeds %d KiB limit", maxSecretSize>>10)
	}

	return out.String(), nil
}

// DecryptValue decrypts a string using the key file at keyPath. It is a
// convenience wrapper that re-reads the identity on every call; callers that
// decrypt several values should build a decrypter once with newDecrypter.
func DecryptValue(encValue, keyPath string) (string, error) {
	if !IsEncrypted(encValue) {
		return encValue, nil
	}
	d, err := newDecrypter(keyPath)
	if err != nil {
		return "", err
	}
	return d.decrypt(encValue)
}

// maxSecretSize bounds DecryptValue output (values are passwords/config strings).
const maxSecretSize = 64 * 1024

func processYAMLNode(node *yaml.Node, process func(string) (string, error)) error {
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			valNode := node.Content[i+1]

			if isSecretField(keyNode.Value) {
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

	// Shared-config setups often place a symlink in the repo pointing to the
	// real config: write to the resolved target instead of replacing the link.
	target := cleanedConfigPath
	if info, lerr := os.Lstat(cleanedConfigPath); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
		if resolved, err := filepath.EvalSymlinks(cleanedConfigPath); err == nil {
			target = resolved
		}
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("failed to parse yaml: %w", err)
	}

	err = processYAMLNode(&root, func(s string) (string, error) {
		if strings.TrimSpace(s) == "" {
			return s, nil
		}
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

	tmpFile := target + ".tmp"
	f, err := os.OpenFile(tmpFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to write temporary config: %w", err)
	}
	if _, err := buf.WriteTo(f); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpFile)
		return fmt.Errorf("failed to write temporary config: %w", err)
	}
	// Sync before rename so a crash right after the swap cannot leave the
	// configuration encrypted-under-partially-written or truncated.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpFile)
		return fmt.Errorf("failed to flush temporary config: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("failed to close temporary config: %w", err)
	}
	if err := os.Rename(tmpFile, target); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("failed to replace config: %w", err)
	}

	return nil
}

// LoadAutoEncrypt encrypts non-empty plaintext secret fields in the config
// before loading it. The private age identity is kept in the user's config
// directory, outside the repository.
func LoadAutoEncrypt(configPath string) (*Config, error) {
	path := ExpandPath(configPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config for automatic secret encryption: %w", err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse config for automatic secret encryption: %w", err)
	}
	if !hasPlaintextSecrets(&root) {
		cfg, loadErr := Load(path)
		if loadErr == nil {
			legacyPrivate := ExpandPath("private.key")
			defaultPrivate := ExpandPath("${HOME}/.config/cms-ha/age.key")
			if _, err := os.Stat(defaultPrivate); os.IsNotExist(err) {
				if _, err := os.Stat(legacyPrivate); err == nil {
					migratedPath, err := migrateLegacyIdentity(legacyPrivate)
					if err != nil {
						return nil, err
					}
					return LoadWithKey(path, migratedPath)
				}
			}
			return cfg, nil
		}
		// Older CLI releases used a project-local private.key. Move it outside
		// the repository, keeping it as a secondary identity if age.key differs.
		for _, legacyPath := range []string{
			ExpandPath("${HOME}/.config/cms-ha/legacy.age.key"),
			ExpandPath("private.key"),
		} {
			if _, statErr := os.Stat(legacyPath); statErr != nil {
				continue
			}
			if legacyCfg, legacyErr := LoadWithKey(path, legacyPath); legacyErr == nil {
				if legacyPath == ExpandPath("private.key") {
					migratedPath, err := migrateLegacyIdentity(legacyPath)
					if err != nil {
						return nil, err
					}
					return LoadWithKey(path, migratedPath)
				}
				return legacyCfg, nil
			}
		}
		return nil, loadErr
	}

	privatePath, publicKey, err := ensureAgeKeyPair()
	if err != nil {
		return nil, err
	}
	if err := EncryptConfig(path, publicKey); err != nil {
		return nil, fmt.Errorf("automatically encrypt config secrets: %w", err)
	}
	return LoadWithKey(path, privatePath)
}

func hasPlaintextSecrets(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if isSecretField(key.Value) {
				if value.Kind == yaml.ScalarNode && strings.TrimSpace(value.Value) != "" && !IsEncrypted(value.Value) {
					return true
				}
			}
			if hasPlaintextSecrets(value) {
				return true
			}
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, child := range node.Content {
			if hasPlaintextSecrets(child) {
				return true
			}
		}
	}
	return false
}

func isSecretField(name string) bool {
	switch name {
	case "password", "root_password", "exporter_password", "admin_password", "provisioner_password", "secret_key":
		return true
	default:
		return false
	}
}

func ensureAgeKeyPair() (privatePath, publicKey string, err error) {
	keyDir := ExpandPath("${HOME}/.config/cms-ha")
	privatePath = filepath.Join(keyDir, "age.key")
	publicPath := filepath.Join(keyDir, "age.pub")
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		return "", "", fmt.Errorf("create age key directory: %w", err)
	}
	if err := os.Chmod(keyDir, 0700); err != nil {
		return "", "", fmt.Errorf("secure age key directory: %w", err)
	}

	keyData, readErr := os.ReadFile(privatePath)
	if os.IsNotExist(readErr) {
		// Backward compatibility for installations that used the old project-local key.
		legacyPrivate := ExpandPath("private.key")
		if _, legacyErr := os.Stat(legacyPrivate); legacyErr == nil {
			privatePath, err = migrateLegacyIdentity(legacyPrivate)
			if err != nil {
				return "", "", err
			}
			keyData, readErr = os.ReadFile(privatePath)
		}
	}
	if readErr == nil {
		identity, parseErr := age.ParseX25519Identity(strings.TrimSpace(string(keyData)))
		if parseErr != nil {
			return "", "", fmt.Errorf("parse local age identity: %w", parseErr)
		}
		publicKey = identity.Recipient().String()
		// Only write the recipient file when it is missing: rewriting it on
		// every run aborts the whole load over an unnecessary write failure.
		if _, statErr := os.Stat(publicPath); os.IsNotExist(statErr) {
			if err := os.WriteFile(publicPath, []byte(publicKey+"\n"), 0600); err != nil { // #nosec G703 -- path is the operator's own age recipient file under ${HOME}/.config/cms-ha
				return "", "", fmt.Errorf("write local age recipient: %w", err)
			}
		} else if statErr != nil {
			return "", "", fmt.Errorf("check local age recipient: %w", statErr)
		}
		if err := os.Chmod(privatePath, 0600); err != nil {
			return "", "", fmt.Errorf("secure age identity: %w", err)
		}
		return privatePath, publicKey, nil
	}
	if !os.IsNotExist(readErr) {
		return "", "", fmt.Errorf("read local age identity: %w", readErr)
	}

	publicKey, privateKey, genErr := GenerateKey()
	if genErr != nil {
		return "", "", genErr
	}
	// Claim the private key with O_EXCL so two concurrent processes never
	// generate different identities here (one process would otherwise leave
	// the configuration encrypted under a key that the other one removed).
	f, createErr := os.OpenFile(privatePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if createErr == nil {
		if _, wErr := f.WriteString(privateKey + "\n"); wErr != nil {
			_ = f.Close()
			return "", "", fmt.Errorf("write local age identity: %w", wErr)
		}
		if cErr := f.Close(); cErr != nil {
			return "", "", fmt.Errorf("write local age identity: %w", cErr)
		}
	} else if os.IsExist(createErr) {
		// Another process won the race: adopt the identity that now exists.
		keyData, readErr := os.ReadFile(privatePath)
		if readErr != nil {
			return "", "", fmt.Errorf("read local age identity: %w", readErr)
		}
		identity, parseErr := age.ParseX25519Identity(strings.TrimSpace(string(keyData)))
		if parseErr != nil {
			return "", "", fmt.Errorf("parse local age identity: %w", parseErr)
		}
		publicKey = identity.Recipient().String()
	} else {
		return "", "", fmt.Errorf("write local age identity: %w", createErr)
	}
	if err := os.WriteFile(publicPath, []byte(publicKey+"\n"), 0600); err != nil { // #nosec G703 -- path is the operator's own age recipient file under ${HOME}/.config/cms-ha
		return "", "", fmt.Errorf("write local age recipient: %w", err)
	}
	return privatePath, publicKey, nil
}

func migrateLegacyIdentity(legacyPath string) (string, error) {
	keyDir := ExpandPath("${HOME}/.config/cms-ha")
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		return "", fmt.Errorf("create age key directory: %w", err)
	}
	if err := os.Chmod(keyDir, 0700); err != nil {
		return "", fmt.Errorf("secure age key directory: %w", err)
	}
	keyData, err := os.ReadFile(legacyPath)
	if err != nil {
		return "", fmt.Errorf("read legacy age identity: %w", err)
	}
	if _, err := age.ParseX25519Identity(strings.TrimSpace(string(keyData))); err != nil {
		return "", fmt.Errorf("parse legacy age identity: %w", err)
	}

	primaryPath := filepath.Join(keyDir, "age.key")
	targetPath := primaryPath
	if primaryData, err := os.ReadFile(primaryPath); err == nil && !bytes.Equal(bytes.TrimSpace(primaryData), bytes.TrimSpace(keyData)) {
		targetPath = filepath.Join(keyDir, "legacy.age.key")
	} else if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("read primary age identity: %w", err)
	}

	if existing, err := os.ReadFile(targetPath); err == nil {
		if !bytes.Equal(bytes.TrimSpace(existing), bytes.TrimSpace(keyData)) {
			return "", fmt.Errorf("cannot migrate legacy age identity: %s already contains a different key", targetPath)
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read migrated age identity: %w", err)
	} else if err := os.WriteFile(targetPath, keyData, 0600); err != nil { // #nosec G703 -- targetPath is inside the operator's ${HOME}/.config/cms-ha key directory
		return "", fmt.Errorf("store legacy age identity outside repository: %w", err)
	}
	if err := os.Chmod(targetPath, 0600); err != nil {
		return "", fmt.Errorf("secure migrated age identity: %w", err)
	}
	if err := os.Remove(legacyPath); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("remove migrated legacy age identity: %w", err)
	}
	return targetPath, nil
}

// DecryptConfig reads the config with encrypted fields, decrypts them, and returns Config.
func DecryptConfig(configPath, keyPath string) (*Config, error) {
	if keyPath == "" {
		return Load(configPath)
	}
	return LoadWithKey(configPath, keyPath)
}
