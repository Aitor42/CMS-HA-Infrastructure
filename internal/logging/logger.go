package logging

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"time"
)

func init() {
	SetVerbose(false)
}

// SetVerbose sets the verbosity level for internal structured logging (slog).
// User-facing messages (Info/Success/Warn/Error) always print exactly once.
func SetVerbose(v bool) {
	level := slog.LevelInfo
	if v {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})))
}

// FormatMsg formats a message if format arguments are provided.
func FormatMsg(msg string, args ...any) string {
	if len(args) > 0 {
		return fmt.Sprintf(msg, args...)
	}
	return msg
}

// Info prints an info message in cyan.
// Every user-facing message is printed exactly once, no matter the verbosity:
// verbose mode only lowers the slog level used by internal structured logs.
func Info(msg string, args ...any) {
	fmt.Print(cyan("ℹ ") + FormatMsg(msg, args...) + "\n")
}

// Success prints a success message in green.
func Success(msg string, args ...any) {
	fmt.Print(green("✓ ") + FormatMsg(msg, args...) + "\n")
}

// Warn prints a warning message in yellow.
func Warn(msg string, args ...any) {
	fmt.Print(yellow("⚠ ") + FormatMsg(msg, args...) + "\n")
}

// Error prints an error message in red. Errors go to stderr so that they are
// separated from command output in pipelines and in CI logs.
func Error(msg string, args ...any) {
	_, _ = fmt.Fprintln(os.Stderr, red("✗ ")+FormatMsg(msg, args...))
}

// Debug records an internal diagnostic message through slog. It is only emitted
// in verbose mode (SetVerbose(true) lowers the level to Debug); user-facing
// progress must use Info/Success/Warn/Error so it always prints.
func Debug(msg string, args ...any) {
	if len(args) > 0 {
		slog.Debug(fmt.Sprintf(msg, args...))
		return
	}
	slog.Debug(msg)
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
