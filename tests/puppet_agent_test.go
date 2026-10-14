package tests

import (
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
)

func TestPuppetAgentCommand(t *testing.T) {
	base := phases.PuppetAgentCommand("")
	if strings.Contains(base, "--server") {
		t.Errorf("expected no --server when server is empty, got %q", base)
	}
	if !strings.HasSuffix(base, "|| [ $? -eq 2 ]") {
		t.Errorf("expected exit-code-2 tolerance, got %q", base)
	}

	got := phases.PuppetAgentCommand("jumpstart.internal.local")
	if strings.Count(got, "--server") != 1 {
		t.Errorf("expected exactly one --server, got %q", got)
	}
	if !strings.Contains(got, "--server jumpstart.internal.local") {
		t.Errorf("expected a single well-formed --server, got %q", got)
	}
}
