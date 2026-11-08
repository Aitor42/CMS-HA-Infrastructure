package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
)

func testConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Network.Internal.CIDR = "192.168.10.0/24"
	cfg.Backup.Engine = "garage"
	cfg.Backup.AccessKey = "test-access-key"
	cfg.Backup.SecretKey = "test-secret-key"
	return cfg
}

// fakeRuntime writes an executable stand-in for docker/podman that records its
// argv and exits according to FAKE_* environment variables. It lets the tests
// exercise the container lifecycle without a real runtime.
func fakeRuntime(t *testing.T) string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "runtime.log")
	t.Setenv("FAKE_RUNTIME_LOG", logPath)
	script := filepath.Join(t.TempDir(), "fake-runtime")
	body := `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_RUNTIME_LOG"
case "$*" in
  inspect*) printf '%s\n' "${FAKE_INSPECT_OUTPUT:-}"; exit "${FAKE_INSPECT_EXIT:-0}" ;;
  "rm -f cms-garage") exit "${FAKE_RM_EXIT:-0}" ;;
  run*) exit "${FAKE_RUN_EXIT:-0}" ;;
  start*) exit 0 ;;
  *"key info"*) exit "${FAKE_KEY_EXIT:-0}" ;;
  *"bucket info"*) exit "${FAKE_INFO_EXIT:-0}" ;;
  *"bucket create"*) exit "${FAKE_CREATE_EXIT:-0}" ;;
  *"bucket allow"*) exit "${FAKE_ALLOW_EXIT:-0}" ;;
esac
exit 0
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("failed to write the fake runtime: %v", err)
	}
	return script
}

func runtimeLog(t *testing.T) string {
	t.Helper()
	content, err := os.ReadFile(os.Getenv("FAKE_RUNTIME_LOG"))
	if err != nil {
		t.Fatalf("failed to read the runtime log: %v", err)
	}
	return string(content)
}

// TestGarageRunArgsUseHostNetwork pins the design decision behind the backup
// drill's first failure: Docker port publishing on a bridge network was not
// reachable from the libvirt guests, so Garage runs on the host network and
// binds the internal address from garage.toml instead.
func TestGarageRunArgsUseHostNetwork(t *testing.T) {
	p := &Phase{cfg: testConfig()}
	joined := strings.Join(p.garageRunArgs(
		"dxflrs/garage:v2.5.0",
		"/var/lib/cms-garage",
		"/var/lib/cms-garage/garage.toml",
		"/var/lib/cms-garage/garage.env",
		"abc123",
	), " ")

	if !strings.Contains(joined, "--network host") {
		t.Errorf("Garage must run on the host network, got: %s", joined)
	}
	if strings.Contains(joined, " -p ") || strings.Contains(joined, "--publish") {
		t.Errorf("Garage must not rely on Docker port publishing, got: %s", joined)
	}
	if !strings.Contains(joined, "--env-file /var/lib/cms-garage/garage.env") {
		t.Errorf("Garage credentials must travel through an env file, got: %s", joined)
	}
	if strings.Contains(joined, "GARAGE_DEFAULT_SECRET_KEY") || strings.Contains(joined, "GARAGE_DEFAULT_ACCESS_KEY") {
		t.Errorf("the S3 credentials must not appear in the container argv, got: %s", joined)
	}
	if !strings.Contains(joined, "cms.ha.backup-config-hash=abc123") {
		t.Errorf("the container must carry the config hash label, got: %s", joined)
	}
}

// TestEnsureGarageConfigReportsChanges rewrites a config written before host
// networking: leaving it in place would keep the S3 API on a wildcard address
// inside a bridge network and every backup would still be refused.
func TestEnsureGarageConfigReportsChanges(t *testing.T) {
	p := &Phase{cfg: testConfig()}
	path := filepath.Join(t.TempDir(), "garage.toml")

	changed, err := p.ensureGarageConfig(path, "192.168.10.254", 3900)
	if err != nil {
		t.Fatalf("ensureGarageConfig failed on the first run: %v", err)
	}
	if !changed {
		t.Error("ensureGarageConfig must report a change when it writes a new config")
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read the rewritten config: %v", err)
	}
	if !strings.Contains(string(content), `api_bind_addr = "192.168.10.254:3900"`) {
		t.Errorf("the S3 API is not bound on the internal address:\n%s", content)
	}
	if strings.Contains(string(content), `"[::]`) {
		t.Errorf("the rewritten config still binds a wildcard address:\n%s", content)
	}

	// A second run must reuse the file instead of rewriting it: the secrets
	// generated on the first run stay stable across re-deploys.
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read the config: %v", err)
	}
	changed, err = p.ensureGarageConfig(path, "192.168.10.254", 3900)
	if err != nil {
		t.Fatalf("ensureGarageConfig failed on the second run: %v", err)
	}
	if changed {
		t.Error("ensureGarageConfig rewrote a config that already matches the topology")
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read the config: %v", err)
	}
	if string(first) != string(second) {
		t.Error("ensureGarageConfig rewrote a config that already binds the internal address")
	}

	// A changed region is a topology change too: the container must be told to
	// restart instead of keeping the region it read at start-up.
	p.cfg.Backup.S3Region = "other-region"
	changed, err = p.ensureGarageConfig(path, "192.168.10.254", 3900)
	if err != nil {
		t.Fatalf("ensureGarageConfig failed on the region change: %v", err)
	}
	if !changed {
		t.Error("ensureGarageConfig must report a change when the region changes")
	}
	content, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read the config: %v", err)
	}
	if !strings.Contains(string(content), `s3_region = "other-region"`) {
		t.Errorf("the rewritten config does not carry the new region:\n%s", content)
	}
}

