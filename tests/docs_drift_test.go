package tests

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDocumentationTracksPinnedVersions keeps the human-readable baseline in
// sync with the values that actually drive the lab. It is deliberately a
// narrow set of checks: versions that have drifted before (Go, Grafana, K3s)
// and stale implementation names that used to be documented as current.
func TestDocumentationTracksPinnedVersions(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(data)
	}

	goMod := read("go.mod")
	goVersion := regexp.MustCompile(`(?m)^go\s+(\d+\.\d+)`).FindStringSubmatch(goMod)
	if len(goVersion) != 2 {
		t.Fatal("could not parse the Go version from go.mod")
	}
	makefile := read("Makefile")
	if !strings.Contains(makefile, "Go "+goVersion[1]+"+") {
		t.Fatalf("Makefile prerequisites do not mention Go %s+", goVersion[1])
	}

	monitor := read("puppet/modules/role/manifests/monitor.pp")
	grafana := regexp.MustCompile(`(?s)package\s*\{\s*'grafana'\s*:\s*ensure\s*=>\s*'([^']+)'`).FindStringSubmatch(monitor)
	if len(grafana) != 2 {
		t.Fatal("could not parse the pinned Grafana package version")
	}
	baseline := read("docs/SOFTWARE_BASELINE.md")
	if !strings.Contains(baseline, "| **Grafana** | "+grafana[1]+" |") {
		t.Fatalf("SOFTWARE_BASELINE.md does not list Grafana %s", grafana[1])
	}

	k3sSource := read("internal/phases/kubernetes/kubernetes.go")
	k3sVersion := regexp.MustCompile(`k3sVersion\s*=\s*"(v[^"]+)"`).FindStringSubmatch(k3sSource)
	if len(k3sVersion) != 2 {
		t.Fatal("could not parse the pinned K3s version")
	}
	if !strings.Contains(baseline, "| **K3s** | "+k3sVersion[1]+" |") {
		t.Fatalf("SOFTWARE_BASELINE.md does not list the pinned K3s version %s", k3sVersion[1])
	}

	readme := read("README.md")
	for _, stale := range []string{"viper", "internal/templates"} {
		if strings.Contains(strings.ToLower(readme), stale) {
			t.Fatalf("README.md still mentions stale implementation detail %q", stale)
		}
	}
	if !strings.Contains(readme, "validates pushes to `main`/`feature/rewrite-in-go`") {
		t.Fatal("README.md CI trigger description does not match ci.yml")
	}
}
