package drbd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

func TestWaitForReplicationFailsWhenThePeerNeverConnects(t *testing.T) {
	old := replicationWait
	replicationWait = retry.Config{MaxAttempts: 2, Interval: time.Millisecond, Timeout: time.Second}
	t.Cleanup(func() { replicationWait = old })

	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, _ string) (string, string, int, error) {
			return "cms_data role:Primary\n  disk:UpToDate\n  peer role:Secondary\n    replication:Connecting\n    peer-disk:DUnknown\n", "", 0, nil
		},
	}
	phase := &Phase{cfg: &config.Config{}, pool: fake}
	node := config.NodeDetail{Name: "master1", IP: "192.168.10.11"}

	err := phase.waitForReplication(t.Context(), node)
	if err == nil {
		t.Fatal("waitForReplication() succeeded with a disconnected replica")
	}
	if !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("waitForReplication() = %v, want the readiness error", err)
	}
}

func TestWaitForReplicationAcceptsAnInProgressSync(t *testing.T) {
	old := replicationWait
	replicationWait = retry.Config{MaxAttempts: 1, Interval: time.Millisecond, Timeout: time.Second}
	t.Cleanup(func() { replicationWait = old })

	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, _ string) (string, string, int, error) {
			return "cms_data role:Primary\n  disk:UpToDate\n  peer role:Secondary\n    replication:Established\n    peer-disk:SyncTarget\n", "", 0, nil
		},
	}
	phase := &Phase{cfg: &config.Config{}, pool: fake}
	node := config.NodeDetail{Name: "master1", IP: "192.168.10.11"}

	if err := phase.waitForReplication(t.Context(), node); err != nil {
		t.Fatalf("waitForReplication() rejected an in-progress sync: %v", err)
	}
}