func TestWriteGarageEnvIsPrivateAndLineSafe(t *testing.T) {
	p := &Phase{cfg: testConfig()}
	path := filepath.Join(t.TempDir(), "garage.env")
	if err := p.writeGarageEnv(path); err != nil {
		t.Fatalf("writeGarageEnv failed: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read the env file: %v", err)
	}
	for _, want := range []string{
		"GARAGE_DEFAULT_ACCESS_KEY=test-access-key",
		"GARAGE_DEFAULT_SECRET_KEY=test-secret-key",
		"GARAGE_DEFAULT_BUCKET=cms-backups",
	} {
		if !strings.Contains(string(content), want) {
			t.Errorf("the env file is missing %q:\n%s", want, content)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat the env file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the env file mode is %o, want 0600", info.Mode().Perm())
	}

	p.cfg.Backup.SecretKey = "line\nbreak"
	if err := p.writeGarageEnv(path); err == nil || !strings.Contains(err.Error(), "line breaks") {
		t.Fatalf("writeGarageEnv() = %v, want a line-break rejection", err)
	}
}

func TestEnsureBucketGrantsAccessAndFailsWhenItCannot(t *testing.T) {
	ctx := context.Background()
	runtime := fakeRuntime(t)
	p := &Phase{cfg: testConfig()}

	// A bucket that already exists must still be granted to the configured
	// key: a previous failed run can leave it inaccessible.
	t.Setenv("FAKE_KEY_EXIT", "0")
	t.Setenv("FAKE_INFO_EXIT", "0")
	t.Setenv("FAKE_ALLOW_EXIT", "0")
	if err := p.ensureBucket(ctx, runtime); err != nil {
		t.Fatalf("ensureBucket failed on an existing bucket: %v", err)
	}
	log := runtimeLog(t)
	if !strings.Contains(log, "bucket allow") {
		t.Errorf("ensureBucket skipped the access grant for an existing bucket:\n%s", log)
	}
	// Garage names the default key exactly after GARAGE_DEFAULT_ACCESS_KEY;
	// "default" is not a key it ever creates.
	if !strings.Contains(log, "--key test-access-key") {
		t.Errorf("ensureBucket did not grant the configured access key:\n%s", log)
	}
	if strings.Contains(log, "--key default") {
		t.Errorf("ensureBucket granted a key named \"default\", which Garage never creates:\n%s", log)
	}

	// A failed grant is fatal: the CronJobs would upload into a bucket they
	// cannot write to and the failure would only surface in the restore drill.
	os.Remove(os.Getenv("FAKE_RUNTIME_LOG"))
	t.Setenv("FAKE_ALLOW_EXIT", "1")
	err := p.ensureBucket(ctx, runtime)
	if err == nil || !strings.Contains(err.Error(), "failed to grant access key") {
		t.Fatalf("ensureBucket() = %v, want the access-grant failure", err)
	}

	// A volume whose key was deleted must fail with an actionable message
	// instead of Garage's bare "Access key not found".
	os.Remove(os.Getenv("FAKE_RUNTIME_LOG"))
	t.Setenv("FAKE_ALLOW_EXIT", "0")
	t.Setenv("FAKE_KEY_EXIT", "1")
	err = p.ensureBucket(ctx, runtime)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("ensureBucket() = %v, want the missing-key error", err)
	}
}

