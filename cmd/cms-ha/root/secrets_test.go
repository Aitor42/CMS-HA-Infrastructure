package root

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
)

func TestSecretsRotate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Cleanup(func() {
		configPath = defaultConfigPath
		rootCmd.SetArgs(nil)
	})

	oldPublic, oldPrivate, err := config.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	keyDir := filepath.Join(home, ".config", "cms-ha")
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		t.Fatalf("create key dir: %v", err)
	}
	keyPath := filepath.Join(keyDir, "age.key")
	if err := os.WriteFile(keyPath, []byte(oldPrivate), 0600); err != nil {
		t.Fatalf("write old key: %v", err)
	}

	configPath = filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("database:\n  user: cms\n  password: db-secret\n"), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	rootCmd.SetArgs([]string{"-c", configPath, "secrets", "rotate"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("secrets rotate: %v", err)
	}

	backups, err := filepath.Glob(configPath + ".bak-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("config backups = %v, %v; want exactly one", backups, err)
	}
	keyBackups, err := filepath.Glob(keyPath + ".bak-*")
	if err != nil || len(keyBackups) != 1 {
		t.Fatalf("key backups = %v, %v; want exactly one", keyBackups, err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat new key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("new key mode = %04o, want 0600", perm)
	}
	newPrivate, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read new key: %v", err)
	}
	cfg, err := config.LoadWithKey(configPath, keyPath)
	if err != nil {
		t.Fatalf("load rekeyed config: %v", err)
	}
	if cfg.Database.Password != "db-secret" {
		t.Fatalf("decrypted password = %q", cfg.Database.Password)
	}
	if _, err := config.LoadWithKey(configPath, keyBackups[0]); err == nil {
		t.Fatal("old key still decrypts the rekeyed config")
	}
	newPublic, err := config.PublicKeyFromPrivate(string(newPrivate))
	if err != nil {
		t.Fatalf("parse new key: %v", err)
	}
	if newPublic == oldPublic {
		t.Fatal("age identity was not rotated")
	}
}
