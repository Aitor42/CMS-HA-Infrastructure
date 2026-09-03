package tests

import (
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/cobbler"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/drbd"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/initvms"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/kubernetes"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/monitoring"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/nginx"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/pki"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/puppet"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/registernodes"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/repairssh"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/traffic"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/ufw"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

func TestPhases_MetadataAndInstantiation(t *testing.T) {
	cfg := &config.Config{
		Nodes: config.NodesConfig{
			Jumpstart: config.NodeDetail{Name: "jumpstart", IP: "192.168.10.10"},
			Masters: []config.NodeDetail{
				{Name: "internal-master1", IP: "192.168.10.11"},
				{Name: "internal-master2", IP: "192.168.10.12"},
			},
			Workers: []config.NodeDetail{
				{Name: "internal-worker1", IP: "192.168.10.13"},
			},
			Router:  config.NodeDetail{Name: "ufw-router", IP: "192.168.10.1"},
			Storage: config.NodeDetail{Name: "internal-storage", IP: "192.168.10.15"},
			Monitor: config.NodeDetail{Name: "internal-monitor", IP: "192.168.10.20"},
			LB:      config.NodeDetail{Name: "main-lb", IP: "192.168.20.100"},
			CMSFrontends: []config.NodeDetail{
				{Name: "main-cms1", IP: "192.168.20.101"},
			},
		},
	}
	pool := &ssh.Pool{}
	lv := libvirt.NewClient("test:///default")

	testCases := []struct {
		phaseName string
		phase     phases.Phase
	}{
		{"00-init-vms", initvms.NewPhase(cfg, pool, lv)},
		{"01-setup-cobbler", cobbler.NewPhase(cfg, pool)},
		{"02-register-nodes", registernodes.NewPhase(cfg, pool)},
		{"03-repair-ssh", repairssh.NewPhase(cfg, pool)},
		{"04-setup-puppet", puppet.NewPhase(cfg, pool)},
		{"DRBD Setup", drbd.NewPhase(cfg, pool)},
		{"Kubernetes Setup", kubernetes.NewPhase(cfg, pool)},
		{"NGINX & WordPress Setup", nginx.NewPhase(cfg, pool)},
		{"Monitoring Setup", monitoring.NewPhase(cfg, pool)},
		{"UFW Setup", ufw.NewPhase(cfg, pool)},
		{"Internal CA Setup", pki.NewPhase(cfg, pool)},
	}

	for _, tc := range testCases {
		t.Run(tc.phaseName, func(t *testing.T) {
			if tc.phase == nil {
				t.Fatalf("expected non-nil Phase for %s", tc.phaseName)
			}
			if tc.phase.Name() != tc.phaseName {
				t.Errorf("expected phase name %q, got %q", tc.phaseName, tc.phase.Name())
			}
			if tc.phase.Description() == "" {
				t.Errorf("expected non-empty description for phase %s", tc.phaseName)
			}
		})
	}
}

func TestPhases_InitVMsOptions(t *testing.T) {
	cfg := &config.Config{}
	pool := &ssh.Pool{}
	lv := libvirt.NewClient("test:///default")

	opts := initvms.Options{
		JumpstartOnly: true,
		NodesOnly:     false,
		Cleanup:       false,
		Recreate:      false,
	}
	p := initvms.NewPhaseWithOpts(cfg, pool, lv, opts)
	if p == nil {
		t.Fatal("expected non-nil phase with options")
	}
}

func TestPhases_TrafficGenerator(t *testing.T) {
	cfg := &config.Config{}
	opts := traffic.Options{
		Mode:        "internal",
		TargetIP:    "192.168.20.100",
		Duration:    1,
		Concurrency: 1,
		WithDB:      false,
		Verbose:     false,
	}

	tr := traffic.New(cfg, opts)
	if tr == nil {
		t.Fatal("expected non-nil traffic generator")
	}
}