func TestEnsureContainerRecreatesWhenTheConfigChanges(t *testing.T) {
	ctx := context.Background()
	runtime := fakeRuntime(t)
	p := &Phase{cfg: testConfig()}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "garage.toml")
	envPath := filepath.Join(dir, "garage.env")
	if err := os.WriteFile(cfgPath, []byte("config"), 0o600); err != nil {
		t.Fatalf("failed to write the config: %v", err)
	}
	if err := p.writeGarageEnv(envPath); err != nil {
		t.Fatalf("writeGarageEnv failed: %v", err)
	}
	hash, err := garageConfigHash(p.cfg.GarageImage(), dir, cfgPath, envPath)
	if err != nil {
		t.Fatalf("garageConfigHash failed: %v", err)
	}
	// A different data volume must invalidate the label: reusing the container
	// would keep mounting the old directory.
	otherHash, err := garageConfigHash(p.cfg.GarageImage(), filepath.Join(dir, "other"), cfgPath, envPath)
	if err != nil {
		t.Fatalf("garageConfigHash failed: %v", err)
	}
	if otherHash == hash {
		t.Fatal("data directory change did not change the container fingerprint")
	}

	// A running container whose label does not match the current config must
	// be recreated, otherwise it keeps the bind address/credentials it read at
	// start-up.
	t.Setenv("FAKE_INSPECT_OUTPUT", p.cfg.GarageImage()+"|true|host|stale")
	if err := p.ensureContainer(ctx, runtime, dir, cfgPath, envPath); err != nil {
		t.Fatalf("ensureContainer failed: %v", err)
	}
	log := runtimeLog(t)
	if !strings.Contains(log, "rm -f cms-garage") {
		t.Errorf("ensureContainer did not remove the stale container:\n%s", log)
	}
	if !strings.Contains(log, "cms.ha.backup-config-hash="+hash) {
		t.Errorf("ensureContainer did not create a container with the current hash:\n%s", log)
	}

	// A matching label means the container is already converged: no recreate.
	os.Remove(os.Getenv("FAKE_RUNTIME_LOG"))
	t.Setenv("FAKE_INSPECT_OUTPUT", p.cfg.GarageImage()+"|true|host|"+hash)
	if err := p.ensureContainer(ctx, runtime, dir, cfgPath, envPath); err != nil {
		t.Fatalf("ensureContainer failed on a converged container: %v", err)
	}
	if log := runtimeLog(t); strings.Contains(log, "rm -f") || strings.Contains(log, "run ") {
		t.Errorf("ensureContainer recreated an already converged container:\n%s", log)
	}
}
