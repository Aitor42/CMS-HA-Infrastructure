package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// timingsVersion is the on-disk schema version of the timings report.
const timingsVersion = 1

// topLevelTimer is the name of the timer wrapped around the whole deployment.
// It is not a phase, so the recorder ignores it: its duration is total_ms.
const topLevelTimer = "Full Infrastructure Deployment"

// PhaseTiming is one entry of the phase list in timings.json.
type PhaseTiming struct {
	Phase  string `json:"phase"`
	Status string `json:"status"` // ok | skipped | dry-run | failed
	// StartedAt is a pointer so skipped/dry-run entries omit it entirely;
	// encoding/json does not apply omitempty to a zero time.Time struct.
	StartedAt  *time.Time `json:"started_at,omitempty"`
	DurationMS int64      `json:"duration_ms"`
	Error      string     `json:"error,omitempty"`
}

// StepTiming is a nested logging.PhaseStart timer (a sub-step of a phase, e.g.
// "Install By Batches" or "Fix Boot Order"). Its start time lets a report group
// steps under the phase whose interval contains them.
type StepTiming struct {
	Step       string    `json:"step"`
	StartedAt  time.Time `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`
}

// Timings is the JSON document written by --timings-file.
type Timings struct {
	Version   int           `json:"version"`
	Config    string        `json:"config,omitempty"`
	StartedAt time.Time     `json:"started_at"`
	TotalMS   int64         `json:"total_ms"`
	Status    string        `json:"status"` // completed | failed
	Phases    []PhaseTiming `json:"phases"`
	Steps     []StepTiming  `json:"steps,omitempty"`
	SSH       ssh.Stats     `json:"ssh"`
}

// timingsRecorder collects phase and sub-step durations for one deployment and
// persists them as JSON. A nil recorder is valid and all its methods are no-ops,
// so the orchestrator does not need to branch on --timings-file being present.
type timingsRecorder struct {
	mu      sync.Mutex
	path    string
	config  string
	start   time.Time
	phases  []PhaseTiming
	steps   []StepTiming
	current string
	pool    *ssh.Pool
}

// newTimingsRecorder starts recording. It returns nil when path is empty.
func newTimingsRecorder(path, configName string, pool *ssh.Pool) *timingsRecorder {
	if path == "" {
		return nil
	}
	r := &timingsRecorder{
		path:   path,
		config: configName,
		start:  time.Now().UTC(),
		pool:   pool,
	}
	logging.SetPhaseObserver(r.observe)
	return r
}

// setCurrent marks the phase being executed so the observer can discard the
// phase's own PhaseTimer (the orchestrator records it explicitly, with the
// skipped/dry-run statuses the timer cannot know about).
func (r *timingsRecorder) setCurrent(name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.current = name
	r.mu.Unlock()
}

func (r *timingsRecorder) clearCurrent() {
	r.setCurrent("")
}

// observe records a completed logging.PhaseStart timer as a sub-step.
func (r *timingsRecorder) observe(name string, start time.Time, duration time.Duration) {
	if r == nil || name == topLevelTimer {
		return
	}
	r.mu.Lock()
	current := r.current
	r.mu.Unlock()
	if name == current {
		return
	}
	r.step(name, start, duration)
}

// step appends an arbitrary measured sub-step (for work that has no
// logging.PhaseTimer, e.g. the inter-phase stagger delay).
func (r *timingsRecorder) step(name string, start time.Time, duration time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, StepTiming{
		Step:       name,
		StartedAt:  start.UTC(),
		DurationMS: duration.Milliseconds(),
	})
}

// phase records the outcome of one orchestrated phase. skipped and dry-run
// phases carry a zero duration but still appear, so a report can tell "not run"
// apart from "missing".
func (r *timingsRecorder) phase(name, status string, start time.Time, duration time.Duration, err error) {
	if r == nil {
		return
	}
	entry := PhaseTiming{
		Phase:      name,
		Status:     status,
		DurationMS: duration.Milliseconds(),
	}
	if !start.IsZero() {
		started := start.UTC()
		entry.StartedAt = &started
	}
	if err != nil {
		entry.Error = err.Error()
	}
	r.mu.Lock()
	r.phases = append(r.phases, entry)
	r.mu.Unlock()
}

// write stops observing and persists the report atomically (temp + rename).
func (r *timingsRecorder) write(status string) error {
	if r == nil {
		return nil
	}
	logging.SetPhaseObserver(nil)

	r.mu.Lock()
	doc := Timings{
		Version:   timingsVersion,
		Config:    r.config,
		StartedAt: r.start,
		TotalMS:   time.Since(r.start).Milliseconds(),
		Status:    status,
		Phases:    append([]PhaseTiming(nil), r.phases...),
		Steps:     append([]StepTiming(nil), r.steps...),
	}
	r.mu.Unlock()

	if r.pool != nil {
		doc.SSH = r.pool.Stats()
	}

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode timings: %w", err)
	}
	if dir := filepath.Dir(r.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create timings directory %s: %w", dir, err)
		}
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write timings file %s: %w", r.path, err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return fmt.Errorf("commit timings file %s: %w", r.path, err)
	}
	return nil
}
