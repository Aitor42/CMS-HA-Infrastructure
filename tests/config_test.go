package tests

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
)

func TestConfig_LoadAndNodeSpecs(t *testing.T) {
	yamlContent := `
vm:
  storage_dir: "~/vm_storage"
  libvirt_uri: "qemu:///system"

ssh:
  private_key: "~/.ssh/id_ed25519"
  connect_timeout: 10s
  batch_mode: true

network:
  internal:
    cidr: "192.168.10.0/24"
    gateway: "192.168.10.1"
    domain: "internal.local"
    bridge: "virbr-int"
  main:
    cidr: "192.168.20.0/24"
    gateway: "192.168.20.1"
    domain: "main.local"
    bridge: "virbr-main"

nodes:
  router:
    name: "ufw-router"
    ip: "192.168.10.1"
    mac: "52:54:00:10:01:02"
    ram_install_mb: 3072
    ram_final_mb: 768
    vcpus: 1
    disk_gb: 5
  jumpstart:
    name: "jumpstart"
    ip: "192.168.10.10"
    ip_main: "192.168.20.10"
    mac_internal: "52:54:00:10:00:01"
    mac_main: "52:54:00:10:02:0a"
    ram_install_mb: 3072
    ram_final_mb: 2048
    vcpus: 2
    disk_gb: 30
  masters:
    - name: "internal-master1"
      ip: "192.168.10.11"
      mac: "52:54:00:10:01:11"
      ram_install_mb: 4096
      ram_final_mb: 1024
      vcpus: 1
      disk_gb: 8
      drbd_disk_gb: 3
    - name: "internal-master2"
      ip: "192.168.10.12"
      mac: "52:54:00:10:01:12"
      ram_install_mb: 4096
      ram_final_mb: 1024
      vcpus: 1
      disk_gb: 8
      drbd_disk_gb: 3
  workers:
    - name: "internal-worker1"
      ip: "192.168.10.13"
      mac: "52:54:00:10:01:13"
      ram_install_mb: 4096
      ram_final_mb: 768
      vcpus: 1
      disk_gb: 8
  storage:
    name: "internal-storage"
    ip: "192.168.10.15"
    mac: "52:54:00:10:01:15"
    ram_install_mb: 3072
    ram_final_mb: 1024
    vcpus: 1
    disk_gb: 8
  monitor:
    name: "internal-monitor"
    ip: "192.168.10.20"
    mac: "52:54:00:10:01:10"
    ram_install_mb: 3072
    ram_final_mb: 1024
    vcpus: 1
    disk_gb: 4
  lb:
    name: "main-lb"
    ip: "192.168.20.100"
    mac: "52:54:00:10:02:64"
    ram_install_mb: 3072
    ram_final_mb: 768
    vcpus: 1
    disk_gb: 4
  cms_frontends:
    - name: "main-cms1"
      ip: "192.168.20.101"
      mac: "52:54:00:10:02:65"
      ram_install_mb: 3072
      ram_final_mb: 1024
      vcpus: 1
      disk_gb: 4
  hotdesks:
    count: 2
    max: 8
    ram_install_mb: 3072
    ram_final_mb: 768
    vcpus: 1
    disk_gb: 3
    base_ip_octet: 201

database:
  name: "wordpress"
  user: "wp_user"
  password: "WpS3cur3P4ss!"
  root_password: "mysqlrootpass"
  port: 30306

monitoring:
  exporters:
    nginx_version: "1.4.1"
    apache_version: "1.0.9"
    mysqld_version: "0.16.0"

pki:
  ca_port: 8443
  provisioner_password: "StepCA-Pr0v1s10ner!"
  step_version: "0.27.5"
  step_ca_version: "0.27.5"
  domain: "ca.internal.local"

deploy:
  ssh_timeout: 600s
  stagger_delay: 5s
`
	tmpFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmpFile, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := config.Load(tmpFile)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if cfg.Database.Name != "wordpress" {
		t.Errorf("expected DB name wordpress, got %s", cfg.Database.Name)
	}

	// Verify path expansion (no raw ~)
	if cfg.VM.StorageDir == "~/vm_storage" {
		t.Errorf("expected tilde expansion in StorageDir, got %s", cfg.VM.StorageDir)
	}

	internalNodes := cfg.InternalNodes()
	if len(internalNodes) != 7 {
		t.Errorf("expected 7 internal nodes, got %d", len(internalNodes))
	}

	mainNodes := cfg.MainNodes()
	if len(mainNodes) != 5 {
		t.Errorf("expected 5 main nodes, got %d", len(mainNodes))
	}

	allNodes := cfg.AllNodes()
	if len(allNodes) == 0 {
		t.Errorf("expected allNodes to be populated")
	}

	sshOpts := cfg.SSHOptions()
	if len(sshOpts) == 0 {
		t.Errorf("expected ssh options")
	}
}

func TestConfig_SecretsEncryption(t *testing.T) {
	pub, priv, err := config.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	if pub == "" || priv == "" {
		t.Fatalf("empty keypair generated")
	}

	secret := "SuperSecretPassword123"
	encrypted, err := config.EncryptValue(secret, pub)
	if err != nil {
		t.Fatalf("failed to encrypt value: %v", err)
	}

	if !config.IsEncrypted(encrypted) {
		t.Fatalf("expected value to be marked as encrypted: %s", encrypted)
	}

	keyFile := filepath.Join(t.TempDir(), "age.key")
	if err := os.WriteFile(keyFile, []byte(priv), 0600); err != nil {
		t.Fatalf("failed to write key file: %v", err)
	}

	decrypted, err := config.DecryptValue(encrypted, keyFile)
	if err != nil {
		t.Fatalf("failed to decrypt value: %v", err)
	}

	if decrypted != secret {
		t.Fatalf("expected decrypted value %q, got %q", secret, decrypted)
	}
}
