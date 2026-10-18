package utils

import (
	"context"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
)

func clientVMTestConfig() *config.Config {
	return &config.Config{
		VM: config.VMConfig{StorageDir: "/srv/cms-ha"},
		Nodes: config.NodesConfig{
			Router: config.NodeDetail{
				Name:        "ufw-router",
				MACInternal: "52:54:00:10:00:01",
				MACMain:     "52:54:00:20:00:01",
				MACWAN:      "52:54:00:30:00:01",
				DiskGB:      5,
			},
			Jumpstart: config.NodeDetail{Name: "jumpstart", DiskGB: 30},
			Masters: []config.NodeDetail{
				{Name: "internal-master1", MAC: "52:54:00:10:00:11", DiskGB: 8, DRBDDiskGB: 4},
				{Name: "internal-master3", MAC: "52:54:00:10:00:13", DiskGB: 8},
			},
			Workers:      []config.NodeDetail{{Name: "internal-worker1", MAC: "52:54:00:10:00:21", DiskGB: 8}},
			Storage:      config.NodeDetail{Name: "internal-storage", MAC: "52:54:00:10:00:31", DiskGB: 8},
			Monitor:      config.NodeDetail{Name: "internal-monitor", MAC: "52:54:00:10:00:41", DiskGB: 10},
			LBs:          []config.NodeDetail{{Name: "main-lb1", MAC: "52:54:00:20:00:51", DiskGB: 8}},
			CMSFrontends: []config.NodeDetail{{Name: "main-cms1", MAC: "52:54:00:20:00:61", DiskGB: 8}},
			Hotdesks:     config.HotdesksConf{Count: 1, DiskGB: 10},
		},
	}
}

func TestClientVMDefinition(t *testing.T) {
	cfg := clientVMTestConfig()

	tests := []struct {
		node       string
		wantNet    string
		wantMAC    string
		wantDRBDGB int
	}{
		{node: "internal-monitor", wantNet: "internal", wantMAC: "52:54:00:10:00:41"},
		{node: "internal-storage", wantNet: "internal", wantMAC: "52:54:00:10:00:31"},
		{node: "internal-master1", wantNet: "internal", wantMAC: "52:54:00:10:00:11", wantDRBDGB: 4},
		{node: "internal-master3", wantNet: "internal", wantMAC: "52:54:00:10:00:13"},
		{node: "internal-worker1", wantNet: "internal", wantMAC: "52:54:00:10:00:21"},
		{node: "main-lb1", wantNet: "main", wantMAC: "52:54:00:20:00:51"},
		{node: "main-cms1", wantNet: "main", wantMAC: "52:54:00:20:00:61"},
		{node: "main-hotdesk1", wantNet: "main", wantMAC: "52:54:00:10:02:c9"},
	}

	for _, tt := range tests {
		t.Run(tt.node, func(t *testing.T) {
			node, nets, extraDisks, err := clientVMDefinition(context.Background(), cfg, nil, tt.node)
			if err != nil {
				t.Fatalf("clientVMDefinition(%q): %v", tt.node, err)
			}
			if node.Name != tt.node {
				t.Errorf("node = %q, want %q", node.Name, tt.node)
			}
			if len(nets) != 1 {
				t.Fatalf("attachments = %v, want exactly one", nets)
			}
			if nets[0].Network != tt.wantNet || nets[0].Mac != tt.wantMAC {
				t.Errorf("attachment = %s on %s, want %s on %s",
					nets[0].Mac, nets[0].Network, tt.wantMAC, tt.wantNet)
			}

			if tt.wantDRBDGB == 0 {
				if len(extraDisks) != 0 {
					t.Errorf("extra disks = %v, want none", extraDisks)
				}
				return
			}
			if len(extraDisks) != 1 || extraDisks[0].SizeGB != tt.wantDRBDGB {
				t.Fatalf("extra disks = %v, want a single %d GB disk", extraDisks, tt.wantDRBDGB)
			}
			if want := "/srv/cms-ha/" + tt.node + "-drbd.qcow2"; extraDisks[0].Path != want {
				t.Errorf("DRBD disk = %q, want %q", extraDisks[0].Path, want)
			}
		})
	}
}

func TestClientVMDefinitionRejectsUnknownNode(t *testing.T) {
	cfg := clientVMTestConfig()
	if _, _, _, err := clientVMDefinition(context.Background(), cfg, nil, "internal-nope"); err == nil {
		t.Fatal("expected an error for a node outside the configured topology")
	}
}
