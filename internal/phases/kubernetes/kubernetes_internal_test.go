package kubernetes

import (
	"context"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

func TestEtcdMemberNames(t *testing.T) {
	out := "master1 Ready control-plane,etcd,master 5m v1.29.9+k3s1\n" +
		"master2 Ready control-plane,etcd,master 5m v1.29.9+k3s1\n" +
		"stale-node Ready control-plane,etcd,master 5m v1.29.9+k3s1\n" +
		"\n   \n"
	members := etcdMemberNames(out)
	for _, want := range []string{"master1", "master2", "stale-node"} {
		if !members[want] {
			t.Errorf("etcdMemberNames() missing %q: %v", want, members)
		}
	}
	if len(members) != 3 {
		t.Errorf("etcdMemberNames() returned %d members, want 3: %v", len(members), members)
	}
	if got := etcdMemberNames("\n\n   \n\t\n"); len(got) != 0 {
		t.Errorf("etcdMemberNames() on blank output = %v, want empty", got)
	}
}

// TestIsTransientAPIError pins the two sides of the retry decision: the
// control plane being briefly unavailable is retried (k3s restarted under
// memory pressure and came back ~15s later, which failed the whole deploy
// when a bare kubectl delete hit the gap), while a real rejection of the
// command is not.
func TestIsTransientAPIError(t *testing.T) {
	transient := []string{
		"clear previous MariaDB initialization Job failed (exit 1): The connection to the server 127.0.0.1:6443 was refused - did you specify the right host or port?",
		"apply MariaDB initialization Job failed (exit 1): Unable to connect to the server: dial tcp 127.0.0.1:6443: connect: connection refused",
		"wait for MariaDB pod failed (exit 1): error: unexpected EOF",
		"apply base kustomize overlay failed (exit 1): Error from server (ServiceUnavailable): the server is currently unable to handle the request",
		"apply Kubernetes namespace failed (exit 1): etcdserver: request timed out",
	}
	for _, detail := range transient {
		if !isTransientAPIError(detail) {
			t.Errorf("expected transient: %s", detail)
		}
	}

	permanent := []string{
		"apply MariaDB initialization Job failed (exit 1): error: no objects passed to apply",
		"apply Kubernetes namespace failed (exit 1): Error from server (NotFound): namespaces \"cms\" not found",
		"wait for MariaDB initialization Job failed (exit 1): error: timed out waiting for the condition",
		"install K3s on master1: Process exited with status 1",
	}
	for _, detail := range permanent {
		if isTransientAPIError(detail) {
			t.Errorf("expected permanent: %s", detail)
		}
	}
}

func TestK3sInstallCommandPinsVerifiedBinary(t *testing.T) {
	cmd := k3sInstallCommand("server --cluster-init", "", false)
	for _, want := range []string{
		"v1.29.9%2Bk3s1/k3s",
		k3sSHA256,
		"sha256sum -c -",
		"install -m 0755 \"$tmp\" /usr/local/bin/k3s",
		"INSTALL_K3S_SKIP_DOWNLOAD=true",
		"INSTALL_K3S_EXEC=\"server --cluster-init\"",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("k3s install command is missing %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "INSTALL_K3S_CHANNEL") {
		t.Error("k3s install command must not resolve a channel")
	}
	for _, want := range []string{"--connect-timeout 15", "--max-time 180", "--max-time 60"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("k3s install command is missing timeout %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "INSTALL_K3S_SKIP_START=true") {
		t.Error("skipStart=false must let the upstream installer start the server")
	}
}

func TestK3sServerInstallSkipsBlockingStart(t *testing.T) {
	cmd := k3sInstallCommand("server --cluster-init", "", true)
	if !strings.Contains(cmd, "INSTALL_K3S_SKIP_START=true") {
		t.Errorf("k3s server command is missing INSTALL_K3S_SKIP_START=true:\n%s", cmd)
	}
	if strings.Contains(cmd, "systemctl restart") {
		t.Error("k3s server install must not block on systemctl restart")
	}
}

func TestK3sAgentInstallSkipsBlockingStart(t *testing.T) {
	cmd := k3sInstallCommand("--flannel-iface enp1s0", "K3S_TOKEN_FILE=/root/.k3s-join-token ", true)
	if !strings.Contains(cmd, "INSTALL_K3S_SKIP_START=true") {
		t.Errorf("k3s agent command is missing INSTALL_K3S_SKIP_START=true:\n%s", cmd)
	}
	if strings.Contains(cmd, "systemctl restart") {
		t.Error("k3s agent install must not block on systemctl restart")
	}
	if strings.Contains(cmd, "export K3S_TOKEN_FILE") {
		t.Error("k3s agent install must not export the token file path into the shell")
	}
	if !strings.Contains(cmd, "| K3S_TOKEN_FILE=/root/.k3s-join-token") {
		t.Error("k3s agent install must pass K3S_TOKEN_FILE to the install script")
	}
}

func TestReadyNodeNames(t *testing.T) {
	out := "master1 Ready control-plane,etcd 1m v1.29.9+k3s1\n" +
		"master2 NotReady control-plane,etcd 1m v1.29.9+k3s1\n" +
		"worker1 Ready <none> 1m v1.29.9+k3s1\n" +
		"worker2 Ready,SchedulingDisabled <none> 1m v1.29.9+k3s1\n" +
		"\n"
	ready := readyNodeNames(out)
	if !ready["master1"] || !ready["worker1"] || !ready["worker2"] {
		t.Errorf("Ready nodes = %v, want master1, worker1 and worker2", ready)
	}
	if ready["master2"] {
		t.Error("NotReady node was counted as Ready")
	}
}

func TestTokenFingerprintIsStableAndShort(t *testing.T) {
	got := tokenFingerprint("K10test::server:token")
	if len(got) != 12 {
		t.Errorf("fingerprint length = %d, want 12", len(got))
	}
	if got != tokenFingerprint("K10test::server:token") {
		t.Error("fingerprint is not stable")
	}
	if got == tokenFingerprint("K10other::server:token") {
		t.Error("different tokens produced the same fingerprint")
	}
}

func TestSyncBackupSchedulesAppliesConfiguredCronSpecs(t *testing.T) {
	cfg := &config.Config{}
	cfg.Backup.Engine = "garage"
	cfg.Backup.Schedule = "0 4 * * *"
	cfg.Backup.RestoreTestSchedule = "15 5 * * *"
	fake := &testutil.FakeExecutor{}
	p := &Phase{cfg: cfg, pool: fake}

	if err := p.syncBackupSchedules(context.Background(), config.NodeDetail{IP: "192.168.10.11"}); err != nil {
		t.Fatalf("syncBackupSchedules() = %v", err)
	}
	var joined strings.Builder
	for _, cmd := range fake.Commands {
		joined.WriteString(cmd.Cmd)
		joined.WriteString("\n")
	}
	for _, want := range []string{
		`kubectl patch cronjob mariadb-backup -n cms --type merge -p '{"spec":{"schedule":"0 4 * * *"}}'`,
		`kubectl patch cronjob mariadb-restore-test -n cms --type merge -p '{"spec":{"schedule":"15 5 * * *"}}'`,
	} {
		if !strings.Contains(joined.String(), want) {
			t.Errorf("syncBackupSchedules did not apply %q:\n%s", want, joined.String())
		}
	}
}

func TestSyncBackupSchedulesFailsWhenThePatchIsRejected(t *testing.T) {
	cfg := &config.Config{}
	cfg.Backup.Engine = "garage"
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			if strings.Contains(cmd, "mariadb-backup") {
				return "", "cronjob not found", 1, nil
			}
			return "", "", 0, nil
		},
	}
	p := &Phase{cfg: cfg, pool: fake}

	err := p.syncBackupSchedules(context.Background(), config.NodeDetail{IP: "192.168.10.11"})
	if err == nil || !strings.Contains(err.Error(), "mariadb-backup") {
		t.Fatalf("syncBackupSchedules() = %v, want the rejected patch", err)
	}
}

func TestRemoveBackupStackDeletesCronJobsAndCredentials(t *testing.T) {
	fake := &testutil.FakeExecutor{}
	p := &Phase{cfg: &config.Config{}, pool: fake}

	if err := p.removeBackupStack(context.Background(), config.NodeDetail{IP: "192.168.10.11"}); err != nil {
		t.Fatalf("removeBackupStack() = %v", err)
	}
	var joined strings.Builder
	for _, cmd := range fake.Commands {
		joined.WriteString(cmd.Cmd)
		joined.WriteString("\n")
	}
	for _, want := range []string{
		"kubectl delete cronjob mariadb-backup mariadb-restore-test -n cms --ignore-not-found=true",
		"kubectl delete secret backup-secret -n cms --ignore-not-found=true",
		"kubectl delete configmap backup-config -n cms --ignore-not-found=true",
	} {
		if !strings.Contains(joined.String(), want) {
			t.Errorf("removeBackupStack did not run %q:\n%s", want, joined.String())
		}
	}
}

func TestFindDRBDPrimaryRequiresBothReplicationPeers(t *testing.T) {
	cfg := &config.Config{}
	cfg.Nodes.Masters = []config.NodeDetail{
		{Name: "m1", IP: "192.168.10.11", DRBDDiskGB: 3},
		{Name: "m2", IP: "192.168.10.12"},
	}
	p := &Phase{cfg: cfg, pool: &testutil.FakeExecutor{}}

	_, err := p.findDRBDPrimary(context.Background())
	if err == nil || !strings.Contains(err.Error(), "both replication peers") {
		t.Fatalf("findDRBDPrimary() = %v, want the single-peer configuration error", err)
	}
}

func TestFindDRBDPrimaryQueriesOnlyTheReplicationPair(t *testing.T) {
	cfg := &config.Config{}
	cfg.Nodes.Masters = []config.NodeDetail{
		{Name: "m1", IP: "192.168.10.11", DRBDDiskGB: 3},
		{Name: "m2", IP: "192.168.10.12", DRBDDiskGB: 3},
		{Name: "m3", IP: "192.168.10.16", DRBDDiskGB: 3},
	}
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, host, _ string) (string, string, int, error) {
			if host == "192.168.10.11" {
				return "Primary/Unknown\n", "", 0, nil
			}
			return "Secondary/Unknown\n", "", 0, nil
		},
	}
	p := &Phase{cfg: cfg, pool: fake}

	primary, err := p.findDRBDPrimary(context.Background())
	if err != nil {
		t.Fatalf("findDRBDPrimary() = %v", err)
	}
	if primary.Name != "m1" {
		t.Errorf("findDRBDPrimary() = %q, want m1", primary.Name)
	}
	for _, cmd := range fake.Commands {
		if cmd.Host == "192.168.10.16" {
			t.Errorf("findDRBDPrimary queried the third control-plane node, which has no DRBD resource: %q", cmd.Cmd)
		}
	}
}

