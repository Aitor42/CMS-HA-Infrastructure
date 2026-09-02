package deploy

import (
	"context"
	"testing"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
)

func TestBuildPhaseList(t *testing.T) {
	cfg := &config.Config{
		Deploy: config.DeployConfig{
			StaggerDelay: 1 * time.Millisecond,
		},
	}
	orch := &Orchestrator{
		Cfg:     cfg,
		Libvirt: &libvirt.Client{},
	}

	// Full list with VM creation
	phasesAll := orch.buildPhaseList(DeployOpts{SkipVMCreate: false})
	if len(phasesAll) != 11 {
		t.Errorf("expected 11 phases, got %d", len(phasesAll))
	}

	// List skipping VM creation
	phasesSkipVM := orch.buildPhaseList(DeployOpts{SkipVMCreate: true})
	if len(phasesSkipVM) != 10 {
		t.Errorf("expected 10 phases, got %d", len(phasesSkipVM))
	}

	// Test DryRun
	err := orch.Deploy(context.Background(), DeployOpts{SkipVMCreate: true, DryRun: true})
	if err != nil {
		t.Errorf("expected dry-run to succeed, got %v", err)
	}
}
