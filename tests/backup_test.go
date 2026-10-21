package tests

import (
	"strings"
	"testing"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"gopkg.in/yaml.v3"
)

func TestConfig_BackupDefaultsAndValidation(t *testing.T) {
	base := func() *config.Config {
		return &config.Config{
			Network: config.NetworkConfig{
				Internal: config.NetworkDetail{CIDR: "192.168.10.0/24"},
				Main:     config.NetworkDetail{CIDR: "192.168.20.0/24", VIP: "192.168.20.100"},
			},
			Nodes: config.NodesConfig{
				Router:    config.NodeDetail{Name: "ufw-router", IP: "192.168.10.1"},
				Jumpstart: config.NodeDetail{Name: "jumpstart", IP: "192.168.10.10"},
				Masters: []config.NodeDetail{
					{Name: "m1", IP: "192.168.10.11"},
					{Name: "m2", IP: "192.168.10.12"},
					{Name: "m3", IP: "192.168.10.16"},
				},
				LBs: []config.NodeDetail{
					{Name: "lb1", IP: "192.168.20.103"},
					{Name: "lb2", IP: "192.168.20.104"},
				},
				CMSFrontends: []config.NodeDetail{{Name: "cms1", IP: "192.168.20.101"}},
			},
			VM: config.VMConfig{StorageDir: "/var/lib/libvirt/images"},
		}
	}

	t.Run("disabled when engine is empty", func(t *testing.T) {
		cfg := base()
		if cfg.BackupEnabled() {
			t.Fatal("expected backup to be disabled with an empty engine")
		}
	})

	t.Run("defaults for an enabled backup", func(t *testing.T) {
		cfg := base()
		cfg.Backup.Engine = "garage"
		if !cfg.BackupEnabled() {
			t.Fatal("expected backup to be enabled")
		}
		if got := cfg.BackupS3Port(); got != 3900 {
			t.Errorf("BackupS3Port() = %d, want 3900", got)
		}
		if got := cfg.BackupBucket(); got != "cms-backups" {
			t.Errorf("BackupBucket() = %q", got)
		}
		if got := cfg.BackupRetentionDays(); got != 7 {
			t.Errorf("BackupRetentionDays() = %d, want 7", got)
		}
		if got := cfg.GarageImage(); got != "dxflrs/garage:v2.4.1" {
			t.Errorf("GarageImage() = %q", got)
		}
		if got := cfg.BackupDataDir(); got != "/var/lib/cms-garage" {
			t.Errorf("BackupDataDir() = %q", got)
		}
		if got := cfg.BackupS3Endpoint(); got != "http://192.168.10.254:3900" {
			t.Errorf("BackupS3Endpoint() = %q, want the hypervisor internal address", got)
		}
	})

	t.Run("validation rejects an unsupported engine", func(t *testing.T) {
		cfg := base()
		cfg.Backup.Engine = "minio"
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for an unsupported backup engine")
		}
	})

	t.Run("validation rejects an unsafe schedule", func(t *testing.T) {
		cfg := base()
		cfg.Backup.Engine = "garage"
		cfg.Backup.Schedule = "0 2 * * *'; rm -rf /"
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for an unsafe schedule")
		}
	})

	t.Run("valid configuration passes", func(t *testing.T) {
		cfg := base()
		cfg.Backup.Engine = "garage"
		if err := cfg.Validate(); err != nil {
			t.Fatalf("expected valid config to pass, got: %v", err)
		}
	})
}

func TestBackupManifests(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		content, err := cms.KubernetesFS.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read %s: %v", path, err)
		}
		// The manifests must be valid YAML.
		var doc map[string]any
		if err := yaml.Unmarshal(content, &doc); err != nil {
			t.Fatalf("%s is not valid YAML: %v", path, err)
		}
		return string(content)
	}

	t.Run("backup cronjob uploads offsite and publishes a metric", func(t *testing.T) {
		content := read("kubernetes/backups/mariadb-backup-cronjob.yaml")
		for _, want := range []string{
			"backup-config",
			"backup-secret",
			"rclone/rclone:",
			"@sha256:",
			"cms_backup_last_success_timestamp_seconds",
		} {
			if !strings.Contains(content, want) {
				t.Errorf("mariadb-backup-cronjob.yaml is missing %q", want)
			}
		}
		if strings.Contains(content, ":latest") {
			t.Error("mariadb-backup-cronjob.yaml must not use the floating :latest tag")
		}
	})

	t.Run("restore-test cronjob restores into an ephemeral database", func(t *testing.T) {
		content := read("kubernetes/backups/mariadb-restore-test-cronjob.yaml")
		for _, want := range []string{
			"backup-config",
			"backup-secret",
			"rclone/rclone:",
			"mariadb-install-db",
			// The pod runs as root, so mariadbd needs the explicit opt-in;
			// without it the drill aborts before restoring anything.
			"--user=root",
			"cms_backup_restore_last_success_timestamp_seconds",
			"cms_backup_restore_duration_seconds",
		} {
			if !strings.Contains(content, want) {
				t.Errorf("mariadb-restore-test-cronjob.yaml is missing %q", want)
			}
		}
		if strings.Contains(content, ":latest") {
			t.Error("mariadb-restore-test-cronjob.yaml must not use the floating :latest tag")
		}
	})

	t.Run("garage template exists and is placeholder-driven", func(t *testing.T) {
		content, err := cms.TemplatesFS.ReadFile("templates/backup/garage.toml.tmpl")
		if err != nil {
			t.Fatalf("failed to read garage template: %v", err)
		}
		for _, want := range []string{
			"{{RPC_SECRET}}",
			"{{S3_PORT}}",
			"{{S3_BIND_ADDR}}",
			`api_bind_addr = "{{S3_BIND_ADDR}}:{{S3_PORT}}"`,
			"data_dir",
		} {
			if !strings.Contains(string(content), want) {
				t.Errorf("garage.toml.tmpl is missing %q", want)
			}
		}
		// The container runs on the host network, so a wildcard bind would
		// expose the store on the WAN and a loopback-only bind would make it
		// unreachable from the cluster.
		if strings.Contains(string(content), `"[::]`) {
			t.Error("garage.toml.tmpl binds a wildcard address; the store must bind the internal bridge")
		}
	})
}
