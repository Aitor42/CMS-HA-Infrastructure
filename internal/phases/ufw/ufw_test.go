package ufw

import (
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

func TestNewPhase(t *testing.T) {
	cfg := &config.Config{}
	pool := &ssh.Pool{}

	p := NewPhase(cfg, pool)
	if p == nil {
		t.Fatal("expected non-nil Phase")
	}
	if p.Name() != "UFW Setup" {
		t.Errorf("unexpected name: %s", p.Name())
	}
	if p.Description() == "" {
		t.Error("expected non-empty description")
	}
}
