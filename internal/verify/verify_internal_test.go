package verify

import (
	"context"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

func TestPhase04AcceptsConfiguredNodesAndTheMariaDBPod(t *testing.T) {
	cfg := &config.Config{
		Nodes: config.NodesConfig{
			Masters: []config.NodeDetail{{Name: "m1", IP: "192.168.10.11"}},
			Workers: []config.NodeDetail{{Name: "w1", IP: "192.168.10.13"}},
		},
	}
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			if strings.Contains(cmd, "---CMS_DELIM---") {
				return "m1 Ready control-plane,etcd\nw1 Ready worker\nstale Ready worker\n" +
					"---CMS_DELIM---\nRunning\n---CMS_DELIM---\n" +
					"m1 Ready control-plane,etcd\nstale Ready control-plane,etcd\n", "", 0, nil
			}
			return "", "", 0, nil
		},
	}
	v := &Verifier{cfg: cfg, ssh: fake}

	res := v.phase04(t.Context())
	if !res.Pass {
		t.Fatalf("phase04() = %+v, want a pass despite the stale node object", res)
	}
}

func TestReadyNodeNames(t *testing.T) {
	got := readyNodeNames("m1 Ready control-plane\nm2 NotReady control-plane\nm3 Ready,SchedulingDisabled worker\n")
	if !got["m1"] || got["m2"] || !got["m3"] {
		t.Fatalf("readyNodeNames() = %v", got)
	}
}

func TestEtcdMemberNames(t *testing.T) {
	got := etcdMemberNames("m1 Ready control-plane,etcd\nstale Ready control-plane,etcd\n")
	if !got["m1"] || !got["stale"] || len(got) != 2 {
		t.Fatalf("etcdMemberNames() = %v", got)
	}
}

func TestCountReadyNodes(t *testing.T) {
	tests := []struct {
		name     string
		nodesOut string
		want     int
	}{
		{
			name:     "all ready",
			nodesOut: "NAME   STATUS   ROLES\nm1     Ready    control-plane\nm2     Ready    control-plane\nm3     Ready    worker\n",
			want:     3,
		},
		{
			name:     "notready is never counted",
			nodesOut: "NAME   STATUS     ROLES\nm1     Ready      control-plane\nm2     NotReady   control-plane\nm3     Ready      worker\n",
			want:     2,
		},
		{
			name:     "all notready counts zero",
			nodesOut: "NAME   STATUS     ROLES\nm1     NotReady   control-plane\nm2     NotReady   control-plane\n",
			want:     0,
		},
		{
			name:     "empty output",
			nodesOut: "",
			want:     0,
		},
		{
			name:     "header only",
			nodesOut: "NAME   STATUS   ROLES   AGE   VERSION\n",
			want:     0,
		},
		{
			name:     "cordoned node still counts as ready",
			nodesOut: "NAME   STATUS                     ROLES\nw1     Ready,SchedulingDisabled   worker\n",
			want:     1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countReadyNodes(tt.nodesOut); got != tt.want {
				t.Errorf("countReadyNodes(%q) = %d, want %d", tt.nodesOut, got, tt.want)
			}
		})
	}
}

func TestOutputHasIPv4IsExact(t *testing.T) {
	out := "2: enp1s0    inet 192.168.20.100/24 brd 192.168.20.255 scope global enp1s0\n"
	if !outputHasIPv4(out, "192.168.20.100") {
		t.Error("outputHasIPv4 did not find the exact address")
	}
	if outputHasIPv4(out, "192.168.20.10") {
		t.Error("outputHasIPv4 matched a prefix of a longer address")
	}
	if outputHasIPv4(out, "") {
		t.Error("outputHasIPv4 matched an empty address")
	}
}
