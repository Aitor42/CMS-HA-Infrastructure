package cobbler

import (
	"context"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

func hasScriptWith(scripts []testutil.Command, substr string) bool {
	for _, s := range scripts {
		if strings.Contains(s.Cmd, substr) {
			return true
		}
	}
	return false
}

func TestInstallCobblerSkipsRebuildWhenCurrent(t *testing.T) {
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			if strings.Contains(cmd, `echo "mirror=$fresh objects=$objects"`) {
				return "mirror=1 objects=1\n", "", 0, nil
			}
			return "", "", 0, nil
		},
	}
	p := &Phase{cfg: &config.Config{}, pool: fake}

	if err := p.installCobbler(t.Context(), "192.168.10.10"); err != nil {
		t.Fatalf("installCobbler: %v", err)
	}
	if len(fake.Scripts) < 10 {
		t.Errorf("ran %d scripts, want the full install sequence", len(fake.Scripts))
	}
	if hasScriptWith(fake.Scripts, "rsync -a ") {
		t.Error("current mirror was rebuilt")
	}
	// Steady state returns before touching the profile or syncing: the mirror
	// and both objects are current, so there is nothing to rebuild.
	if hasScriptWith(fake.Scripts, "cobbler profile edit") {
		t.Error("steady state should not rewrite the profile")
	}
	if hasScriptWith(fake.Scripts, "cobbler sync") {
		t.Error("steady state should not synchronize Cobbler")
	}
}

func TestInstallCobblerRebuildsStaleMirror(t *testing.T) {
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			if strings.Contains(cmd, `echo "mirror=$fresh objects=$objects"`) {
				return "mirror=0 objects=0\n", "", 0, nil
			}
			return "", "", 0, nil
		},
	}
	p := &Phase{cfg: &config.Config{}, pool: fake}

	if err := p.installCobbler(t.Context(), "192.168.10.10"); err != nil {
		t.Fatalf("installCobbler: %v", err)
	}
	if !hasScriptWith(fake.Scripts, "rsync -a ") {
		t.Error("stale mirror was not rebuilt")
	}
}

func TestInstallCobblerStopsAtFailingStep(t *testing.T) {
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			if strings.Contains(cmd, "apt-get install -y cobbler") {
				return "", "package not found", 1, nil
			}
			return "", "", 0, nil
		},
	}
	p := &Phase{cfg: &config.Config{}, pool: fake}

	err := p.installCobbler(t.Context(), "192.168.10.10")
	if err == nil || !strings.Contains(err.Error(), "install step") {
		t.Fatalf("installCobbler() = %v, want a failing install step", err)
	}
	if hasScriptWith(fake.Scripts, "cobbler profile edit") {
		t.Error("phase continued after a failed install step")
	}
}
