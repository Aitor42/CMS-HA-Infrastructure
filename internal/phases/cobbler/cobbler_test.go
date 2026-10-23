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

func TestInstallCobblerRecreatesObjectsWithoutRebuild(t *testing.T) {
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			if strings.Contains(cmd, `echo "mirror=$fresh objects=$objects"`) {
				return "mirror=1 objects=0\n", "", 0, nil
			}
			return "", "", 0, nil
		},
	}
	p := &Phase{cfg: &config.Config{}, pool: fake}

	if err := p.installCobbler(t.Context(), "192.168.10.10"); err != nil {
		t.Fatalf("installCobbler: %v", err)
	}
	if hasScriptWith(fake.Scripts, "rsync -a ") {
		t.Error("fresh mirror was rebuilt even though only the objects were missing")
	}
	if !hasScriptWith(fake.Scripts, "cobbler profile add") {
		t.Error("missing profile was not recreated")
	}
}

func TestEnsureProfileUsesListForExistenceChecks(t *testing.T) {
	fake := &testutil.FakeExecutor{}
	p := &Phase{pool: fake}

	if err := p.ensureProfile(t.Context(), "192.168.10.10"); err != nil {
		t.Fatalf("ensureProfile: %v", err)
	}
	if len(fake.Scripts) != 1 {
		t.Fatalf("ran %d scripts, want 1", len(fake.Scripts))
	}
	script := fake.Scripts[0].Cmd
	// Cobbler 3.3's `distro report`/`profile report` exit 0 even when the
	// object is missing, so the script must use the list commands instead.
	for _, want := range []string{"cobbler distro list", "cobbler profile list", "grep -Fqx"} {
		if !strings.Contains(script, want) {
			t.Errorf("ensureProfile script does not contain %q", want)
		}
	}
	for _, unwanted := range []string{"cobbler distro report --name", "cobbler profile report --name"} {
		if strings.Contains(script, unwanted) {
			t.Errorf("ensureProfile script uses unreliable existence check %q", unwanted)
		}
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
