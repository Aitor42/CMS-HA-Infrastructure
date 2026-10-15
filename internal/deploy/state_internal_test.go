package deploy

import (
	"context"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
)

type fakePhase struct{ name string }

func (f fakePhase) Name() string              { return f.name }
func (f fakePhase) Description() string       { return f.name }
func (f fakePhase) Run(context.Context) error { return nil }

func testPhases() []phases.Phase {
	return []phases.Phase{
		fakePhase{"00-init-vms"},
		fakePhase{"DRBD Setup"},
		fakePhase{"Kubernetes Setup"},
	}
}

func TestPhaseKey(t *testing.T) {
	cases := map[string]string{
		"00-init-vms":             "00-init-vms",
		"DRBD Setup":              "drbd-setup",
		"NGINX & WordPress Setup": "nginx-wordpress-setup",
		"  Kubernetes Setup  ":    "kubernetes-setup",
	}
	for in, want := range cases {
		if got := phaseKey(in); got != want {
			t.Errorf("phaseKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlanRun(t *testing.T) {
	list := testPhases()

	t.Run("fresh run resets the checkpoint", func(t *testing.T) {
		skip, reset, err := planRun(list, DeployOpts{}, &State{})
		if err != nil {
			t.Fatal(err)
		}
		if !reset || len(skip) != 0 {
			t.Fatalf("expected reset with nothing skipped, got reset=%v skip=%v", reset, skip)
		}
	})

	t.Run("resume skips completed phases", func(t *testing.T) {
		prior := &State{CompletedPhases: []string{"00-init-vms", "drbd-setup"}}
		skip, reset, err := planRun(list, DeployOpts{Resume: true}, prior)
		if err != nil {
			t.Fatal(err)
		}
		if reset {
			t.Fatal("resume must not reset the checkpoint")
		}
		if !skip["00-init-vms"] || !skip["drbd-setup"] || skip["kubernetes-setup"] {
			t.Fatalf("unexpected skip set: %v", skip)
		}
	})

	t.Run("from-phase skips the phases before it", func(t *testing.T) {
		skip, _, err := planRun(list, DeployOpts{FromPhase: "kubernetes-setup"}, &State{})
		if err != nil {
			t.Fatal(err)
		}
		if !skip["00-init-vms"] || !skip["drbd-setup"] || skip["kubernetes-setup"] {
			t.Fatalf("unexpected skip set: %v", skip)
		}
	})

	t.Run("from-phase accepts the display name", func(t *testing.T) {
		skip, _, err := planRun(list, DeployOpts{FromPhase: "DRBD Setup"}, &State{})
		if err != nil {
			t.Fatal(err)
		}
		if !skip["00-init-vms"] || skip["drbd-setup"] {
			t.Fatalf("unexpected skip set: %v", skip)
		}
	})

	t.Run("unknown phase lists the valid ones", func(t *testing.T) {
		_, _, err := planRun(list, DeployOpts{FromPhase: "nope"}, &State{})
		if err == nil {
			t.Fatal("expected an error for an unknown phase")
		}
	})

	t.Run("resume and from-phase are mutually exclusive", func(t *testing.T) {
		if _, _, err := planRun(list, DeployOpts{Resume: true, FromPhase: "drbd-setup"}, &State{}); err == nil {
			t.Fatal("expected an error when both flags are set")
		}
	})
}
