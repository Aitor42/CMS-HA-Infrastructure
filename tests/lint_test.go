package tests

import (
	"context"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/lint"
)

func TestLint_FindModuleRoot(t *testing.T) {
	root, err := lint.FindModuleRoot()
	if err != nil {
		t.Fatalf("expected module root to be found: %v", err)
	}
	if root == "" {
		t.Fatalf("expected non-empty module root")
	}
}

func TestLint_FindFiles(t *testing.T) {
	root, err := lint.FindModuleRoot()
	if err != nil {
		t.Fatalf("findModuleRoot failed: %v", err)
	}
	scripts := lint.FindFiles(root, "*.sh")
	if len(scripts) == 0 {
		t.Logf("no shell scripts found or already migrated")
	}
}

func TestLint_RunLintsGo(t *testing.T) {
	ctx := context.Background()
	// Run only Go linter (go vet)
	opts := lint.Options{Go: true}
	err := lint.RunLints(ctx, opts)
	if err != nil {
		t.Errorf("go lint failed: %v", err)
	}
}
