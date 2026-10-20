package utils

import (
	"reflect"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
)

// batchTestConfig mirrors the hosted-CI topology: three management/network
// guests, three masters, two workers and four frontends, twelve guests in all.
func batchTestConfig() *config.Config {
	master := func(n int) config.NodeDetail {
		return config.NodeDetail{Name: "internal-master" + string(rune('0'+n)), IP: "192.168.10.1" + string(rune('0'+n))}
	}
	return &config.Config{
		Nodes: config.NodesConfig{
			Router:  config.NodeDetail{Name: "ufw-router", IP: "192.168.10.1"},
			Monitor: config.NodeDetail{Name: "internal-monitor", IP: "192.168.10.20"},
			Storage: config.NodeDetail{Name: "internal-storage", IP: "192.168.10.15"},
			Masters: []config.NodeDetail{master(1), master(2), master(3)},
			Workers: []config.NodeDetail{{Name: "internal-worker1", IP: "192.168.10.13"}, {Name: "internal-worker2", IP: "192.168.10.14"}},
			LBs:     []config.NodeDetail{{Name: "main-lb1", IP: "192.168.20.11"}, {Name: "main-lb2", IP: "192.168.20.12"}},
			CMSFrontends: []config.NodeDetail{
				{Name: "main-cms1", IP: "192.168.20.21"},
				{Name: "main-cms2", IP: "192.168.20.22"},
			},
		},
	}
}

func batchSizes(batches []installBatch) []int {
	var sizes []int
	for _, b := range batches {
		sizes = append(sizes, len(b.nodes))
	}
	return sizes
}

func batchNodeNames(batches []installBatch) []string {
	var names []string
	for _, b := range batches {
		for _, n := range b.nodes {
			names = append(names, n.name)
		}
	}
	return names
}

// TestPlanInstallBatchesKeepsSemanticGroups is the documented procedure: with
// no width configured the three semantic batches are kept, even when the
// Kubernetes batch is wider than the host's vCPU count.
func TestPlanInstallBatchesKeepsSemanticGroups(t *testing.T) {
	cfg := clientVMTestConfig()

	batches := planInstallBatches(cfg)

	wantNames := []string{
		"1: Network/Management",
		"2: Kubernetes & DB",
		"3: CMS Frontends & Workstations",
	}
	if got := batchSizes(batches); !reflect.DeepEqual(got, []int{3, 3, 3}) {
		t.Fatalf("batch sizes = %v, want [3 3 3]", got)
	}
	for i, want := range wantNames {
		if batches[i].name != want {
			t.Errorf("batch %d name = %q, want %q", i+1, batches[i].name, want)
		}
	}
	wantOrder := []string{
		"ufw-router", "internal-monitor", "internal-storage",
		"internal-master1", "internal-master3", "internal-worker1",
		"main-lb1", "main-cms1", "main-hotdesk1",
	}
	if got := batchNodeNames(batches); !reflect.DeepEqual(got, wantOrder) {
		t.Errorf("node order = %v, want %v", got, wantOrder)
	}
}

// TestPlanInstallBatchesWidth re-splits the same order into balanced rounds.
// The CI topology (twelve guests, four vCPUs) is the case this exists for: the
// five-wide Kubernetes batch becomes 4/4/4, which installs in three rounds
// instead of four.
func TestPlanInstallBatchesWidth(t *testing.T) {
	cfg := batchTestConfig()

	cfg.Deploy.InstallBatchSize = 4
	batches := planInstallBatches(cfg)
	if got := batchSizes(batches); !reflect.DeepEqual(got, []int{4, 4, 4}) {
		t.Fatalf("width 4: batch sizes = %v, want [4 4 4]", got)
	}
	if got, want := batches[0].name, "1/3"; got != want {
		t.Errorf("width 4: first batch name = %q, want %q", got, want)
	}
	// The first master now installs next to the network batch; nothing is
	// dropped, duplicated or reordered by the re-split.
	wantOrder := []string{
		"ufw-router", "internal-monitor", "internal-storage", "internal-master1",
		"internal-master2", "internal-master3", "internal-worker1", "internal-worker2",
		"main-lb1", "main-lb2", "main-cms1", "main-cms2",
	}
	if got := batchNodeNames(batches); !reflect.DeepEqual(got, wantOrder) {
		t.Errorf("width 4: node order = %v, want %v", got, wantOrder)
	}

	cfg.Deploy.InstallBatchSize = 5
	if got := batchSizes(planInstallBatches(cfg)); !reflect.DeepEqual(got, []int{5, 5, 2}) {
		t.Errorf("width 5: batch sizes = %v, want [5 5 2]", got)
	}

	cfg.Deploy.InstallBatchSize = 12
	if got := batchSizes(planInstallBatches(cfg)); !reflect.DeepEqual(got, []int{12}) {
		t.Errorf("width 12: batch sizes = %v, want [12]", got)
	}

	cfg.Deploy.InstallBatchSize = 50
	if got := batchSizes(planInstallBatches(cfg)); !reflect.DeepEqual(got, []int{12}) {
		t.Errorf("width above the guest count: batch sizes = %v, want [12]", got)
	}
}

func TestChunkInstallNodes(t *testing.T) {
	nodes := func(n int) []installNode {
		out := make([]installNode, n)
		for i := range out {
			out[i] = installNode{name: "n" + string(rune('a'+i))}
		}
		return out
	}

	tests := []struct {
		name  string
		nodes []installNode
		width int
		want  []int
	}{
		{name: "exact multiple", nodes: nodes(12), width: 4, want: []int{4, 4, 4}},
		{name: "remainder", nodes: nodes(15), width: 4, want: []int{4, 4, 4, 3}},
		{name: "single round", nodes: nodes(3), width: 4, want: []int{3}},
		{name: "one at a time", nodes: nodes(3), width: 1, want: []int{1, 1, 1}},
		{name: "empty", nodes: nil, width: 4, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := chunkInstallNodes(tt.nodes, tt.width)
			if !reflect.DeepEqual(batchSizes(got), tt.want) {
				t.Fatalf("chunk sizes = %v, want %v", batchSizes(got), tt.want)
			}
			if len(tt.nodes) == 0 {
				return
			}
			// Chunking must not reorder, drop or duplicate a guest.
			flat := make([]installNode, 0, len(tt.nodes))
			for _, b := range got {
				flat = append(flat, b.nodes...)
			}
			if !reflect.DeepEqual(flat, tt.nodes) {
				t.Errorf("chunked nodes = %v, want the original order", flat)
			}
		})
	}
}
