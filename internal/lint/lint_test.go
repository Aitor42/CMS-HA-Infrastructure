package lint

import (
	"context"
	"testing"
)

func TestFindModuleRoot(t *testing.T) {
	root, err := findModuleRoot()
	if err != nil {
		t.Fatalf("expected module root to be found: %v", err)
	}
	if root == "" {
		t.Fatalf("expected non-empty module root")
	}
}

func TestFindFiles(t *testing.T) {
	root, err := findModuleRoot()
	if err != nil {
		t.Fatalf("findModuleRoot failed: %v", err)
	}
	scripts := findFiles(root, "*.sh")
	if len(scripts) == 0 {
		t.Logf("no shell scripts found or already migrated")
	}
}

func TestRunLintsGo(t *testing.T) {
	ctx := context.Background()
	// Run only Go linter (go vet)
	opts := Options{Go: true}
	err := RunLints(ctx, opts)
	if err != nil {
		t.Errorf("go lint failed: %v", err)
	}
}
