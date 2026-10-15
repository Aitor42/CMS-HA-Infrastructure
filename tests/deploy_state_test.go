package tests

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/deploy"
)

// The deploy checkpoint is black-box testable through the exported State API;
// the planning internals (phaseKey/planRun) stay in internal/deploy as a
// white-box suite (Go cannot reach unexported identifiers from another package).
func TestDeploy_StateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deploy.state")

	// A missing file yields an empty, usable state (first run).
	s, err := deploy.LoadState(path)
	if err != nil {
		t.Fatalf("LoadState on missing file: %v", err)
	}
	if len(s.CompletedPhases) != 0 || s.Version == 0 {
		t.Fatalf("unexpected empty state: %+v", s)
	}

	s.StartedAt = time.Now().UTC()
	if err := s.MarkCompleted(path, "00-init-vms"); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}
	if err := s.MarkCompleted(path, "00-init-vms"); err != nil {
		t.Fatalf("MarkCompleted (repeat): %v", err)
	}

	reloaded, err := deploy.LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(reloaded.CompletedPhases) != 1 || !reloaded.HasCompleted("00-init-vms") {
		t.Fatalf("checkpoint not persisted correctly: %+v", reloaded)
	}
	if reloaded.Status != "running" {
		t.Fatalf("status = %q, want running", reloaded.Status)
	}
}

func TestDeploy_StateRejectsWrongVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deploy.state")
	if err := os.WriteFile(path, []byte(`{"version":99}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := deploy.LoadState(path); err == nil {
		t.Fatal("expected an error for an unsupported state version")
	}
}
