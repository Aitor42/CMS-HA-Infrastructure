package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/kubernetes"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

func kubernetesConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Database: config.DatabaseConfig{
			Password:         testutil.FakeSecret("wp"),
			RootPassword:     testutil.FakeSecret("root"),
			ExporterPassword: testutil.FakeSecret("exporter"),
		},
		WordPress: config.WordPressConfig{AdminPassword: testutil.FakeSecret("admin")},
		Network: config.NetworkConfig{
			Internal: config.NetworkDetail{CIDR: "192.168.10.0/24"},
		},
		Nodes: config.NodesConfig{
			Masters: []config.NodeDetail{
				{Name: "master1", IP: "192.168.10.11", FQDN: "master1.internal.local"},
				{Name: "master2", IP: "192.168.10.12", FQDN: "master2.internal.local"},
				{Name: "master3", IP: "192.168.10.13", FQDN: "master3.internal.local"},
			},
			Workers: []config.NodeDetail{
				{Name: "worker1", IP: "192.168.10.21", FQDN: "worker1.internal.local"},
			},
		},
	}
}

func TestKubernetesPhaseRun(t *testing.T) {
	cfg := kubernetesConfig(t)
	master1 := cfg.Nodes.Masters[0].IP
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			switch {
			case strings.Contains(cmd, "cat /var/lib/rancher/k3s/server/node-token"):
				return "K10test::server:token\n", "", 0, nil
			case strings.Contains(cmd, "kubectl get nodes --no-headers"):
				return "master1 Ready control-plane,etcd,master 1m v1.29.9+k3s1\nmaster2 Ready control-plane,etcd,master 1m v1.29.9+k3s1\nmaster3 Ready control-plane,etcd,master 1m v1.29.9+k3s1\nworker1 Ready <none> 1m v1.29.9+k3s1\n", "", 0, nil
			case strings.Contains(cmd, "kubectl get nodes -l node-role.kubernetes.io/etcd"):
				return "master1 Ready control-plane,etcd\nmaster2 Ready control-plane,etcd\nmaster3 Ready control-plane,etcd\n", "", 0, nil
			default:
				return "", "", 0, nil
			}
		},
	}

	if err := kubernetes.NewPhase(cfg, fake).Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !hasCommand(fake.Commands, master1, `INSTALL_K3S_EXEC="server --cluster-init`) {
		t.Error("master1 was not initialized with the cluster-init server")
	}
	if !hasCommand(fake.Commands, master1, "INSTALL_K3S_SKIP_START=true") {
		t.Error("master1 install still blocks on systemctl restart")
	}
	if !hasCommand(fake.Commands, master1, "systemctl start k3s --no-block") {
		t.Error("master1 was not started with --no-block")
	}
	for _, ip := range []string{"192.168.10.12", "192.168.10.13", "192.168.10.21"} {
		if got := string(fake.Files[ip]["/root/.k3s-join-token"]); got != "K10test::server:token" {
			t.Errorf("join token on %s = %q", ip, got)
		}
	}
	for _, ip := range []string{"192.168.10.12", "192.168.10.13"} {
		if !hasCommand(fake.Commands, ip, "--server https://192.168.10.11:6443 --token-file") {
			t.Errorf("%s did not join as a server", ip)
		}
	}
	if !hasCommand(fake.Commands, "192.168.10.21", "K3S_URL=https://192.168.10.11:6443") {
		t.Error("worker did not join as an agent")
	}
	if !hasCommand(fake.Commands, "192.168.10.21", "K3S_TOKEN_FILE=/root/.k3s-join-token") {
		t.Error("worker did not read the token from the protected file")
	}
	if hasCommand(fake.Commands, "192.168.10.21", `K3S_TOKEN="$(cat`) {
		t.Error("worker expanded the token into the environment")
	}
	if !hasCommand(fake.Commands, master1, "drbd-status=primary") {
		t.Error("master1 was not labeled as the DRBD primary")
	}
	if len(fake.CopiedTars) != 1 {
		t.Errorf("copied %d kustomize tarballs, want 1", len(fake.CopiedTars))
	}
	if len(fake.Files[master1]["/tmp/k3s-manifests/generated/mariadb-secret.yaml"]) == 0 {
		t.Error("MariaDB Secret manifest was not uploaded")
	}
	for _, want := range []string{
		"base/namespace.yaml",
		"generated/mariadb-secret.yaml",
		"overlays/lab",
		"monitoring",
		"jobs",
		"kubectl delete job init-wordpress-db",
		"kubectl wait --for=condition=complete job/init-wordpress-db",
		"rm -rf /tmp/k3s-manifests",
	} {
		if !hasCommand(fake.Commands, master1, want) {
			t.Errorf("master1 never ran %q", want)
		}
	}
	if hasCommand(fake.Commands, master1, "apply -k /tmp/k3s-manifests/backups") {
		t.Error("backup root applied with backups disabled")
	}
}

