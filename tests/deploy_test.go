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

	// Full list with VM creation
	phasesAll := orch.BuildPhaseList(deploy.DeployOpts{SkipVMCreate: false})
	if len(phasesAll) != 11 {
		t.Errorf("expected 11 phases, got %d", len(phasesAll))
	}

	// List skipping VM creation
	phasesSkipVM := orch.BuildPhaseList(deploy.DeployOpts{SkipVMCreate: true})
	if len(phasesSkipVM) != 10 {
		t.Errorf("expected 10 phases, got %d", len(phasesSkipVM))
	}

	// Test DryRun
	err := orch.Deploy(context.Background(), deploy.DeployOpts{SkipVMCreate: true, DryRun: true})
	if err != nil {
		t.Errorf("expected dry-run to succeed, got %v", err)
	}
}
