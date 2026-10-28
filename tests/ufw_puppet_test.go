package tests

import (
	"os"
	"strings"
	"testing"
)

// TestUFWPuppetChecksMatchTheSingleLinePolicyOutput guards two idempotency
// regressions that only show up as every Puppet run re-applying a default
// policy: ufw prints the three defaults on one line, so only the incoming
// policy is prefixed with "Default:".
func TestUFWPuppetChecksMatchTheSingleLinePolicyOutput(t *testing.T) {
	base := readRepoFile(t, "puppet/modules/role/manifests/base.pp")
	if strings.Contains(base, `grep -q "Default: allow (outgoing)"`) {
		t.Error("base.pp matches a prefixed outgoing policy that ufw never prints")
	}
	if !strings.Contains(base, `grep -q "allow (outgoing)"`) {
		t.Error("base.pp does not check the outgoing policy token")
	}

	router := readRepoFile(t, "puppet/modules/role/manifests/router.pp")
	if strings.Contains(router, `grep -q "Default: deny (routed)"`) {
		t.Error("router.pp matches a prefixed routed policy that ufw never prints")
	}
	if !strings.Contains(router, `grep -q "deny (routed)"`) {
		t.Error("router.pp does not check the routed policy token")
	}
}

// TestNATRulesAreRenderedFromConfiguration keeps the router's DNAT/MASQUERADE
// targets tied to the configured networks. A hardcoded VIP or subnet is a
// silent breakage for any deployment that changes network.*.cidr or
// network.main.vip.
func TestNATRulesAreRenderedFromConfiguration(t *testing.T) {
	nat := readRepoFile(t, "templates/ufw/nat-rules")
	for _, placeholder := range []string{"${WAN_IF}", "${INTERNAL_CIDR}", "${MAIN_CIDR}", "${LB_VIP}"} {
		if !strings.Contains(nat, placeholder) {
			t.Errorf("templates/ufw/nat-rules is missing placeholder %s", placeholder)
		}
	}
	for _, hardcoded := range []string{"192.168.10.0/24", "192.168.20.0/24", "192.168.20.100"} {
		if strings.Contains(nat, hardcoded) {
			t.Errorf("templates/ufw/nat-rules still hardcodes %s", hardcoded)
		}
	}
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(repoFile(t, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}
