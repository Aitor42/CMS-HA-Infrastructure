package tests

import (
	"log/slog"
	"testing"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
)

func TestLogging_Formatting(t *testing.T) {
	msg := logging.FormatMsg("Testing %s %d", "number", 42)
	if msg != "Testing number 42" {
		t.Errorf("unexpected formatted message: %s", msg)
	}

	raw := logging.FormatMsg("Simple message without format args")
	if raw != "Simple message without format args" {
		t.Errorf("unexpected raw message: %s", raw)
	}
}

func TestLogging_VerbosityToggle(t *testing.T) {
	// Assert on the actual effect instead of only calling it: SetVerbose must
	// toggle the default slog handler's verbosity level.
	logging.SetVerbose(false)
	t.Cleanup(func() { logging.SetVerbose(false) })

	if slog.Default().Enabled(t.Context(), slog.LevelDebug) {
		t.Error("expected debug logs disabled in quiet mode")
	}
	logging.Info("Quiet info test")

	logging.SetVerbose(true)
	if !slog.Default().Enabled(t.Context(), slog.LevelDebug) {
		t.Error("expected debug logs enabled in verbose mode")
	}
	logging.Info("Verbose info test: %s", "active")
	logging.Success("Verbose success test: %s", "active")
	logging.Warn("Verbose warn test: %s", "active")
}

func TestLogging_PhaseTimer(t *testing.T) {
	timer := logging.PhaseStart("Unit Test Phase")
	time.Sleep(5 * time.Millisecond)
	timer.End()
}
