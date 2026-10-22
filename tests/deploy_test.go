package tests

import (
	"context"
	"testing"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/deploy"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
)

func TestDeploy_BuildPhaseList(t *testing.T) {
	cfg := &config.Config{
		Deploy: config.DeployConfig{
			StaggerDelay: 1 * time.Millisecond,
		},
	}
	orch := &deploy.Orchestrator{
		Cfg:     cfg,
		Libvirt: &libvirt.Client{},
	}

	// Build both variants so the assertions can compare them.
	phasesAll := orch.BuildPhaseList(deploy.DeployOpts{SkipVMCreate: false})
	phasesSkipVM := orch.BuildPhaseList(deploy.DeployOpts{SkipVMCreate: true})

	// Invariants instead of brittle exact counts: skipping VM creation must
	// drop the init-vms related phases (inserted in two places) and keep the
	// remaining phases in the same relative order.
	if len(phasesAll)-len(phasesSkipVM) != 2 {
		t.Errorf("expected exactly 2 more phases with SkipVMCreate=false, got %d vs %d", len(phasesAll), len(phasesSkipVM))
	}
	if len(phasesAll) < 10 {
		t.Errorf("unexpectedly small phase list: %d", len(phasesAll))
	}
	j := 0
	for _, ph := range phasesAll {
		if j < len(phasesSkipVM) && ph.Name() == phasesSkipVM[j].Name() {
			j++
		}
	}
	if j != len(phasesSkipVM) {
		t.Errorf("skip-VM phase list is not a clean subsequence of the full list (matched %d of %d)", j, len(phasesSkipVM))
	}

	// A dry run must return without touching libvirt.
	err := orch.Deploy(context.Background(), deploy.DeployOpts{SkipVMCreate: true, DryRun: true})
	if err != nil {
		t.Errorf("expected dry-run to succeed, got %v", err)
	}
}
