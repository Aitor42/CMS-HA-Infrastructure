package tests

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/pki"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

func pkiConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return &config.Config{
		PKI: config.PKIConfig{
			ProvisionerPassword: testutil.FakeSecret("ca"),
			CAPort:              8443,
			Domain:              "ca.internal.local",
			StepVersion:         "0.28.0",
			StepCAVersion:       "0.28.0",
			CertDNSNames:        []string{"cms.internal.local"},
			CertLifetimeHours:   2160,
		},
		Network: config.NetworkConfig{
			Main: config.NetworkDetail{VIP: "192.168.20.100"},
		},
		Nodes: config.NodesConfig{
			Jumpstart: config.NodeDetail{Name: "jumpstart", IP: "192.168.10.10"},
			Masters: []config.NodeDetail{
				{Name: "master1", IP: "192.168.10.11"},
			},
			LBs: []config.NodeDetail{
				{Name: "lb1", IP: "192.168.20.101"},
			},
		},
	}
}

func TestPKIPhaseRun(t *testing.T) {
	cfg := pkiConfig(t)
	jumpIP := cfg.Nodes.Jumpstart.IP
	certPEM, keyPEM, rootPEM, err := testutil.NewTestCertificate(cfg.LBTLSSANs(), cfg.PKICommonName(),
		time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
	if err != nil {
		t.Fatalf("NewTestCertificate: %v", err)
	}
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			switch {
			case strings.Contains(cmd, "cat /root/.step/certs/root_ca.crt"):
				return string(rootPEM), "", 0, nil
			case strings.Contains(cmd, "cat /tmp/cms-lb.crt"):
				return string(certPEM) + "===CMSKEY===\n" + string(keyPEM), "", 0, nil
			default:
				return "", "", 0, nil
			}
		},
	}

	if err := pki.NewPhase(cfg, fake).Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The CA password must be uploaded with the file mode the phase asks for
	// (the fake records content; the mode is asserted by the source contract).
	if got := string(fake.Files[jumpIP]["/root/.step/password.txt"]); got != testutil.FakeSecret("ca")+"\n" {
		t.Errorf("uploaded CA password = %q, want the configured fixture value", got)
	}
	for _, want := range []string{
		"step ca init",
		"systemctl enable --now step-ca",
		"step ca provisioner update",
		"systemctl restart step-ca",
		"curl -kf",
		"step ca certificate",
		"command -v step-ca",
	} {
		if !hasCommand(fake.Commands, jumpIP, want) {
			t.Errorf("jumpstart never ran %q", want)
		}
	}

	// The root CA goes to every non-jumpstart node and is trusted there.
	if got := string(fake.Files["192.168.10.11"]["/usr/local/share/ca-certificates/cms_root_ca.crt"]); got != string(rootPEM) {
		t.Errorf("master root CA = %q, want the CA PEM", got)
	}
	if !hasCommand(fake.Commands, "192.168.10.11", "update-ca-certificates") {
		t.Error("master did not refresh its CA trust store")
	}

	// The LB certificate and key are installed and nginx is reloaded.
	if got := string(fake.Files["192.168.20.101"]["/etc/nginx/ssl/cms.crt"]); got != strings.TrimSpace(string(certPEM)) {
		t.Errorf("LB certificate = %q, want the issued PEM", got)
	}
	if got := string(fake.Files["192.168.20.101"]["/etc/nginx/ssl/cms.key"]); got != strings.TrimSpace(string(keyPEM)) {
		t.Errorf("LB key = %q, want the issued key", got)
	}
	// The renewal timer reads the CA endpoint from this file; without it the
	// script falls back to the default domain/port/name.
	env := string(fake.Files["192.168.20.101"]["/etc/cms-ha/pki.env"])
	for _, want := range []string{
		"CMS_CA_URL='https://ca.internal.local:8443'",
		"CMS_ROOT_CA='/usr/local/share/ca-certificates/cms_root_ca.crt'",
		"CMS_CA_NAME='CMS Local CA'",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("LB PKI environment is missing %q:\n%s", want, env)
		}
	}
	if !hasCommand(fake.Commands, "192.168.20.101", "systemctl reload nginx") {
		t.Error("LB did not reload nginx with the new certificate")
	}

	// The unprivileged copy for the k6 container is written under HOME.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	localCA := filepath.Join(home, ".cms-ha", "cms_root_ca.crt")
	if got, err := os.ReadFile(localCA); err != nil || string(got) != string(rootPEM) {
		t.Errorf("local root CA %s = %q, %v", localCA, got, err)
	}
}

func TestPKIPhaseRejectsCertificateWithoutEverySAN(t *testing.T) {
	cfg := pkiConfig(t)
	// The VIP is the address clients actually use; a certificate without it
	// would still load in nginx and pass a `curl -k` probe.
	certPEM, keyPEM, rootPEM, err := testutil.NewTestCertificate(
		[]string{"cms.internal.local", "192.168.20.101"}, cfg.PKICommonName(),
		time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
	if err != nil {
		t.Fatalf("NewTestCertificate: %v", err)
	}
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			switch {
			case strings.Contains(cmd, "cat /root/.step/certs/root_ca.crt"):
				return string(rootPEM), "", 0, nil
			case strings.Contains(cmd, "cat /tmp/cms-lb.crt"):
				return string(certPEM) + "===CMSKEY===\n" + string(keyPEM), "", 0, nil
			default:
				return "", "", 0, nil
			}
		},
	}

	err = pki.NewPhase(cfg, fake).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "192.168.20.100") {
		t.Fatalf("Run() = %v, want the missing VIP SAN to fail the phase", err)
	}
	if _, ok := fake.Files["192.168.20.101"]["/etc/nginx/ssl/cms.crt"]; ok {
		t.Error("the invalid certificate was installed on the load balancer")
	}
	if hasCommand(fake.Commands, "192.168.20.101", "systemctl reload nginx") {
		t.Error("nginx was reloaded with a certificate that does not cover the VIP")
	}
	// The rejected certificate's key must not linger on the CA host.
	if !hasCommand(fake.Commands, cfg.Nodes.Jumpstart.IP, "rm -f /tmp/cms-lb.crt /tmp/cms-lb.key") {
		t.Error("the CA host kept /tmp/cms-lb.crt and /tmp/cms-lb.key after a rejected certificate")
	}
}

func TestPKIPhaseRequiresProvisionerPassword(t *testing.T) {
	cfg := pkiConfig(t)
	cfg.PKI.ProvisionerPassword = ""
	fake := &testutil.FakeExecutor{}

	err := pki.NewPhase(cfg, fake).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "provisioner_password") {
		t.Fatalf("Run() = %v, want the provisioner password error", err)
	}
	if len(fake.Commands) != 0 {
		t.Errorf("phase ran %d commands without a password, want 0", len(fake.Commands))
	}
}

func TestPKIPhaseFailsWhenRootCAIsUnreadable(t *testing.T) {
	cfg := pkiConfig(t)
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			if strings.Contains(cmd, "cat /root/.step/certs/root_ca.crt") {
				return "", "", 1, context.DeadlineExceeded
			}
			return "", "", 0, nil
		},
	}

	err := pki.NewPhase(cfg, fake).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "failed to read root CA") {
		t.Fatalf("Run() = %v, want the root CA read failure", err)
	}
}
