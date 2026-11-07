package tests

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/lint"
)

// TestLegacyShellSuiteRemoved keeps the v1 cleanup from regressing. The old
// scripts are still available in git history, and the mapping document is the
// only supported reference.
func TestLegacyShellSuiteRemoved(t *testing.T) {
	root, err := lint.FindModuleRoot()
	if err != nil {
		t.Fatalf("cannot locate module root: %v", err)
	}
	for _, rel := range []string{"scripts", "deploy_all.sh"} {
		if _, err := os.Stat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Errorf("%s must not exist in the v2 tree (stat error: %v)", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "LEGACY-V1-MAPPING.md")); err != nil {
		t.Fatalf("docs/LEGACY-V1-MAPPING.md must preserve the v1 → v2 mapping: %v", err)
	}
}
