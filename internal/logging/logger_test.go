package logging

import (
	"testing"
	"time"
)

func TestLoggingFormatting(t *testing.T) {
	msg := formatMsg("Testing %s %d", "number", 42)
	if msg != "Testing number 42" {
		t.Errorf("unexpected formatted message: %s", msg)
	}

	raw := formatMsg("Simple message without format args")
	if raw != "Simple message without format args" {
		t.Errorf("unexpected raw message: %s", raw)
	}
}

func TestVerbosityToggle(t *testing.T) {
	SetVerbose(true)
	Info("Verbose info test: %s", "active")
	Success("Verbose success test: %s", "active")
	Warn("Verbose warn test: %s", "active")

	SetVerbose(false)
	Info("Standard info test")
	Success("Standard success test")
	Warn("Standard warn test")
}

func TestPhaseTimer(t *testing.T) {
	timer := PhaseStart("Unit Test Phase")
	time.Sleep(5 * time.Millisecond)
	timer.End()
}