func TestKubernetesPhaseIgnoresStaleEtcdNode(t *testing.T) {
	cfg := kubernetesConfig(t)
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			switch {
			case strings.Contains(cmd, "cat /var/lib/rancher/k3s/server/node-token"):
				return "K10test::server:token\n", "", 0, nil
			case strings.Contains(cmd, "kubectl get nodes --no-headers"):
				return "master1 Ready control-plane,etcd,master 1m v1.29.9+k3s1\nmaster2 Ready control-plane,etcd,master 1m v1.29.9+k3s1\nmaster3 Ready control-plane,etcd,master 1m v1.29.9+k3s1\nworker1 Ready <none> 1m v1.29.9+k3s1\n", "", 0, nil
			case strings.Contains(cmd, "kubectl get nodes -l node-role.kubernetes.io/etcd"):
				// A stale object from an earlier failed join must not hide a
				// missing configured member: the phase checks expected names,
				// not just the row count.
				return "master1 Ready control-plane,etcd\nmaster2 Ready control-plane,etcd\nmaster3 Ready control-plane,etcd\nstale-node Ready control-plane,etcd\n", "", 0, nil
			default:
				return "", "", 0, nil
			}
		},
	}

	if err := kubernetes.NewPhase(cfg, fake).Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestKubernetesPhaseRequiresThreeMasters(t *testing.T) {
	cfg := kubernetesConfig(t)
	cfg.Nodes.Masters = cfg.Nodes.Masters[:2]
	fake := &testutil.FakeExecutor{}

	err := kubernetes.NewPhase(cfg, fake).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "at least 3 master nodes") {
		t.Fatalf("Run() = %v, want the master-count error", err)
	}
	if len(fake.Commands) != 0 {
		t.Errorf("phase ran %d commands, want 0", len(fake.Commands))
	}
}

func TestKubernetesPhaseRequiresSecrets(t *testing.T) {
	cfg := kubernetesConfig(t)
	cfg.Database.RootPassword = ""
	fake := &testutil.FakeExecutor{}

	err := kubernetes.NewPhase(cfg, fake).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "must be set in config.yaml") {
		t.Fatalf("Run() = %v, want the missing-secret error", err)
	}
	if len(fake.Commands) != 0 {
		t.Errorf("phase ran %d commands, want 0", len(fake.Commands))
	}
}

