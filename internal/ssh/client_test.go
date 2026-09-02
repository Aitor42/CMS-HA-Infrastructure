package ssh

import (
	"context"
	"testing"
	"time"
)

func TestNewPoolLazyInit(t *testing.T) {
	// Creating a pool with a non-existent key should succeed without crashing (lazy key loading)
	pool, err := NewPool("/tmp/non_existent_key_12345", 2*time.Second)
	if err != nil {
		t.Fatalf("expected NewPool to succeed in lazy mode, got: %v", err)
	}
	defer pool.Close()

	// Calling a command with a non-existent key should fail with clear key error
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, _, _, err = pool.RunCommand(ctx, "127.0.0.1", "echo test")
	if err == nil {
		t.Errorf("expected error connecting without valid key, got nil")
	}
}

func TestRunParallelAggregation(t *testing.T) {
	pool, err := NewPool("/tmp/non_existent_key_12345", 1*time.Second)
	if err != nil {
		t.Fatalf("unexpected pool error: %v", err)
	}
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	hosts := []string{"192.0.2.1", "192.0.2.2"}
	results := pool.RunParallel(ctx, hosts, "echo test")

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
}
