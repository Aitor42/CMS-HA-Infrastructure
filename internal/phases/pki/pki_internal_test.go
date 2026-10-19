package pki

import "testing"

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