func TestKubernetesPhaseLabelsActualDRBDPrimary(t *testing.T) {
	cfg := kubernetesConfig(t)
	cfg.Nodes.Masters[0].DRBDDiskGB = 3
	cfg.Nodes.Masters[1].DRBDDiskGB = 3
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, host, cmd string) (string, string, int, error) {
			switch {
			case strings.Contains(cmd, "drbdadm role cms_data"):
				if host == "192.168.10.12" {
					return "Primary/Secondary\n", "", 0, nil
				}
				return "Secondary/Primary\n", "", 0, nil
			case strings.Contains(cmd, "cat /var/lib/rancher/k3s/server/node-token"):
				return "K10test::server:token\n", "", 0, nil
			case strings.Contains(cmd, "kubectl get nodes --no-headers"):
				return "master1 Ready control-plane,etcd,master 1m v1.29.9+k3s1\nmaster2 Ready control-plane,etcd,master 1m v1.29.9+k3s1\nmaster3 Ready control-plane,etcd,master 1m v1.29.9+k3s1\nworker1 Ready <none> 1m v1.29.9+k3s1\n", "", 0, nil
			case strings.Contains(cmd, "kubectl get nodes -l node-role.kubernetes.io/etcd"):
				return "master1 Ready control-plane,etcd\nmaster2 Ready control-plane,etcd\nmaster3 Ready control-plane,etcd\n", "", 0, nil
			default:
				return "", "", 0, nil
			}
		},
	}

	if err := kubernetes.NewPhase(cfg, fake).Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !hasCommand(fake.Commands, "192.168.10.11", "kubectl label node master2 drbd-status=primary") {
		t.Error("the DRBD primary (master2) was not labeled")
	}
	if hasCommand(fake.Commands, "192.168.10.11", "kubectl label node master1 drbd-status=primary") {
		t.Error("master1 was labeled primary even though DRBD reports master2 as Primary")
	}
}

func TestKubernetesPhaseReportsPrimaryLabelFailure(t *testing.T) {
	cfg := kubernetesConfig(t)
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			if strings.Contains(cmd, "kubectl label node") {
				return "", "Error from server (NotFound): node not found", 1, nil
			}
			switch {
			case strings.Contains(cmd, "cat /var/lib/rancher/k3s/server/node-token"):
				return "K10test::server:token\n", "", 0, nil
			case strings.Contains(cmd, "kubectl get nodes --no-headers"):
				return "master1 Ready control-plane,etcd,master 1m v1.29.9+k3s1\nmaster2 Ready control-plane,etcd,master 1m v1.29.9+k3s1\nmaster3 Ready control-plane,etcd,master 1m v1.29.9+k3s1\nworker1 Ready <none> 1m v1.29.9+k3s1\n", "", 0, nil
			case strings.Contains(cmd, "kubectl get nodes -l node-role.kubernetes.io/etcd"):
				return "master1 Ready control-plane,etcd\nmaster2 Ready control-plane,etcd\nmaster3 Ready control-plane,etcd\n", "", 0, nil
			default:
				return "", "", 0, nil
			}
		},
	}

	err := kubernetes.NewPhase(cfg, fake).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "could not label") {
		t.Fatalf("Run() = %v, want the primary-label failure", err)
	}
	if hasCommand(fake.Commands, "192.168.10.11", "apply -k /tmp/k3s-manifests/overlays/lab") {
		t.Error("phase continued to deploy workloads after the primary label failed")
	}
}

func TestKubernetesPhaseReportsServerJoinFailure(t *testing.T) {
	cfg := kubernetesConfig(t)
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, host, cmd string) (string, string, int, error) {
			if host == "192.168.10.12" && strings.Contains(cmd, "--server https://192.168.10.11:6443") {
				return "", "connection refused", 7, nil
			}
			switch {
			case strings.Contains(cmd, "cat /var/lib/rancher/k3s/server/node-token"):
				return "K10test::server:token\n", "", 0, nil
			case strings.Contains(cmd, "kubectl get nodes --no-headers"):
				return "master1 Ready control-plane,etcd,master 1m v1.29.9+k3s1\nmaster2 Ready control-plane,etcd,master 1m v1.29.9+k3s1\nmaster3 Ready control-plane,etcd,master 1m v1.29.9+k3s1\nworker1 Ready <none> 1m v1.29.9+k3s1\n", "", 0, nil
			case strings.Contains(cmd, "kubectl get nodes -l node-role.kubernetes.io/etcd"):
				return "master1 Ready control-plane,etcd\nmaster2 Ready control-plane,etcd\nmaster3 Ready control-plane,etcd\n", "", 0, nil
			default:
				return "", "", 0, nil
			}
		},
	}

	err := kubernetes.NewPhase(cfg, fake).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "join K3s master2") {
		t.Fatalf("Run() = %v, want the master2 join failure", err)
	}
}
