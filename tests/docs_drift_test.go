package tests

import (
	"io/fs"
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

	frontend := read("puppet/modules/role/manifests/cms_frontend.pp")
	// The frontend must fetch a versioned WordPress archive and verify it:
	// latest.tar.gz is a moving target that can change the required PHP
	// version or the archive layout between two nodes of the same deploy.
	for _, line := range strings.Split(frontend, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue // comments may explain the old behaviour
		}
		if strings.Contains(line, "wordpress.org/latest.tar.gz") {
			t.Fatal("role::cms_frontend must download a versioned WordPress archive, not latest.tar.gz")
		}
	}
	wordpress := regexp.MustCompile(`(?m)^\s*\$wordpress_version\s*=\s*'([^']+)'`).FindStringSubmatch(frontend)
	if len(wordpress) != 2 {
		t.Fatal("could not parse the pinned WordPress version from role::cms_frontend")
	}
	if !strings.Contains(frontend, "sha256sum -c") {
		t.Fatal("role::cms_frontend must verify the WordPress archive checksum")
	}
	if !strings.Contains(baseline, "| **WordPress** | "+wordpress[1]) {
		t.Fatalf("SOFTWARE_BASELINE.md does not list the pinned WordPress version %s", wordpress[1])
	}

	// The PHP CVE floor lives twice: Puppet enforces it and verify checks it.
	// If they drift, one side accepts a vulnerable frontend or the other
	// rejects a patched one.
	phpFloor := regexp.MustCompile(`(?m)^\s*\$php_patched_version\s*=\s*'([^']+)'`).FindStringSubmatch(frontend)
	if len(phpFloor) != 2 {
		t.Fatal("could not parse the PHP security floor from role::cms_frontend")
	}
	if !strings.Contains(read("internal/verify/verify_functional.go"), `phpPatchedPackage = "`+phpFloor[1]+`"`) {
		t.Fatalf("internal/verify does not enforce the Puppet PHP floor %s", phpFloor[1])
	}
	if !strings.Contains(baseline, phpFloor[1]) {
		t.Fatalf("SOFTWARE_BASELINE.md does not list the PHP patch floor %s", phpFloor[1])
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

	// The pipeline runs weekly so a new Go standard-library advisory shows up
	// even when an idle `main` is not receiving pushes.
	if !strings.Contains(read(".github/workflows/ci.yml"), "schedule:") {
		t.Fatal("ci.yml must keep a scheduled run for advisories on an idle default branch")
	}
	// Runner images are pinned on purpose: `ubuntu-latest` moved to Ubuntu
	// 26.04 on 2026-11-14 and would change CI results without a commit.
	for _, name := range []string{".github/workflows/ci.yml", ".github/workflows/release.yml"} {
		if strings.Contains(read(name), "runs-on: ubuntu-latest") {
			t.Fatalf("%s tracks ubuntu-latest; pin the runner image instead", name)
		}
	}

	// The MariaDB and rclone images are repeated across manifests (StatefulSet,
	// init Job and both CronJobs). If one repo:tag resolved to two digests, the
	// backup would run different code from the database it dumps.
	digests := map[string]string{}
	err := filepath.WalkDir(filepath.Join("..", "kubernetes"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, ref := range regexp.MustCompile(`image:\s*(\S+)@(sha256:[a-f0-9]{64})`).FindAllStringSubmatch(string(body), -1) {
			if prev, ok := digests[ref[1]]; ok && prev != ref[2] {
				t.Fatalf("%s pins %s@%s but another manifest pins %s", path, ref[1], ref[2], prev)
			}
			digests[ref[1]] = ref[2]
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk kubernetes manifests: %v", err)
	}
	if len(digests) < 2 {
		t.Fatalf("expected MariaDB and rclone to be digest-pinned, found %d distinct images", len(digests))
	}
}
