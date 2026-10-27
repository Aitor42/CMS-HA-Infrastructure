package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coverage.out")
	content := "mode: set\n" +
		"github.com/Aitor42/CMS-HA-Infrastructure/internal/config/a.go:10.1,12.2 2 1\n" +
		"github.com/Aitor42/CMS-HA-Infrastructure/internal/config/a.go:20.1,22.2 1 0\n" +
		"example.com/other/b.go:1.1,2.2 1 1\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	got, err := readProfile(path, "github.com/Aitor42/CMS-HA-Infrastructure")
	if err != nil {
		t.Fatalf("readProfile: %v", err)
	}
	blocks := got["internal/config/a.go"]
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2: %+v", len(blocks), blocks)
	}
	if blocks[0] != (block{start: 10, end: 12, count: 1}) {
		t.Fatalf("first block = %+v", blocks[0])
	}
	if blocks[1] != (block{start: 20, end: 22, count: 0}) {
		t.Fatalf("second block = %+v", blocks[1])
	}
}
