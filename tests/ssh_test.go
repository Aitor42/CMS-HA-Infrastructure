package tests

import (
	"context"
	"testing"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

func TestSSH_NewPoolLazyInit(t *testing.T) {
	// Creating a pool with a non-existent key should succeed without crashing (lazy key loading)
	pool, err := ssh.NewPool("/tmp/non_existent_key_12345", 2*time.Second)
	if err != nil {
		t.Fatalf("expected NewPool to succeed in lazy mode, got: %v", err)
	}
	defer pool.Close()

	// Calling a command with a non-existent key should fail gracefully
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, _, _, err = pool.RunCommand(ctx, "127.0.0.1", "echo test")
	if err == nil {
		t.Errorf("expected error connecting without valid key, got nil")
	}
}

func TestSSH_RunParallelAggregation(t *testing.T) {
	pool, err := ssh.NewPool("/tmp/non_existent_key_12345", 1*time.Second)
	if err != nil {
		t.Fatalf("unexpected pool error: %v", err)
	}
	defer pool.Close()

	// Fresh context per call: sharing one 1s timeout across sequential
	// RunParallel invocations made the test flaky on slow machines.
	runParallel := func(hosts []string, want int, label string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		results := pool.RunParallel(ctx, hosts, "echo test")
		if len(results) != want {
			t.Fatalf("%s: expected %d results, got %d", label, want, len(results))
		}
	}

	runParallel([]string{"192.0.2.1", "192.0.2.2"}, 2, "basic")

	// Duplicate hosts - should return exact number of entries without collapsing
	runParallel([]string{"192.0.2.1", "192.0.2.1", "192.0.2.2"}, 3, "duplicate hosts")

	// Empty hosts
	runParallel([]string{}, 0, "empty hosts")
}

func TestSSH_RunCommandStdinRejectsNilWriter(t *testing.T) {
	// A nil stdout writer must fail fast, before any connection attempt.
	pool, err := ssh.NewPool("/tmp/non_existent_key_12345", time.Second)
	if err != nil {
		t.Fatalf("unexpected pool error: %v", err)
	}
	defer pool.Close()

	if _, _, err := pool.RunCommandStdin(context.Background(), "192.0.2.1", "true", nil, nil); err == nil {
		t.Fatal("expected an error for a nil stdout writer")
	}
}
