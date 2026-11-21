package initvms

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

// errStalled stands for the stalled-first-boot error the recovery reboot
// targets: the guest applied its network configuration but sshd never started
// listening, which is what the 2026-11-21 daily run failed on.
var errStalled = errors.New("timed out waiting for ssh")

// bootWaitExecutor fails WaitForSSH until succeedAfter calls are made; a zero
// or negative succeedAfter never succeeds. Embedding the FakeExecutor keeps
// the rest of the Executor surface inert.
type bootWaitExecutor struct {
	testutil.FakeExecutor
	succeedAfter int
	waits        int
}

func (e *bootWaitExecutor) WaitForSSH(_ context.Context, _ string, _ time.Duration) error {
	e.waits++
	if e.succeedAfter > 0 && e.waits >= e.succeedAfter {
		return nil
	}
	return errStalled
}

func bootPhase(pool *bootWaitExecutor) *Phase {
	return &Phase{
		cfg: &config.Config{
			Nodes: config.NodesConfig{
				Jumpstart: config.NodeDetail{Name: "jumpstart", IP: "192.168.10.10"},
			},
		},
		pool: pool,
	}
}

func TestWaitBootSSHFirstWaitSucceeds(t *testing.T) {
	pool := &bootWaitExecutor{succeedAfter: 1}
	// lv stays nil on purpose: the success path must never touch libvirt, so
	// an accidental recovery reboot would dereference it and fail the test.
	p := bootPhase(pool)
	p.waitBootSSH(context.Background())
	if pool.waits != 1 {
		t.Fatalf("WaitForSSH called %d times, want 1 (no recovery reboot)", pool.waits)
	}
}

func TestWaitBootSSHRecoversWithReboot(t *testing.T) {
	pool := &bootWaitExecutor{succeedAfter: 2}
	// Destroy/Start go through the real Client wrapper; without a matching
	// domain (or without virsh on the PATH at all) they fail fast and the
	// recovery proceeds, which is the behaviour under test.
	p := bootPhase(pool)
	p.lv = &libvirt.Client{URI: "qemu:///session"}
	p.waitBootSSH(context.Background())
	if pool.waits != 2 {
		t.Fatalf("WaitForSSH called %d times, want 2 (first wait + retry after recovery reboot)", pool.waits)
	}
}

func TestWaitBootSSHGivesUpAfterReboot(t *testing.T) {
	pool := &bootWaitExecutor{}
	p := bootPhase(pool)
	p.lv = &libvirt.Client{URI: "qemu:///session"}
	p.waitBootSSH(context.Background())
	if pool.waits != 2 {
		t.Fatalf("WaitForSSH called %d times, want 2 (one wait + one retry after reboot)", pool.waits)
	}
}
