package tests

import (
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
	logging.SetVerbose(true)
	logging.Info("Verbose info test: %s", "active")
	logging.Success("Verbose success test: %s", "active")
	logging.Warn("Verbose warn test: %s", "active")

	logging.SetVerbose(false)
	logging.Info("Standard info test")
	logging.Success("Standard success test")
	logging.Warn("Standard warn test")
}

func TestLogging_PhaseTimer(t *testing.T) {
	timer := logging.PhaseStart("Unit Test Phase")
	time.Sleep(5 * time.Millisecond)
	timer.End()
}
