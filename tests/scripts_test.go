package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/lint"
)

// scriptsBanner marks a v1 shell script as deprecated reference-only material.
const scriptsBanner = "DEPRECATED (v1, reference only)"

// TestScripts_AreDeprecatedAndCatalogued keeps the v1/v2 duplication honest: the
// legacy scripts/ directory may only contain scripts that are both explicitly
// deprecated and catalogued in scripts/README.md. A new v1 script (or a stale
// catalogue entry) therefore fails CI instead of silently drifting from the Go
// orchestrator / Ansible / Puppet implementation.
func TestScripts_AreDeprecatedAndCatalogued(t *testing.T) {
	root, err := lint.FindModuleRoot()
	if err != nil {
		t.Fatalf("cannot locate module root: %v", err)
	}
	scriptsDir := filepath.Join(root, "scripts")

	readme, err := os.ReadFile(filepath.Join(scriptsDir, "README.md"))
	if err != nil {
		t.Fatalf("scripts/README.md is required to catalogue the deprecated scripts: %v", err)
	}
	readmeText := string(readme)

	found := 0
	walkErr := filepath.WalkDir(scriptsDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".sh") {
			return nil
		}
		found++

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(content), scriptsBanner) {
			t.Errorf("%s is missing the deprecation banner", rel)
		}
		if !strings.Contains(readmeText, rel) {
			t.Errorf("%s is not catalogued in scripts/README.md", rel)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk scripts/: %v", walkErr)
	}
	if found == 0 {
		t.Fatal("expected to find the v1 shell scripts under scripts/")
	}

	// Every catalogued script must still exist (no stale entries). The mapping
	// table rows look like: | `scripts/00_init_vms.sh` | ... |
	for _, line := range strings.Split(readmeText, "\n") {
		const marker = "| `scripts/"
		if !strings.HasPrefix(line, marker) {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "| `"))
		if len(fields) == 0 {
			continue
		}
		rel := strings.TrimSuffix(fields[0], "`")
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("scripts/README.md lists %s but it does not exist", rel)
		}
	}
}
