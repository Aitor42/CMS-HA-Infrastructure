package logging

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"sync"
	"sync/atomic"
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

// runID is the current deployment/run identifier. It is attached to structured
// debug records so a log line can be correlated with timings.json and the
// checkpoint without parsing the human-readable prefix.
var runID atomic.Value

// SetRunID sets the identifier attached to subsequent structured log records.
func SetRunID(id string) {
	runID.Store(id)
}

// RunID returns the current identifier, or an empty string when none is set.
func RunID() string {
	if v := runID.Load(); v != nil {
		return v.(string)
	}
	return ""
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
		msg = fmt.Sprintf(msg, args...)
	}
	attrs := []any{}
	if id := RunID(); id != "" {
		attrs = append(attrs, "run_id", id)
	}
	slog.Debug(msg, attrs...)
}

// phaseObserver receives the name, start time and duration of every completed
// phase timer. It is used by the deploy package to persist timings.json.
var (
	phaseObserverMu sync.RWMutex
	phaseObserver   func(name string, start time.Time, duration time.Duration)
)

// SetPhaseObserver installs a global phase-timer observer. Passing nil removes
// it. Timers still print their normal output; the observer is an extra sink for
// structured measurements, not a replacement for the user-facing log.
func SetPhaseObserver(fn func(name string, start time.Time, duration time.Duration)) {
	phaseObserverMu.Lock()
	phaseObserver = fn
	phaseObserverMu.Unlock()
}

func notifyPhaseObserver(name string, start time.Time, duration time.Duration) {
	phaseObserverMu.RLock()
	observer := phaseObserver
	phaseObserverMu.RUnlock()
	if observer != nil {
		observer(name, start, duration)
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
	notifyPhaseObserver(t.name, t.start, duration)
	Success("Completed phase: %s in %s", t.name, FormatDuration(duration))
}

// FormatDuration renders a duration in human units with at most two decimal
// places (time.Duration.String prints all 9 available digits): "1h 2m 3.45s",
// "1m 9.00s", "12.34s" or "215.4ms" depending on the magnitude.
func FormatDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%.2fµs", float64(d.Nanoseconds())/1000)
	case d < time.Second:
		return fmt.Sprintf("%.1fms", float64(d.Nanoseconds())/1e6)
	}

	// Round once to hundredths of a second and derive every unit from that, so
	// the seconds field can never print as "60.00" (e.g. 59m 59.999s).
	totalCS := int64(math.Round(d.Seconds() * 100))
	hours := totalCS / 360000
	minutes := (totalCS % 360000) / 6000
	seconds := (totalCS % 6000) / 100
	centis := totalCS % 100

	switch {
	case hours > 0:
		return fmt.Sprintf("%dh %dm %d.%02ds", hours, minutes, seconds, centis)
	case minutes > 0:
		return fmt.Sprintf("%dm %d.%02ds", minutes, seconds, centis)
	default:
		return fmt.Sprintf("%d.%02ds", seconds, centis)
	}
}
