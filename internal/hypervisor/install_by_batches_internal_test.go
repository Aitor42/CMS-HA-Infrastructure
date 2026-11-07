package hypervisor

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

// fakeBatchBackend implements batchBackend in memory, including the state
// transitions the real virsh calls produce.
type fakeBatchBackend struct {
	mu sync.Mutex

	states map[string]string

	created    []string
	started    []string
	shutdown   []string
	forced     []string
	bootOrders [][]string
	maxMemory  map[string]int64
	memory     map[string]int64
	waits      int

	createErr   error
	startErr    error
	shutdownErr error
	forceErr    error
	maxMemErr   error
	memErr      error
	bootErr     error
	waitErr     error
}

func newFakeBatchBackend(states map[string]string) *fakeBatchBackend {
	return &fakeBatchBackend{
		states:    states,
		maxMemory: map[string]int64{},
		memory:    map[string]int64{},
	}
}

func (f *fakeBatchBackend) CreateVM(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, name)
	f.states[name] = "running"
	return nil
}

func (f *fakeBatchBackend) DomainStates(context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	states := make(map[string]string, len(f.states))
	for name, state := range f.states {
		states[name] = state
	}
	return states, nil
}

func (f *fakeBatchBackend) Start(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.started = append(f.started, name)
	f.states[name] = "running"
	return nil
}

func (f *fakeBatchBackend) Shutdown(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.shutdownErr != nil {
		return f.shutdownErr
	}
	f.shutdown = append(f.shutdown, name)
	f.states[name] = "shut off"
	return nil
}

func (f *fakeBatchBackend) ForceStop(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forceErr != nil {
		return f.forceErr
	}
	f.forced = append(f.forced, name)
	f.states[name] = "shut off"
	return nil
}

func (f *fakeBatchBackend) SetMaxMemory(_ context.Context, name string, kib int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.maxMemErr != nil {
		return f.maxMemErr
	}
	f.maxMemory[name] = kib
	return nil
}

func (f *fakeBatchBackend) SetMemory(_ context.Context, name string, kib int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.memErr != nil {
		return f.memErr
	}
	f.memory[name] = kib
	return nil
}

func (f *fakeBatchBackend) FixBootOrder(_ context.Context, names []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bootOrders = append(f.bootOrders, append([]string(nil), names...))
	return f.bootErr
}

func (f *fakeBatchBackend) WaitForSSH(_ context.Context, _ ssh.Executor, _, _ []string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waits++
	return f.waitErr
}

const batchNodeOrder = "router,monitor,storage,worker1,worker2"

func batchBackendConfig() *config.Config {
	return &config.Config{
		Deploy: config.DeployConfig{InstallBatchSize: 5},
		Nodes: config.NodesConfig{
			Router:  config.NodeDetail{Name: "router", IP: "192.168.10.1", RAMFinalMB: 768},
			Monitor: config.NodeDetail{Name: "monitor", IP: "192.168.10.2", RAMFinalMB: 1024},
			Storage: config.NodeDetail{Name: "storage", IP: "192.168.10.3", RAMFinalMB: 2048},
			Workers: []config.NodeDetail{
				{Name: "worker1", IP: "192.168.10.11", RAMInstallMB: 1024, RAMFinalMB: 2048},
				{Name: "worker2", IP: "192.168.10.12", RAMInstallMB: 1024, RAMFinalMB: 4096},
			},
		},
	}
}

