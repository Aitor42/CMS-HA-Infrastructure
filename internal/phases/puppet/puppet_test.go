package puppet

import (
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

func TestNewPhase(t *testing.T) {
	cfg := &config.Config{
		Nodes: config.NodesConfig{
			Jumpstart: config.NodeDetail{IP: "192.168.10.10"},
			Masters: []config.NodeDetail{
				{IP: "192.168.10.11"},
				{IP: "192.168.10.12"},
			},
		},
	}
	pool := &ssh.Pool{}

	p := NewPhase(cfg, pool)
	if p == nil {
		t.Fatal("expected non-nil Phase")
	}
	if p.Name() != "04-setup-puppet" {
		t.Errorf("unexpected name: %s", p.Name())
	}
	if p.Description() == "" {
		t.Error("expected non-empty description")
	}

	phaseImpl, ok := p.(*Phase)
	if !ok {
		t.Fatal("expected *Phase type")
	}
	agentIPs := phaseImpl.getAgentIPs()
	if len(agentIPs) != 2 {
		t.Errorf("expected 2 agent IPs, got %d", len(agentIPs))
	}
}
