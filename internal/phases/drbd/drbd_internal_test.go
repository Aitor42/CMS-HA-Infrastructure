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

func TestWaitForReplicationRejectsAStaleLocalDiskWithAHealthyPeer(t *testing.T) {
	old := replicationWait
	replicationWait = retry.Config{MaxAttempts: 1, Interval: time.Millisecond, Timeout: time.Second}
	t.Cleanup(func() { replicationWait = old })

	// The peer is healthy, but the local disk is not. A substring match on
	// "disk:UpToDate" would also hit "peer-disk:UpToDate" and let this pass.
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, _ string) (string, string, int, error) {
			return "cms_data role:Primary\n  disk:Inconsistent\n  peer role:Secondary\n    replication:Established\n    peer-disk:UpToDate\n", "", 0, nil
		},
	}
	phase := &Phase{cfg: &config.Config{}, pool: fake}
	node := config.NodeDetail{Name: "master1", IP: "192.168.10.11"}

	err := phase.waitForReplication(t.Context(), node)
	if err == nil {
		t.Fatal("waitForReplication() accepted a Primary with a stale local disk")
	}
	if !strings.Contains(err.Error(), `local disk "Inconsistent"`) {
		t.Fatalf("waitForReplication() = %v, want the local-disk reason", err)
	}
}

func TestWaitForReplicationFailsClosedWithoutALocalDiskLine(t *testing.T) {
	old := replicationWait
	replicationWait = retry.Config{MaxAttempts: 1, Interval: time.Millisecond, Timeout: time.Second}
	t.Cleanup(func() { replicationWait = old })

	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, _ string) (string, string, int, error) {
			// A peer role line must not stand in for the local role either.
			return "cms_data role:Secondary\n  peer role:Primary\n    replication:Established\n    peer-disk:UpToDate\n", "", 0, nil
		},
	}
	phase := &Phase{cfg: &config.Config{}, pool: fake}
	node := config.NodeDetail{Name: "master1", IP: "192.168.10.11"}

	if err := phase.waitForReplication(t.Context(), node); err == nil {
		t.Fatal("waitForReplication() accepted a node that is not the local Primary")
	}
}

func TestDRBDStatusParsingUsesTheLocalLines(t *testing.T) {
	out := "cms_data role:Secondary\n  disk:Inconsistent\n  peer role:Primary\n    replication:Established\n    peer-disk:UpToDate\n"
	if got := drbdLocalRole(out); got != "Secondary" {
		t.Fatalf("drbdLocalRole() = %q, want Secondary (peer role must not match)", got)
	}
	if got := drbdLocalDisk(out); got != "Inconsistent" {
		t.Fatalf("drbdLocalDisk() = %q, want Inconsistent (peer-disk must not match)", got)
	}
	if got := drbdLocalRole("no resource here"); got != "" {
		t.Fatalf("drbdLocalRole() = %q, want empty for unparseable output", got)
	}
	if got := drbdLocalDisk("no resource here"); got != "" {
		t.Fatalf("drbdLocalDisk() = %q, want empty for unparseable output", got)
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
