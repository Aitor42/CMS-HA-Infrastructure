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
	} {
		if !strings.Contains(content, want) {
			t.Errorf("cms-tls-renew.sh is missing %q", want)
		}
	}

	// A threshold above the configured 2160h lifetime is rejected by step, so
	// it must never come back as the drill's mechanism.
	if strings.Contains(content, "8760h") {
		t.Error("cms-tls-renew.sh uses an --expires-in above the certificate lifetime")
	}
}
