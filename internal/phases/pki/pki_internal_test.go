package pki

import (
	"strings"
	"testing"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

func TestStepReleaseURLs(t *testing.T) {
	tests := []struct {
		name    string
		repo    string
		asset   string
		version string
		want    []string
	}{
		{
			name:    "pinned cli tries the -1 asset and then the plain one",
			repo:    "cli",
			asset:   "step-cli",
			version: "0.31.0",
			want: []string{
				"https://github.com/smallstep/cli/releases/download/v0.31.0/step-cli_0.31.0-1_${ARCH}.deb",
				"https://github.com/smallstep/cli/releases/download/v0.31.0/step-cli_0.31.0_${ARCH}.deb",
			},
		},
		{
			name:    "a leading v is normalised",
			repo:    "certificates",
			asset:   "step-ca",
			version: "v0.30.2",
			want: []string{
				"https://github.com/smallstep/certificates/releases/download/v0.30.2/step-ca_0.30.2-1_${ARCH}.deb",
				"https://github.com/smallstep/certificates/releases/download/v0.30.2/step-ca_0.30.2_${ARCH}.deb",
			},
		},
		{
			name:    "the ca asset shape that only exists without the -1 suffix is still tried",
			repo:    "certificates",
			asset:   "step-ca",
			version: "0.27.5",
			want: []string{
				"https://github.com/smallstep/certificates/releases/download/v0.27.5/step-ca_0.27.5-1_${ARCH}.deb",
				"https://github.com/smallstep/certificates/releases/download/v0.27.5/step-ca_0.27.5_${ARCH}.deb",
			},
		},
		{
			name:    "no pin falls back to the version-independent alias",
			repo:    "certificates",
			asset:   "step-ca",
			version: "  ",
			want:    []string{"https://github.com/smallstep/certificates/releases/latest/download/step-ca_${ARCH}.deb"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stepReleaseURLs(tt.repo, tt.asset, tt.version)
			if len(got) != len(tt.want) {
				t.Fatalf("stepReleaseURLs(%q, %q, %q) = %v, want %v", tt.repo, tt.asset, tt.version, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("candidate %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestQuoteURLsKeepsArchExpansion(t *testing.T) {
	got := quoteURLs([]string{"https://example.invalid/step-cli_${ARCH}.deb", "https://example.invalid/alt_${ARCH}.deb"})
	want := `"https://example.invalid/step-cli_${ARCH}.deb" "https://example.invalid/alt_${ARCH}.deb"`
	if got != want {
		t.Errorf("quoteURLs() = %s, want %s", got, want)
	}
}

func TestValidateIssuedCertificateAcceptsEverySAN(t *testing.T) {
	now := time.Now()
	sans := []string{"cms.internal.local", "192.168.20.100", "192.168.20.101"}
	certPEM, keyPEM, err := testutil.NewTestCertificate(sans, "CMS Local CA", now.Add(-time.Hour), now.Add(90*24*time.Hour))
	if err != nil {
		t.Fatalf("NewTestCertificate: %v", err)
	}

	if err := validateIssuedCertificate(string(certPEM), string(keyPEM), sans, "CMS Local CA", now); err != nil {
		t.Fatalf("validateIssuedCertificate() = %v, want a pass", err)
	}
}

func TestValidateIssuedCertificateRejectsMissingSAN(t *testing.T) {
	now := time.Now()
	certPEM, keyPEM, err := testutil.NewTestCertificate(
		[]string{"cms.internal.local", "192.168.20.101"}, "CMS Local CA",
		now.Add(-time.Hour), now.Add(90*24*time.Hour))
	if err != nil {
		t.Fatalf("NewTestCertificate: %v", err)
	}

	err = validateIssuedCertificate(string(certPEM), string(keyPEM),
		[]string{"cms.internal.local", "192.168.20.100", "192.168.20.101"}, "CMS Local CA", now)
	if err == nil || !strings.Contains(err.Error(), "192.168.20.100") {
		t.Fatalf("validateIssuedCertificate() = %v, want the missing VIP SAN to be reported", err)
	}
}

func TestValidateIssuedCertificateRejectsMismatchedKey(t *testing.T) {
	now := time.Now()
	sans := []string{"cms.internal.local"}
	certPEM, _, err := testutil.NewTestCertificate(sans, "CMS Local CA", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("NewTestCertificate: %v", err)
	}
	_, otherKeyPEM, err := testutil.NewTestCertificate(sans, "CMS Local CA", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("NewTestCertificate: %v", err)
	}

	err = validateIssuedCertificate(string(certPEM), string(otherKeyPEM), sans, "CMS Local CA", now)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("validateIssuedCertificate() = %v, want the key mismatch", err)
	}
}

func TestValidateIssuedCertificateRejectsForeignIssuer(t *testing.T) {
	now := time.Now()
	sans := []string{"cms.internal.local"}
	certPEM, keyPEM, err := testutil.NewTestCertificate(sans, "Other CA", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("NewTestCertificate: %v", err)
	}

	err = validateIssuedCertificate(string(certPEM), string(keyPEM), sans, "CMS Local CA", now)
	if err == nil || !strings.Contains(err.Error(), "Other CA") {
		t.Fatalf("validateIssuedCertificate() = %v, want the foreign issuer to be reported", err)
	}
}

func TestValidateIssuedCertificateRejectsExpiredCertificate(t *testing.T) {
	now := time.Now()
	sans := []string{"cms.internal.local"}
	certPEM, keyPEM, err := testutil.NewTestCertificate(sans, "CMS Local CA", now.Add(-2*time.Hour), now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("NewTestCertificate: %v", err)
	}

	err = validateIssuedCertificate(string(certPEM), string(keyPEM), sans, "CMS Local CA", now)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("validateIssuedCertificate() = %v, want the expired certificate to be rejected", err)
	}
}

func TestValidateIssuedCertificateRejectsNonPEM(t *testing.T) {
	err := validateIssuedCertificate("not a certificate", "not a key", []string{"cms.internal.local"}, "CMS Local CA", time.Now())
	if err == nil || !strings.Contains(err.Error(), "not PEM") {
		t.Fatalf("validateIssuedCertificate() = %v, want the non-PEM error", err)
	}
}
