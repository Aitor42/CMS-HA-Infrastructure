package tests

import (
	"path"
	"strings"
	"testing"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"gopkg.in/yaml.v3"
)

// kustomization is the subset of a kustomization.yaml used by the manifests.
type kustomization struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Resources  []string `yaml:"resources"`
	Patches    []struct {
		Path string `yaml:"path"`
	} `yaml:"patches"`
}

// TestKustomize_Structure verifies that kubernetes/ is a coherent kustomize
// tree: every kustomization is valid YAML and every referenced resource/patch
// file exists in the embedded filesystem, so `kubectl apply -k` cannot fail on
// a missing file.
func TestKustomize_Structure(t *testing.T) {
	roots := []string{
		"kubernetes/base",
		"kubernetes/backups",
		"kubernetes/jobs",
		"kubernetes/monitoring",
		"kubernetes/overlays/lab",
	}

	for _, dir := range roots {
		dir := dir
		t.Run(dir, func(t *testing.T) {
			kfile := path.Join(dir, "kustomization.yaml")
			raw, err := cms.KubernetesFS.ReadFile(kfile)
			if err != nil {
				t.Fatalf("missing %s: %v", kfile, err)
			}

			var k kustomization
			if err := yaml.Unmarshal(raw, &k); err != nil {
				t.Fatalf("%s is not valid YAML: %v", kfile, err)
			}
			if k.APIVersion == "" || k.Kind != "Kustomization" {
				t.Fatalf("%s must be a kustomize Kustomization (apiVersion/kind)", kfile)
			}
			if len(k.Resources) == 0 {
				t.Fatalf("%s must list at least one resource", kfile)
			}

			exists := func(rel string) bool {
				resolved := path.Clean(path.Join(dir, rel))
				// A directory reference (e.g. ../../base) resolves to its
				// kustomization.yaml.
				if _, err := cms.KubernetesFS.ReadFile(path.Join(resolved, "kustomization.yaml")); err == nil {
					return true
				}
				if _, err := cms.KubernetesFS.ReadFile(resolved); err == nil {
					return true
				}
				return false
			}

			for _, r := range k.Resources {
				if !exists(r) {
					t.Errorf("%s references %q which does not exist", kfile, r)
				}
			}
			for _, p := range k.Patches {
				if p.Path != "" && !exists(p.Path) {
					t.Errorf("%s references patch %q which does not exist", kfile, p.Path)
				}
			}
		})
	}

	t.Run("lab overlay composes the base", func(t *testing.T) {
		raw, err := cms.KubernetesFS.ReadFile("kubernetes/overlays/lab/kustomization.yaml")
		if err != nil {
			t.Fatalf("missing lab overlay kustomization: %v", err)
		}
		var k kustomization
		if err := yaml.Unmarshal(raw, &k); err != nil {
			t.Fatalf("lab overlay is not valid YAML: %v", err)
		}
		found := false
		for _, r := range k.Resources {
			if r == "../../base" {
				found = true
			}
		}
		if !found {
			t.Error("the lab overlay must compose ../../base")
		}
	})

	// The Prometheus scrape RBAC is declared here (not in Go) but must stay
	// least-privilege: a cluster-admin binding would be a privilege escalation.
	t.Run("prometheus RBAC is read-only and cluster-admin-free", func(t *testing.T) {
		raw, err := cms.KubernetesFS.ReadFile("kubernetes/monitoring/prometheus-rbac.yaml")
		if err != nil {
			t.Fatalf("missing prometheus-rbac.yaml: %v", err)
		}
		text := string(raw)
		for _, want := range []string{
			"kind: ServiceAccount",
			"kind: ClusterRole",
			"kind: ClusterRoleBinding",
			"prometheus-metrics-reader",
			"namespace: kube-system",
			"nonResourceURLs: [\"/metrics\"]",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("prometheus-rbac.yaml missing %q", want)
			}
		}
		if strings.Contains(text, "cluster-admin") {
			t.Error("prometheus-rbac.yaml must not bind cluster-admin")
		}
		if strings.Contains(text, `verbs: ["*"]`) || strings.Contains(text, "verbs: ['*']") {
			t.Error("prometheus-rbac.yaml must not grant wildcard verbs")
		}
	})
}
