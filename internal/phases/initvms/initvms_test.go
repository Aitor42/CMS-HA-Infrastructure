package initvms

import (
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

func TestNewPhase(t *testing.T) {
	cfg := &config.Config{}
	pool := &ssh.Pool{}
	lv := libvirt.NewClient("test:///default")

	p := NewPhase(cfg, pool, lv)
	if p == nil {
		t.Fatal("expected non-nil Phase")
	}
	if p.Name() != "00-init-vms" {
		t.Errorf("unexpected name: %s", p.Name())
	}
	if p.Description() == "" {
		t.Error("expected non-empty description")
	}

	opts := Options{
		JumpstartOnly: true,
		NodesOnly:     false,
		Cleanup:       false,
		Recreate:      false,
	}
	pOpts := NewPhaseWithOpts(cfg, pool, lv, opts)
	if pOpts == nil {
		t.Fatal("expected non-nil PhaseWithOpts")
	}
}

func TestGenerateRandomPasswordHash(t *testing.T) {
	hash := generateRandomPasswordHash()
	if hash == "" {
		t.Fatal("expected non-empty password hash")
	}
}
