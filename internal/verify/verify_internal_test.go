package verify

import (
	"context"
	"errors"
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

func phase03Config() *config.Config {
	return &config.Config{
		PKI: config.PKIConfig{
			CAName:           "CMS Local CA",
			CertDNSNames:     []string{"cms.internal.local"},
			CertMinValidDays: 30,
		},
		Network: config.NetworkConfig{
			Main: config.NetworkDetail{VIP: "192.168.20.100"},
		},
		Nodes: config.NodesConfig{
			LBs:          []config.NodeDetail{{Name: "lb1", IP: "192.168.20.103"}},
			CMSFrontends: []config.NodeDetail{{Name: "cms1", IP: "192.168.20.101"}},
		},
	}
}

func TestPhase03VerifiesTheChainAndEverySAN(t *testing.T) {
	cfg := phase03Config()
	var lbCmd string
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, host, cmd string) (string, string, int, error) {
			if strings.Contains(cmd, "openssl x509") {
				lbCmd = cmd
				return "issuer=CN = CMS Local CA\n", "", 0, nil
			}
			if strings.Contains(cmd, "ip -4 -o addr show") {
				if host == "192.168.20.103" {
					return "2: enp1s0    inet 192.168.20.100/24 brd 192.168.20.255 scope global enp1s0\n", "", 0, nil
				}
				return "", "", 0, nil
			}
			return "", "", 0, nil
		},
	}
	v := &Verifier{cfg: cfg, ssh: fake}

	res := v.phase03(t.Context())
	if !res.Pass {
		t.Fatalf("phase03() = %+v, want a pass", res)
	}
	if lbCmd == "" {
		t.Fatal("phase03 did not run the certificate check")
	}
	for _, want := range []string{
		"openssl verify -CAfile '/usr/local/share/ca-certificates/cms_root_ca.crt' -untrusted /etc/nginx/ssl/cms.crt -verify_hostname 'cms.internal.local'",
		"openssl verify -CAfile '/usr/local/share/ca-certificates/cms_root_ca.crt' -untrusted /etc/nginx/ssl/cms.crt -verify_ip '192.168.20.100'",
		"openssl verify -CAfile '/usr/local/share/ca-certificates/cms_root_ca.crt' -untrusted /etc/nginx/ssl/cms.crt -verify_ip '192.168.20.103'",
	} {
		if !strings.Contains(lbCmd, want) {
			t.Errorf("certificate check is missing %q:\n%s", want, lbCmd)
		}
	}
}

func TestPhase03FailsWhenTheChainVerificationFails(t *testing.T) {
	cfg := phase03Config()
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, host, cmd string) (string, string, int, error) {
			if strings.Contains(cmd, "openssl x509") {
				return "error 20 at 0 depth lookup: unable to get local issuer certificate\n", "", 1, nil
			}
			if strings.Contains(cmd, "ip -4 -o addr show") && host == "192.168.20.103" {
				return "2: enp1s0    inet 192.168.20.100/24 brd 192.168.20.255 scope global enp1s0\n", "", 0, nil
			}
			return "", "", 0, nil
		},
	}
	v := &Verifier{cfg: cfg, ssh: fake}

	res := v.phase03(t.Context())
	if res.Pass {
		t.Fatalf("phase03() = %+v, want a failure when the chain does not verify", res)
	}
	if !strings.Contains(res.Details, "certificate check failed") {
		t.Errorf("phase03() details = %q, want the certificate failure", res.Details)
	}
}

func TestPhase05ChecksHTTPReadinessAndLoadedRules(t *testing.T) {
	cfg := &config.Config{Nodes: config.NodesConfig{
		Monitor: config.NodeDetail{Name: "internal-monitor", IP: "192.168.10.20"},
	}}
	var gotCmd string
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			gotCmd = cmd
			return "active\nactive\nactive\n", "", 0, nil
		},
	}
	v := &Verifier{cfg: cfg, ssh: fake}

	res := v.phase05(t.Context())
	if !res.Pass {
		t.Fatalf("phase05() = %+v, want a pass", res)
	}
	for _, want := range []string{
		"prometheus-alertmanager",
		"http://127.0.0.1:3000/api/health",
		"http://127.0.0.1:9090/-/ready",
		"http://127.0.0.1:9093/-/ready",
		`"infrastructure"`,
	} {
		if !strings.Contains(gotCmd, want) {
			t.Errorf("phase05 command is missing %q:\n%s", want, gotCmd)
		}
	}
	if !strings.Contains(res.Details, "alert rules loaded") {
		t.Errorf("phase05() details = %q, want the loaded-rules detail", res.Details)
	}
}

func TestPhase05FailsWhenAListenerIsNotReady(t *testing.T) {
	cfg := &config.Config{Nodes: config.NodesConfig{
		Monitor: config.NodeDetail{Name: "internal-monitor", IP: "192.168.10.20"},
	}}
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, _ string) (string, string, int, error) {
			return "active\nactive\nactive\n", "", 7, errors.New("exit status 7")
		},
	}
	v := &Verifier{cfg: cfg, ssh: fake}

	if res := v.phase05(t.Context()); res.Pass {
		t.Fatalf("phase05() = %+v, want a failure when a listener is not ready", res)
	}
}

func TestPhase05FailsWhenAUnitIsInactive(t *testing.T) {
	cfg := &config.Config{Nodes: config.NodesConfig{
		Monitor: config.NodeDetail{Name: "internal-monitor", IP: "192.168.10.20"},
	}}
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, _ string) (string, string, int, error) {
			return "active\ninactive\nactive\n", "", 3, nil
		},
	}
	v := &Verifier{cfg: cfg, ssh: fake}

	if res := v.phase05(t.Context()); res.Pass {
		t.Fatalf("phase05() = %+v, want a failure when a unit is inactive", res)
	}
}
