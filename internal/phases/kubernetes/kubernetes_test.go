package kubernetes

import (
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

func TestNewPhase(t *testing.T) {
	cfg := &config.Config{
		Nodes: config.NodesConfig{
			Masters: []config.NodeDetail{
				{Name: "internal-master1", IP: "192.168.10.11"},
				{Name: "internal-master2", IP: "192.168.10.12"},
			},
		},
	}
	pool := &ssh.Pool{}

	p := NewPhase(cfg, pool)
	if p == nil {
		t.Fatal("expected non-nil Phase")
	}
	if p.Name() != "Kubernetes Setup" {
		t.Errorf("unexpected name: %s", p.Name())
	}
	if p.Description() == "" {
		t.Error("expected non-empty description")
	}
}
