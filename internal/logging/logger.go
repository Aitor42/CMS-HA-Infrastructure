package logging

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/fatih/color"
)

var (
	verbose bool
	logger  *slog.Logger
)

func init() {
	SetVerbose(false)
}

// SetVerbose sets the verbosity level for logging.
func SetVerbose(v bool) {
	verbose = v
	level := slog.LevelInfo
	if v {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{
		Level: level,
	}
	logger = slog.New(slog.NewTextHandler(os.Stdout, opts))
	slog.SetDefault(logger)
}

// FormatMsg formats a message if format arguments are provided.
func FormatMsg(msg string, args ...any) string {
	if len(args) > 0 {
		return fmt.Sprintf(msg, args...)
	}
	return msg
}

// Info prints an info message in cyan.
func Info(msg string, args ...any) {
	formatted := FormatMsg(msg, args...)
	fmt.Print(color.CyanString("ℹ ") + formatted + "\n")
	if verbose {
		logger.Info(formatted)
	}
}

// Success prints a success message in green.
func Success(msg string, args ...any) {
	formatted := FormatMsg(msg, args...)
	fmt.Print(color.GreenString("✓ ") + formatted + "\n")
	if verbose {
		logger.Info(formatted)
	}
}

// Warn prints a warning message in yellow.
func Warn(msg string, args ...any) {
	formatted := FormatMsg(msg, args...)
	fmt.Print(color.YellowString("⚠ ") + formatted + "\n")
	if verbose {
		logger.Warn(formatted)
	}
}

// Error prints an error message in red.
func Error(msg string, args ...any) {
	formatted := FormatMsg(msg, args...)
	fmt.Print(color.RedString("✗ ") + formatted + "\n")
	if verbose {
		logger.Error(formatted)
	}
}

// PhaseTimer measures and prints the duration of a phase.
type PhaseTimer struct {
	name  string
	start time.Time
}

// PhaseStart starts a new phase timer.
func PhaseStart(name string) *PhaseTimer {
	Info("Starting phase: %s", name)
	return &PhaseTimer{
		name:  name,
		start: time.Now(),
	}
}

// End finishes the phase timer and prints the duration.
func (t *PhaseTimer) End() {
	duration := time.Since(t.start)
	Success("Completed phase: %s in %v", t.name, duration)
}

// ProgressLine prints an inline progress message, overwriting the current line.
func ProgressLine(format string, args ...any) {
	fmt.Printf("\r\033[K"+format, args...)
}
