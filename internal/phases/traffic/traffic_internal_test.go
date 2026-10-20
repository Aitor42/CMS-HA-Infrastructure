package traffic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
)

func testConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Network.Main.CIDR = "192.168.20.0/24"
	cfg.Network.Internal.CIDR = "192.168.10.0/24"
	cfg.Network.Main.VIP = "192.168.20.100"
	cfg.Nodes.CMSFrontends = []config.NodeDetail{{Name: "main-cms1", IP: "192.168.20.101"}}
	cfg.Traffic = config.TrafficConfig{VHost: "cms.example.test"}
	return cfg
}

func TestResolveBaseURL(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{"target with scheme", Options{TargetIP: "http://10.0.0.5:8080"}, "http://10.0.0.5:8080"},
		{"target bare host", Options{TargetIP: "10.0.0.5"}, "https://10.0.0.5"},
		{"external uses the VIP", Options{Mode: "external"}, "https://192.168.20.100"},
		{"internal uses the first frontend", Options{Mode: "internal"}, "http://192.168.20.101"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveBaseURL(testConfig(), tc.opts); got != tc.want {
				t.Fatalf("resolveBaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveBaseURL_InternalFallback(t *testing.T) {
	cfg := testConfig()
	cfg.Nodes.CMSFrontends = nil
	if got := resolveBaseURL(cfg, Options{Mode: "internal"}); got != "http://192.168.20.101" {
		t.Fatalf("internal fallback = %q, want http://192.168.20.101", got)
	}
}

func TestBuildArgs_WithCAStreamsToPrometheus(t *testing.T) {
	args := buildArgs("grafana/k6:0.55.0", k6Run{
		baseURL:  "https://192.168.20.100",
		vhost:    "cms.example.test",
		caCert:   hostCACertPath,
		rwURL:    "http://192.168.10.20:9090/api/v1/write",
		vus:      7,
		duration: 90,
	})
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"--network host",
		"-e BASE_URL=https://192.168.20.100",
		"-e VHOST=cms.example.test",
		"-v " + hostCACertPath + ":" + containerCACert + ":ro",
		"-e SSL_CERT_FILE=" + containerCACert,
		"-e K6_PROMETHEUS_RW_SERVER_URL=http://192.168.10.20:9090/api/v1/write",
		"-e K6_PROMETHEUS_RW_TREND_STATS=p(95),p(99)",
		"grafana/k6:0.55.0 run",
		"--vus 7",
		"--duration 90s",
		"--out experimental-prometheus-rw",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q\nfull: %s", want, joined)
		}
	}
	if strings.Contains(joined, "insecure-skip-tls-verify") {
		t.Errorf("TLS verification must not be skipped when a CA is mounted: %s", joined)
	}
	if !strings.HasSuffix(joined, " -") {
		t.Errorf("script must be read from stdin (trailing '-'): %s", joined)
	}
}

func TestBuildArgs_NoCASkipsTLSVerification(t *testing.T) {
	args := buildArgs("grafana/k6:0.55.0", k6Run{
		baseURL:  "https://192.168.20.100",
		vhost:    "cms.example.test",
		vus:      5,
		duration: 60,
	})
	joined := strings.Join(args, " ")

	// k6 removed the bare `--insecure` flag; the load test must use the
	// documented `--insecure-skip-tls-verify`, right after `k6 run`.
	if !strings.Contains(joined, "run --insecure-skip-tls-verify --vus 5") {
		t.Errorf("expected --insecure-skip-tls-verify right after `run`: %s", joined)
	}
	if strings.Contains(joined, "SSL_CERT_FILE") {
		t.Errorf("no CA should be mounted: %s", joined)
	}
	if strings.Contains(joined, "experimental-prometheus-rw") {
		t.Errorf("no remote-write output when prometheus_rw_url is empty: %s", joined)
	}
}

// TestFirstExistingFile covers the CA lookup the load test performs before it
// mounts the certificate into the k6 container: the home copy the PKI phase
// leaves for the unprivileged account must be used when it is there, and a
// missing CA must fall through to skipping verification instead of failing.
func TestFirstExistingFile(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present.crt")
	if err := os.WriteFile(present, []byte("cert"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.crt")

	if got := firstExistingFile([]string{missing, present}); got != present {
		t.Errorf("firstExistingFile = %q, want %q", got, present)
	}
	if got := firstExistingFile([]string{missing}); got != "" {
		t.Errorf("firstExistingFile with no match = %q, want empty", got)
	}
	if got := firstExistingFile(nil); got != "" {
		t.Errorf("firstExistingFile(nil) = %q, want empty", got)
	}
}
