package tests

import (
	"os"
	"strings"
	"testing"
)

// TestTLSRenewScript pins the two renewal modes of the load-balancer
// certificate timer. The production path must keep asking for a renewal only
// when fewer than 720h remain; the e2e drill needs --force because step
// rejects an --expires-in value above the certificate lifetime ("must be
// within (lower than) the certificate validity period"), which is what an
// earlier version of the drill got wrong.
func TestTLSRenewScript(t *testing.T) {
	script, err := os.ReadFile(repoFile(t, "puppet/modules/role/files/nginx/cms-tls-renew.sh"))
	if err != nil {
		t.Fatalf("failed to read cms-tls-renew.sh: %v", err)
	}
	content := string(script)

	for _, want := range []string{
		`RENEW_WITHIN="720h"`,
		`RENEW_ARGS=(--expires-in "$RENEW_WITHIN")`,
		`if [ "${RENEW_FORCE:-0}" = "1" ]; then`,
		`RENEW_ARGS=(--force)`,
		`step ca renew "${RENEW_ARGS[@]}"`,
		`--exec "systemctl reload nginx"`,
		`certificate renewed and nginx reloaded`,
		`PKI_ENV=/etc/cms-ha/pki.env`,
		`. "$PKI_ENV"`,
		`CA_URL="${CMS_CA_URL:-https://ca.internal.local:8443}"`,
		`ROOT_CA="${CMS_ROOT_CA:-/usr/local/share/ca-certificates/cms_root_ca.crt}"`,
		`CA_NAME="${CMS_CA_NAME:-CMS Local CA}"`,
		`grep -Fq -- "$CA_NAME"`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("cms-tls-renew.sh is missing %q", want)
		}
	}

	// The CA name is configuration, not a regular expression: a name with a
	// regex metacharacter must still match the issuer line literally.
	if strings.Contains(content, `grep -q "$CA_NAME"`) {
		t.Error("cms-tls-renew.sh matches the CA name as a regular expression instead of a fixed string")
	}

	// The CA endpoint must come from the file the PKI phase writes, not from a
	// second hardcoded copy that silently points at the old CA.
	if strings.Contains(content, `CA_URL="https://`) {
		t.Error("cms-tls-renew.sh hardcodes CA_URL instead of sourcing /etc/cms-ha/pki.env")
	}

	// A threshold above the configured 2160h lifetime is rejected by step, so
	// it must never come back as the drill's mechanism.
	if strings.Contains(content, "8760h") {
		t.Error("cms-tls-renew.sh uses an --expires-in above the certificate lifetime")
	}

	// step exits 0 when the certificate is not due, so "not renewed" must be
	// handled before the exit-code branch or the timer would report a renewal
	// that never happened.
	notRenewed := strings.Index(content, `grep -qi "not renewed"`)
	statusBranch := strings.Index(content, `if [ "$status" -ne 0 ]`)
	if notRenewed < 0 || statusBranch < 0 || notRenewed > statusBranch {
		t.Error("cms-tls-renew.sh must check \"not renewed\" before the exit-code branch")
	}
}
