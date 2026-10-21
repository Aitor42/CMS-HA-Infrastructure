package backup

import (
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
	cfg.Backup.AccessKey = "access"
	cfg.Backup.SecretKey = "secret"
	return cfg
}

// TestGarageRunArgsUseHostNetwork pins the design decision behind the backup
// drill's first failure: Docker port publishing on a bridge network was not
// reachable from the libvirt guests, so Garage runs on the host network and
// binds the internal address from garage.toml instead.
func TestGarageRunArgsUseHostNetwork(t *testing.T) {
	p := &Phase{cfg: testConfig()}
	joined := strings.Join(p.garageRunArgs("dxflrs/garage:v2.4.1", "/var/lib/cms-garage", "/var/lib/cms-garage/garage.toml"), " ")

	if !strings.Contains(joined, "--network host") {
		t.Errorf("Garage must run on the host network, got: %s", joined)
	}
	if strings.Contains(joined, " -p ") || strings.Contains(joined, "--publish") {
		t.Errorf("Garage must not rely on Docker port publishing, got: %s", joined)
	}
	if !strings.Contains(joined, "GARAGE_DEFAULT_ACCESS_KEY=access") {
		t.Errorf("Garage must receive the configured access key, got: %s", joined)
	}
}

// TestEnsureGarageConfigMigratesOldBind rewrites a config written before host
// networking: leaving it in place would keep the S3 API on a wildcard address
// inside a bridge network and every backup would still be refused.
func TestEnsureGarageConfigMigratesOldBind(t *testing.T) {
	p := &Phase{cfg: testConfig()}
	path := filepath.Join(t.TempDir(), "garage.toml")
	if err := os.WriteFile(path, []byte("metadata_dir = \"/var/lib/garage/meta\"\n\n[s3_api]\napi_bind_addr = \"[::]:3900\"\n"), 0o600); err != nil {
		t.Fatalf("failed to write the old config: %v", err)
	}

	if err := p.ensureGarageConfig(path, "192.168.10.254", 3900); err != nil {
		t.Fatalf("ensureGarageConfig failed: %v", err)
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
	if err := p.ensureGarageConfig(path, "192.168.10.254", 3900); err != nil {
		t.Fatalf("ensureGarageConfig failed on the second run: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read the config: %v", err)
	}
	if string(first) != string(second) {
		t.Error("ensureGarageConfig rewrote a config that already binds the internal address")
	}
}
