package deploy

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStateLockSerializesDeployRuns(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "deploy.state")

	first, err := acquireStateLock(statePath)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	second, err := acquireStateLock(statePath)
	if err == nil {
		_ = second.Close()
		t.Fatal("second acquire succeeded while the first lock was held")
	}
	if !strings.Contains(err.Error(), "deployment already running") {
		t.Fatalf("unexpected contention error: %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("release first lock: %v", err)
	}
	third, err := acquireStateLock(statePath)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("release third lock: %v", err)
	}
}

func TestStateLockIndependentPaths(t *testing.T) {
	dir := t.TempDir()
	a, err := acquireStateLock(filepath.Join(dir, "a.state"))
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	defer func() { _ = a.Close() }()

	b, err := acquireStateLock(filepath.Join(dir, "b.state"))
	if err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("release b: %v", err)
	}
}

func TestStateLockCloseNil(t *testing.T) {
	var lock *stateLock
	if err := lock.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}