func runningBatchState() map[string]string {
	return map[string]string{
		"router": "running", "monitor": "running", "storage": "running",
		"worker1": "running", "worker2": "running",
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func TestInstallByBatchesShutsDownResizesAndBoots(t *testing.T) {
	cfg := batchBackendConfig()
	b := newFakeBatchBackend(runningBatchState())

	if err := installByBatches(context.Background(), cfg, b, &testutil.FakeExecutor{}); err != nil {
		t.Fatalf("installByBatches: %v", err)
	}

	if b.waits != 1 {
		t.Errorf("WaitForSSH calls = %d, want 1", b.waits)
	}
	if len(b.bootOrders) != 2 {
		t.Fatalf("FixBootOrder calls = %d, want 2", len(b.bootOrders))
	}
	for i, got := range b.bootOrders {
		if strings.Join(got, ",") != batchNodeOrder {
			t.Errorf("boot order call %d = %v, want %s", i+1, got, batchNodeOrder)
		}
	}
	wantNodes := sortedCopy(strings.Split(batchNodeOrder, ","))
	if got := sortedCopy(b.shutdown); strings.Join(got, ",") != strings.Join(wantNodes, ",") {
		t.Errorf("shutdown = %v, want every node in the batch", got)
	}
	if got := sortedCopy(b.started); strings.Join(got, ",") != strings.Join(wantNodes, ",") {
		t.Errorf("started = %v, want the final boot of the batch", got)
	}
	if got := b.maxMemory["worker1"]; got != 2048*1024 {
		t.Errorf("worker1 max memory = %d KiB, want %d", got, 2048*1024)
	}
	if got := b.memory["worker2"]; got != 4096*1024 {
		t.Errorf("worker2 memory = %d KiB, want %d", got, 4096*1024)
	}
	if len(b.created) != 0 {
		t.Errorf("created = %v, want no VM creation", b.created)
	}
}

func TestInstallByBatchesCreatesMissingAndStartsStopped(t *testing.T) {
	cfg := batchBackendConfig()
	states := runningBatchState()
	delete(states, "storage")
	states["worker2"] = "shut off"
	b := newFakeBatchBackend(states)

	if err := installByBatches(context.Background(), cfg, b, &testutil.FakeExecutor{}); err != nil {
		t.Fatalf("installByBatches: %v", err)
	}

	if strings.Join(b.created, ",") != "storage" {
		t.Errorf("created = %v, want [storage]", b.created)
	}
	if got := sortedCopy(b.started); strings.Join(got, ",") != "monitor,router,storage,worker1,worker2,worker2" {
		t.Errorf("started = %v, want worker2 at batch start and every node at the end", got)
	}
}

func TestInstallByBatchesStopsOnSSHFailure(t *testing.T) {
	cfg := batchBackendConfig()
	b := newFakeBatchBackend(runningBatchState())
	b.waitErr = errors.New("no login")

	err := installByBatches(context.Background(), cfg, b, &testutil.FakeExecutor{})
	if err == nil || !strings.Contains(err.Error(), "wait for SSH in batch 1: no login") {
		t.Fatalf("installByBatches() = %v, want the SSH failure", err)
	}
	if len(b.shutdown) != 0 || len(b.maxMemory) != 0 || len(b.started) != 0 {
		t.Errorf("phase kept going after the SSH failure: shutdown=%v resize=%v start=%v", b.shutdown, b.maxMemory, b.started)
	}
}

func TestInstallByBatchesReportsResizeFailure(t *testing.T) {
	cfg := batchBackendConfig()
	b := newFakeBatchBackend(runningBatchState())
	b.maxMemErr = errors.New("cannot set memory")

	err := installByBatches(context.Background(), cfg, b, &testutil.FakeExecutor{})
	if err == nil || !strings.Contains(err.Error(), "failed to resize batch 1") {
		t.Fatalf("installByBatches() = %v, want the resize failure", err)
	}
	if len(b.started) != 0 {
		t.Errorf("started = %v, want no final boot after the resize failure", b.started)
	}
}

func TestInstallByBatchesReportsTopologyStartFailure(t *testing.T) {
	cfg := batchBackendConfig()
	b := newFakeBatchBackend(runningBatchState())
	b.startErr = errors.New("cannot boot")

	err := installByBatches(context.Background(), cfg, b, &testutil.FakeExecutor{})
	if err == nil || !strings.Contains(err.Error(), "failed to start the provisioned topology") {
		t.Fatalf("installByBatches() = %v, want the topology start failure", err)
	}
}
