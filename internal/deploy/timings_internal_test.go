package deploy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
)

func TestTimingsRecorderWritesPhasesAndSteps(t *testing.T) {
	t.Cleanup(func() { logging.SetPhaseObserver(nil) })

	path := filepath.Join(t.TempDir(), "nested", "timings.json")
	rec := newTimingsRecorder(path, "config.ci.yaml", nil)
	if rec == nil {
		t.Fatal("expected a recorder when a path is configured")
	}

	start := time.Now().Add(-3 * time.Second)
	rec.setCurrent("01-setup-cobbler")
	// The phase's own timer must not be duplicated as a sub-step, while a
	// genuinely nested timer must be captured through the logging observer.
	logging.PhaseStart("01-setup-cobbler").End()
	logging.PhaseStart("Install By Batches").End()
	rec.step("stagger", start, 5*time.Second)
	rec.clearCurrent()

	rec.phase("01-setup-cobbler", "ok", start, 3*time.Second, nil)
	rec.phase("02-register-nodes", "skipped", time.Time{}, 0, nil)
	rec.phase("03-repair-ssh", "failed", start, time.Second, os.ErrDeadlineExceeded)

	if err := rec.write("failed"); err != nil {
		t.Fatalf("write timings: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read timings: %v", err)
	}
	var doc Timings
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse timings: %v", err)
	}

	if doc.Version != timingsVersion {
		t.Errorf("version = %d, want %d", doc.Version, timingsVersion)
	}
	if doc.Config != "config.ci.yaml" {
		t.Errorf("config = %q, want config.ci.yaml", doc.Config)
	}
	if doc.Status != "failed" {
		t.Errorf("status = %q, want failed", doc.Status)
	}
	if len(doc.Phases) != 3 {
		t.Fatalf("phases = %d, want 3", len(doc.Phases))
	}
	if got := doc.Phases[1].Status; got != "skipped" {
		t.Errorf("phase 2 status = %q, want skipped", got)
	}
	if doc.Phases[2].Error == "" {
		t.Error("failed phase should carry its error")
	}
	if len(doc.Steps) != 2 {
		t.Fatalf("steps = %d, want 2 (nested timer + stagger)", len(doc.Steps))
	}
	if doc.Steps[0].Step != "Install By Batches" {
		t.Errorf("first step = %q, want Install By Batches", doc.Steps[0].Step)
	}
	if doc.Steps[1].Step != "stagger" {
		t.Errorf("second step = %q, want stagger", doc.Steps[1].Step)
	}
	if doc.TotalMS < 0 {
		t.Errorf("total_ms = %d, want >= 0", doc.TotalMS)
	}
}

func TestTimingsRecorderDisabledWithoutPath(t *testing.T) {
	rec := newTimingsRecorder("", "", nil)
	if rec != nil {
		t.Fatal("expected a nil recorder when no path is configured")
	}
	// Nil-safe methods are part of the contract.
	rec.setCurrent("x")
	rec.observe("x", time.Now(), time.Second)
	rec.phase("x", "ok", time.Now(), time.Second, nil)
	rec.step("x", time.Now(), time.Second)
	rec.clearCurrent()
	if err := rec.write("completed"); err != nil {
		t.Fatalf("nil recorder write: %v", err)
	}
}
