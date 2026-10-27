package kubernetes

import (
	"strings"
	"testing"
)

func TestCountNonEmptyLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{
			name: "no members at all",
			in:   "",
			want: 0,
		},
		{
			name: "only whitespace and newlines",
			in:   "\n\n   \n\t\n",
			want: 0,
		},
		{
			name: "one member with trailing newline",
			in:   "node1 Ready control-plane,etcd,master 5m v1.29.15+k3s1\n",
			want: 1,
		},
		{
			name: "three members",
			in:   "a\nb\nc\n",
			want: 3,
		},
		{
			name: "no trailing newline",
			in:   "a\nb",
			want: 2,
		},
		{
			name: "blank lines between members are not members",
			in:   "a\n\nb\n",
			want: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countNonEmptyLines(tt.in); got != tt.want {
				t.Errorf("countNonEmptyLines(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
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
		t.Error("server install must wait for readiness")
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
