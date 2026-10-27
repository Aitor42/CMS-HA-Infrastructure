package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Load loads the configuration from the given path using default age key location.
func Load(path string) (*Config, error) {
	return LoadWithKey(path, "")
}

// LoadWithKey loads configuration from the given path using a specified age key file.
func LoadWithKey(path string, keyPath string) (*Config, error) {
	path = ExpandPath(path)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	c.VM.StorageDir = ExpandPath(c.VM.StorageDir)
	c.SSH.PrivateKey = ExpandPath(c.SSH.PrivateKey)

	// NUM_HOTDESKS overrides the configured hotdesk count at runtime.
	if envHotdesks := os.Getenv("NUM_HOTDESKS"); envHotdesks != "" {
		if val, err := strconv.Atoi(envHotdesks); err == nil && val > 0 {
			c.Nodes.Hotdesks.Count = val
		} else {
			slog.Warn("ignoring invalid NUM_HOTDESKS value", "value", envHotdesks)
		}
	}

	resolvedKeyPath := keyPath
	if resolvedKeyPath == "" {
		defaultKey := ExpandPath("${HOME}/.config/cms-ha/age.key")
		if _, err := os.Stat(defaultKey); err == nil {
			resolvedKeyPath = defaultKey
		} else if _, err := os.Stat("private.key"); err == nil {
			resolvedKeyPath = "private.key"
		} else {
			resolvedKeyPath = defaultKey
		}
	} else {
		resolvedKeyPath = ExpandPath(resolvedKeyPath)
	}

	hasEncrypted := IsEncrypted(c.Database.Password) ||
		IsEncrypted(c.Database.RootPassword) ||
		IsEncrypted(c.Database.ExporterPassword) ||
		IsEncrypted(c.WordPress.AdminPassword) ||
		IsEncrypted(c.PKI.ProvisionerPassword) ||
		IsEncrypted(c.Backup.SecretKey)

	if hasEncrypted {
		if _, err := os.Stat(resolvedKeyPath); err != nil {
			return nil, fmt.Errorf("configuration contains encrypted fields but key file %s does not exist: %w", resolvedKeyPath, err)
		}

		d, err := newDecrypter(resolvedKeyPath)
		if err != nil {
			return nil, err
		}
		secrets := []struct {
			value *string
			label string
		}{
			{&c.Database.Password, "db password"},
			{&c.Database.RootPassword, "db root password"},
			{&c.Database.ExporterPassword, "db exporter password"},
			{&c.WordPress.AdminPassword, "WordPress admin password"},
			{&c.PKI.ProvisionerPassword, "pki provisioner password"},
			{&c.Backup.SecretKey, "backup secret key"},
		}
		for _, s := range secrets {
			if !IsEncrypted(*s.value) {
				continue
			}
			v, err := d.decrypt(*s.value)
			if err != nil {
				return nil, fmt.Errorf("decrypt %s: %w", s.label, err)
			}
			*s.value = v
		}
	}

	return &c, nil
}
