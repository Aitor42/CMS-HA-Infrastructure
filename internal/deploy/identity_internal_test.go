package deploy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
)

func TestConfigFingerprint(t *testing.T) {
	cfg := &config.Config{
		VM:       config.VMConfig{StorageDir: "/srv/lab", LibvirtURI: "qemu:///system"},
		Database: config.DatabaseConfig{Name: "wordpress", Password: "one"},
	}
	original, err := configFingerprint(cfg, DeployOpts{})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database.Password = "rotated"
	cfg.Database.RootPassword = "root-secret"
	cfg.Database.ExporterPassword = "exporter-secret"
	cfg.WordPress.AdminPassword = "wp-secret"
	cfg.Monitoring.GrafanaAdminPassword = "grafana-secret"
	cfg.PKI.ProvisionerPassword = "ca-secret"
	cfg.Backup.AccessKey = "access-key"
	cfg.Backup.SecretKey = "s3-secret"
	cfg.Deploy.SSHTimeout = time.Hour
	got, err := configFingerprint(cfg, DeployOpts{Resume: true, RunID: "another-run"})
	if err != nil || got != original {
		t.Fatalf("credentials and run metadata must not affect fingerprint: %q, %v", got, err)
	}
	if cfg.Database.Password != "rotated" || cfg.Backup.SecretKey != "s3-secret" {
		t.Fatal("fingerprinting mutated caller's configuration")
	}
	cfg.Database.Name = "different-environment"
	changed, _ := configFingerprint(cfg, DeployOpts{})
	if changed == original {
		t.Fatal("desired state change must invalidate fingerprint")
	}
	for _, opts := range []DeployOpts{{FastProvisioning: true}, {SkipVMCreate: true}} {
		withOptions, _ := configFingerprint(cfg, opts)
		if withOptions == changed {
			t.Fatalf("provisioning mode must affect identity: %+v", opts)
		}
	}
	if _, err := configFingerprint(nil, DeployOpts{}); err == nil {
		t.Fatal("nil config must fail")
	}
}

func TestCheckpointIdentity(t *testing.T) {
	identity := &Identity{ConfigSHA256: "config", BinarySHA256: "binary", HostSHA256: "host", Build: "v1"}
	for _, mode := range []DeployOpts{{Resume: true}, {FromPhase: "drbd-setup"}} {
		t.Run("compatible", func(t *testing.T) {
			s := &State{Identity: identity, CompletedPhases: []string{"00-init-vms"}}
			if err := s.bindIdentity(identity, mode); err != nil {
				t.Fatal(err)
			}
		})
		for _, changed := range []Identity{
			{ConfigSHA256: "other", BinarySHA256: "binary", HostSHA256: "host"},
			{ConfigSHA256: "config", BinarySHA256: "other", HostSHA256: "host"},
			{ConfigSHA256: "config", BinarySHA256: "binary", HostSHA256: "other"},
		} {
			s := &State{Identity: identity, CompletedPhases: []string{"00-init-vms"}}
			if err := s.bindIdentity(&changed, mode); err == nil {
				t.Fatalf("accepted changed deployment identity: %+v", changed)
			}
		}
		legacy := &State{CompletedPhases: []string{"00-init-vms"}}
		if err := legacy.bindIdentity(identity, mode); err == nil || !strings.Contains(err.Error(), "new --state-file") {
			t.Fatalf("legacy progress must require explicit migration: %v", err)
		}
	}
	fresh := &State{}
	if err := fresh.bindIdentity(identity, DeployOpts{Resume: true}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "deploy.state")
	if err := fresh.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(path)
	if err != nil || loaded.Identity == nil || *loaded.Identity != *identity {
		t.Fatalf("identity not persisted: %+v, %v", loaded, err)
	}
	// An explicit full run can bind a new identity; it does not trust progress.
	if err := loaded.bindIdentity(&Identity{ConfigSHA256: "new"}, DeployOpts{}); err != nil {
		t.Fatal(err)
	}
}

func TestDeploymentIdentityDoesNotStoreSecrets(t *testing.T) {
	cfg := &config.Config{Database: config.DatabaseConfig{Password: "do-not-store-this-password"}}
	identity, err := deploymentIdentity(cfg, DeployOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{identity.ConfigSHA256, identity.BinarySHA256, identity.HostSHA256} {
		if len(hash) != 64 {
			t.Fatalf("expected sha256 identity, got %q", hash)
		}
	}
	encoded, err := json.Marshal(identity)
	if err != nil || strings.Contains(string(encoded), cfg.Database.Password) {
		t.Fatalf("unsafe identity serialization: %s, %v", encoded, err)
	}
}

func TestResumeRejectsLegacyStateBeforePreflight(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deploy.state")
	state := &State{CompletedPhases: []string{"00-init-vms"}}
	if err := state.Save(path); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	// No libvirt/SSH dependencies: a preflight would panic, but unsafe resume
	// must stop before any host checks or checkpoint writes.
	o := &Orchestrator{Cfg: &config.Config{}}
	err := o.Deploy(t.Context(), DeployOpts{Resume: true, StateFile: path})
	if err == nil || !strings.Contains(err.Error(), "no deployment identity") {
		t.Fatalf("expected identity rejection before preflight, got %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("rejected resume mutated checkpoint")
	}
}
