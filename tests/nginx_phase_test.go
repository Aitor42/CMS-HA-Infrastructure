package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/nginx"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

func nginxConfig() *config.Config {
	return &config.Config{
		Network: config.NetworkConfig{
			Main: config.NetworkDetail{VIP: "192.168.20.100"},
		},
		Traffic: config.TrafficConfig{VHost: "cms.fake-enterprise.com"},
		Nodes: config.NodesConfig{
			Storage: config.NodeDetail{Name: "internal-storage", IP: "192.168.10.15"},
			LBs: []config.NodeDetail{
				{Name: "main-lb1", IP: "192.168.20.103"},
				{Name: "main-lb2", IP: "192.168.20.104"},
			},
			CMSFrontends: []config.NodeDetail{
				{Name: "main-cms1", IP: "192.168.20.101"},
				{Name: "main-cms2", IP: "192.168.20.102"},
			},
		},
	}
}

func TestNginxPhaseChecksTheRealExportMountVIPAndHTTPS(t *testing.T) {
	cfg := nginxConfig()
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, host, cmd string) (string, string, int, error) {
			switch {
			case strings.Contains(cmd, "exportfs -s"):
				return "/srv/nfs/wp-uploads 192.168.20.0/24(sync,wdelay,hide,no_subtree_check,sec=sys,rw,secure,root_squash,all_squash,anonuid=33,anongid=33)\n", "", 0, nil
			case strings.Contains(cmd, "findmnt"):
				return "192.168.10.15:/srv/nfs/wp-uploads nfs4\n", "", 0, nil
			case strings.Contains(cmd, "ip -4 -o addr show"):
				if host == "192.168.20.103" {
					return "2: enp1s0    inet 192.168.20.100/24 brd 192.168.20.255 scope global enp1s0\n", "", 0, nil
				}
				return "2: enp1s0    inet 192.168.20.104/24 brd 192.168.20.255 scope global enp1s0\n", "", 0, nil
			case strings.Contains(cmd, "curl"):
				return "200", "", 0, nil
			default:
				return "", "", 0, nil
			}
		},
	}

	if err := nginx.NewPhase(cfg, fake).Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Diagnostics: RunParallel only keeps stdout, so the command has to merge
	// stderr or `nginx -t` failures lose their reason.
	for _, host := range []string{"192.168.20.103", "192.168.20.104"} {
		if !hasCommand(fake.Commands, host, "exec 2>&1; nginx -t") {
			t.Errorf("%s never ran nginx -t with stderr diagnostics", host)
		}
		if !hasCommand(fake.Commands, host, "curl -s -k -f") ||
			!hasCommand(fake.Commands, host, "--resolve 'cms.fake-enterprise.com:443:127.0.0.1'") {
			t.Errorf("%s was not probed over HTTPS with the configured vhost", host)
		}
		if !hasCommand(fake.Commands, host, "ip -4 -o addr show") {
			t.Errorf("%s never checked VIP ownership", host)
		}
	}

	if !hasCommand(fake.Commands, "192.168.10.15", "anonuid=33") ||
		!hasCommand(fake.Commands, "192.168.10.15", "anongid=33") ||
		!hasCommand(fake.Commands, "192.168.10.15", "all_squash") {
		t.Error("the storage check does not assert all_squash,anonuid=33,anongid=33")
	}

	for _, host := range []string{"192.168.20.101", "192.168.20.102"} {
		if !hasCommand(fake.Commands, host, "findmnt") ||
			!hasCommand(fake.Commands, host, "192.168.10.15:/srv/nfs/wp-uploads") {
			t.Errorf("%s mount check does not assert the NFS source", host)
		}
	}
}

func TestNginxPhaseFailsWhenTheExportLacksAnonMapping(t *testing.T) {
	cfg := nginxConfig()
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			if strings.Contains(cmd, "exportfs -s") {
				// The path exists but the identity mapping is missing: the old
				// path-only grep accepted this export.
				return "/srv/nfs/wp-uploads 192.168.20.0/24(rw,sync)\n", "", 1, nil
			}
			return "", "", 0, nil
		},
	}

	err := nginx.NewPhase(cfg, fake).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "anonuid=33") {
		t.Fatalf("Run() = %v, want the export identity failure", err)
	}
}
