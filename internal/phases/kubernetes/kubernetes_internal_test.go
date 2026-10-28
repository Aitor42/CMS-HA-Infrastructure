package kubernetes

import (
	"strings"
	"testing"
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
