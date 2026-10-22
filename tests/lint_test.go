package tests

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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

// TestLint_FindFiles uses a throwaway tree instead of the repository so the
// assertion only tests the walk itself (recursion, pattern matching and the
// skip list), not whether scripts/ still happens to contain a .sh file.
func TestLint_FindFiles(t *testing.T) {
	root := t.TempDir()
	files := []string{
		"a.sh",
		"b.txt",
		"nested/c.sh",
		".git/ignored.sh",
		"vendor/ignored.sh",
		"node_modules/ignored.sh",
		"testdata/ignored.sh",
	}
	for _, rel := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	got := lint.FindFiles(root, "*.sh")
	for i := range got {
		rel, err := filepath.Rel(root, got[i])
		if err != nil {
			t.Fatalf("relative path for %s: %v", got[i], err)
		}
		got[i] = rel
	}
	sort.Strings(got)

	want := []string{"a.sh", filepath.Join("nested", "c.sh")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindFiles = %v, want %v", got, want)
	}
}

func TestLint_FindFilesMultiBucketsInOneWalk(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"a.go", "b.sh", "nested/c.go", "docs/d.md"} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create %s: %v", rel, err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	got := lint.FindFilesMulti(root, "*.go", "*.sh", "*.md")
	for pattern, paths := range got {
		for i, path := range paths {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				t.Fatalf("relative path for %s: %v", path, err)
			}
			paths[i] = rel
		}
		sort.Strings(paths)
		got[pattern] = paths
	}

	want := map[string][]string{
		"*.go": {"a.go", filepath.Join("nested", "c.go")},
		"*.sh": {"b.sh"},
		"*.md": {filepath.Join("docs", "d.md")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindFilesMulti = %v, want %v", got, want)
	}
}

func TestLint_RunLintsGo(t *testing.T) {
	ctx := context.Background()
	// Run only the Go linter (go vet): CI runs the same entry point, so a
	// failure here is a real vet failure rather than a missing optional tool.
	opts := lint.Options{Go: true}
	if err := lint.RunLints(ctx, opts); err != nil {
		t.Errorf("go lint failed: %v", err)
	}
}
