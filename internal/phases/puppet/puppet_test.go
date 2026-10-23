package puppet

import (
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
)

func TestCatalogConcurrencyResolution(t *testing.T) {
	t.Setenv("PUPPET_CONCURRENCY", "")
	p := &Phase{cfg: &config.Config{Deploy: config.DeployConfig{PuppetConcurrency: 6}}}
	if got, source := p.catalogConcurrency(); got != 6 || source != "config" {
		t.Fatalf("config concurrency = (%d, %q), want (6, config)", got, source)
	}

	t.Setenv("PUPPET_CONCURRENCY", "7")
	if got, source := p.catalogConcurrency(); got != 7 || source != "PUPPET_CONCURRENCY" {
		t.Fatalf("env concurrency = (%d, %q), want (7, PUPPET_CONCURRENCY)", got, source)
	}

	// An invalid override falls through to config/auto instead of failing.
	t.Setenv("PUPPET_CONCURRENCY", "many")
	if got, source := p.catalogConcurrency(); got != 6 || source != "config" {
		t.Fatalf("invalid env concurrency = (%d, %q), want (6, config)", got, source)
	}

	p.cfg.Deploy.PuppetConcurrency = 0
	got, source := p.catalogConcurrency()
	if source != "auto" {
		t.Fatalf("auto source = %q, want auto", source)
	}
	if got < 4 || got > 8 {
		t.Fatalf("auto concurrency = %d, want 4..8", got)
	}
}

func TestAutosignPolicyIsExact(t *testing.T) {
	policy := autosignPolicy("jumpstart.internal.local", []config.NodeSpec{
		{FQDN: "internal-master1.internal.local"},
		{FQDN: "main-lb1.main.local"},
		{FQDN: "internal-master1.internal.local"}, // duplicate
		{FQDN: ""}, // skipped
	})
	if strings.Contains(policy, "*") {
		t.Fatalf("autosign policy must not contain a wildcard: %q", policy)
	}
	want := "internal-master1.internal.local\njumpstart.internal.local\nmain-lb1.main.local\n"
	if policy != want {
		t.Fatalf("autosign policy = %q, want %q", policy, want)
	}
}
