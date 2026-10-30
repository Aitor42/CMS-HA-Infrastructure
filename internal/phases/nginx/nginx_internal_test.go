package nginx

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

func testConfig() *config.Config {
	return &config.Config{
		Network: config.NetworkConfig{
			Main: config.NetworkDetail{VIP: "192.168.20.100"},
		},
		Traffic: config.TrafficConfig{VHost: "cms.fake-enterprise.com"},
		Nodes: config.NodesConfig{
			LBs: []config.NodeDetail{
				{Name: "main-lb1", IP: "192.168.20.103"},
				{Name: "main-lb2", IP: "192.168.20.104"},
			},
		},
	}
}

func fastRetry(t *testing.T) {
	t.Helper()
	oldVIP, oldHTTPS := vipOwnershipRetry, httpsProbeRetry
	t.Cleanup(func() {
		vipOwnershipRetry, httpsProbeRetry = oldVIP, oldHTTPS
	})
	vipOwnershipRetry = retry.Config{MaxAttempts: 1, Interval: time.Millisecond}
	httpsProbeRetry = retry.Config{MaxAttempts: 1, Interval: time.Millisecond}
}

func TestVerifyVIPOwnershipRequiresExactlyOneHolder(t *testing.T) {
	fastRetry(t)
	cfg := testConfig()

	cases := map[string]struct {
		holders map[string]bool
		wantErr bool
	}{
		"one holder":   {holders: map[string]bool{"192.168.20.103": true}, wantErr: false},
		"no holder":    {holders: map[string]bool{}, wantErr: true},
		"both holders": {holders: map[string]bool{"192.168.20.103": true, "192.168.20.104": true}, wantErr: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &testutil.FakeExecutor{
				RunFunc: func(_ context.Context, host, _ string) (string, string, int, error) {
					if tc.holders[host] {
						return "2: enp1s0    inet 192.168.20.100/24 brd 192.168.20.255 scope global enp1s0\n", "", 0, nil
					}
					return "2: enp1s0    inet 192.168.20.103/24 brd 192.168.20.255 scope global enp1s0\n", "", 0, nil
				},
			}
			err := NewPhase(cfg, fake).(*Phase).verifyVIPOwnership(t.Context(), []string{"192.168.20.103", "192.168.20.104"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("verifyVIPOwnership() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "exactly one") {
				t.Errorf("error %q does not explain the exactly-one requirement", err)
			}
		})
	}
}

func TestVerifyHTTPSProbesEveryBalancerWithVhost(t *testing.T) {
	fastRetry(t)
	cfg := testConfig()
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _, _ string) (string, string, int, error) {
			return "200", "", 0, nil
		},
	}
	if err := NewPhase(cfg, fake).(*Phase).verifyHTTPS(t.Context(), []string{"192.168.20.103", "192.168.20.104"}); err != nil {
		t.Fatalf("verifyHTTPS: %v", err)
	}

	for _, host := range []string{"192.168.20.103", "192.168.20.104"} {
		found := false
		for _, c := range fake.Commands {
			if c.Host != host || !strings.Contains(c.Cmd, "curl") {
				continue
			}
			found = true
			for _, want := range []string{
				"-k",
				"--resolve 'cms.fake-enterprise.com:443:127.0.0.1'",
				"https://cms.fake-enterprise.com/",
				"exec 2>&1",
			} {
				if !strings.Contains(c.Cmd, want) {
					t.Errorf("%s HTTPS command is missing %q: %s", host, want, c.Cmd)
				}
			}
		}
		if !found {
			t.Errorf("%s was never probed over HTTPS", host)
		}
	}
}

func TestVerifyHTTPSFailsOnNon2xx(t *testing.T) {
	fastRetry(t)
	cfg := testConfig()
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, host, _ string) (string, string, int, error) {
			if host == "192.168.20.104" {
				return "500", "", 22, nil
			}
			return "200", "", 0, nil
		},
	}
	err := NewPhase(cfg, fake).(*Phase).verifyHTTPS(t.Context(), []string{"192.168.20.103", "192.168.20.104"})
	if err == nil || !strings.Contains(err.Error(), "192.168.20.104") {
		t.Fatalf("verifyHTTPS() = %v, want a failure naming the broken balancer", err)
	}
}
