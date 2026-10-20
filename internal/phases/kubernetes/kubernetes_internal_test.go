package kubernetes

import "testing"

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
