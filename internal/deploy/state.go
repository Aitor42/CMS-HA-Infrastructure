package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
)

// stateVersion is the on-disk schema version of the checkpoint file.
const stateVersion = 1

// DefaultStateFile is where the orchestrator records phase progress unless it
// is overridden with --state-file.
const DefaultStateFile = "deploy.state"

// State is the orchestrator checkpoint persisted between runs. It lets a failed
// deployment resume from the last successful phase instead of redoing work
// (notably re-running VM creation and the Puppet convergence).
type State struct {
	Version         int       `json:"version"`
	RunID           string    `json:"run_id,omitempty"`
	Status          string    `json:"status"` // running | completed | failed
	StartedAt       time.Time `json:"started_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	CompletedPhases []string  `json:"completed_phases"`
	LastError       string    `json:"last_error,omitempty"`
}

// newRunID returns a sortable, unique-enough identifier for one deploy
// invocation. The timestamp makes runs easy to order in an artifact list; the
// nanosecond suffix keeps two runs started in the same second distinct.
func newRunID() string {
	now := time.Now().UTC()
	return fmt.Sprintf("%s-%06d", now.Format("20060102T150405Z"), now.Nanosecond()%1000000)
}

// LoadState reads a checkpoint file. A missing file is not an error: it returns
// a fresh state so the first run starts from scratch.
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if os.IsNotExist(err) {
		return &State{Version: stateVersion}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state file %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse state file %s: %w", path, err)
	}
	if s.Version != stateVersion {
		return nil, fmt.Errorf("unsupported state file version %d in %s (want %d)", s.Version, path, stateVersion)
	}
	return &s, nil
}

// Save writes the checkpoint atomically (temp file + rename) with 0600, so an
// interrupted deploy never leaves a half-written state file behind.
func (s *State) Save(path string) error {
	s.Version = stateVersion
	s.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("write state file %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("commit state file %s: %w", path, err)
	}
	return nil
}

// HasCompleted reports whether the phase key already completed in this or a
// previous run.
func (s *State) HasCompleted(key string) bool {
	for _, p := range s.CompletedPhases {
		if p == key {
			return true
		}
	}
	return false
}

// MarkCompleted records a phase key as done and persists the checkpoint.
func (s *State) MarkCompleted(path, key string) error {
	if !s.HasCompleted(key) {
		s.CompletedPhases = append(s.CompletedPhases, key)
	}
	s.Status = "running"
	s.LastError = ""
	return s.Save(path)
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// phaseKey normalises a phase name (or a --from-phase argument) to a stable,
// comparable key: lowercase, non-alphanumeric runs collapsed to a single dash.
// "00-init-vms" -> "00-init-vms", "DRBD Setup" -> "drbd-setup".
func phaseKey(name string) string {
	return strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

// PhaseKeys returns the run keys of every phase, in execution order.
func PhaseKeys(list []phases.Phase) []string {
	keys := make([]string, 0, len(list))
	for _, p := range list {
		keys = append(keys, phaseKey(p.Name()))
	}
	return keys
}

// planRun decides which phases to skip for this invocation.
//
//   - --resume     : skip every phase already recorded in the checkpoint.
//   - --from-phase : skip everything before the named phase.
//   - neither      : run everything and reset the checkpoint (reset == true).
//
// The returned reset flag tells the caller to start a fresh checkpoint.
func planRun(list []phases.Phase, opts DeployOpts, prior *State) (skip map[string]bool, reset bool, err error) {
	skip = map[string]bool{}

	if opts.Resume && opts.FromPhase != "" {
		return nil, false, fmt.Errorf("--resume and --from-phase are mutually exclusive")
	}

	switch {
	case opts.Resume:
		if prior != nil {
			for _, key := range prior.CompletedPhases {
				skip[key] = true
			}
		}
		return skip, false, nil

	case opts.FromPhase != "":
		want := phaseKey(opts.FromPhase)
		if want == "" {
			return nil, false, fmt.Errorf("--from-phase requires a phase name")
		}
		for i, p := range list {
			if phaseKey(p.Name()) == want {
				for _, before := range list[:i] {
					skip[phaseKey(before.Name())] = true
				}
				return skip, false, nil
			}
		}
		return nil, false, fmt.Errorf("unknown phase %q (valid phases: %s)", opts.FromPhase, strings.Join(PhaseKeys(list), ", "))

	default:
		return skip, true, nil
	}
}