func TestLabelPrimaryClearsEveryStaleLabel(t *testing.T) {
	cfg := &config.Config{}
	cfg.Nodes.Masters = []config.NodeDetail{
		{Name: "m1", IP: "192.168.10.11", FQDN: "m1.internal.local", DRBDDiskGB: 3},
		{Name: "m2", IP: "192.168.10.12", FQDN: "m2.internal.local", DRBDDiskGB: 3},
	}
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, host, _ string) (string, string, int, error) {
			if host == "192.168.10.11" {
				return "Primary/Unknown\n", "", 0, nil
			}
			return "Secondary/Unknown\n", "", 0, nil
		},
	}
	p := &Phase{cfg: cfg, pool: fake}

	if err := p.labelPrimary(context.Background(), cfg.Nodes.Masters[0]); err != nil {
		t.Fatalf("labelPrimary() = %v", err)
	}
	var joined strings.Builder
	for _, cmd := range fake.Commands {
		joined.WriteString(cmd.Cmd)
		joined.WriteString("\n")
	}
	if !strings.Contains(joined.String(), "kubectl get nodes -l drbd-status=primary") {
		t.Errorf("labelPrimary did not select stale primaries by label:\n%s", joined.String())
	}
	if !strings.Contains(joined.String(), `case "$node" in m1.internal.local|m1)`) {
		t.Errorf("labelPrimary did not keep the current primary:\n%s", joined.String())
	}
}
