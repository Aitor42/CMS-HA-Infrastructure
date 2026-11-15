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
	// The patch level is part of the security floor: go-version-file feeds it
	// to every CI job, so an implicit patch would build an unfixed standard
	// library. Require an explicit x.y.z directive.
	goVersion := regexp.MustCompile(`(?m)^go\s+(\d+\.\d+\.\d+)\s*$`).FindStringSubmatch(goMod)
	if len(goVersion) != 2 {
		t.Fatal("go.mod must pin an explicit Go patch version (go x.y.z)")
	}
	goMinor := goVersion[1][:strings.LastIndex(goVersion[1], ".")]
	makefile := read("Makefile")
	if !strings.Contains(makefile, "Go "+goMinor+"+") {
		t.Fatalf("Makefile prerequisites do not mention Go %s+", goMinor)
	}

	monitor := read("puppet/modules/role/manifests/monitor.pp")
	grafana := regexp.MustCompile(`(?m)^\s*\$grafana_version\s*=\s*'([^']+)'`).FindStringSubmatch(monitor)
	if len(grafana) != 2 {
		t.Fatal("could not parse the pinned Grafana package version")
	}
	baseline := read("docs/SOFTWARE_BASELINE.md")
	if !strings.Contains(baseline, "| **cms-ha (Go CLI)** | Go "+goVersion[1]+" |") {
		t.Fatalf("SOFTWARE_BASELINE.md does not list the pinned Go version %s", goVersion[1])
	}
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
	// The manifest validators must target the same Kubernetes minor as the
	// pinned K3s release; validating against an older schema hides fields that
	// the deployed API server would reject.
	kubeVersion := strings.TrimPrefix(strings.Split(k3sVersion[1], "+")[0], "v")
	for name, body := range map[string]string{
		"Makefile":              makefile,
		"internal/lint/lint.go": read("internal/lint/lint.go"),
	} {
		if !strings.Contains(body, kubeVersion) {
			t.Fatalf("%s does not validate manifests against Kubernetes %s", name, kubeVersion)
		}
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
