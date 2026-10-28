package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// etcdMemberNames returns the node names in a `kubectl get nodes -l
// node-role.kubernetes.io/etcd --no-headers` listing. Counting names in Go
// keeps a stale node object from being mistaken for a live member, and lets the
// caller report which configured server is actually missing.
func etcdMemberNames(out string) map[string]bool {
	members := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		members[fields[0]] = true
	}
	return members
}

func runChecked(ctx context.Context, pool ssh.Executor, host, command, action string) error {
	stdout, stderr, code, err := pool.RunCommand(ctx, host, command)
	// A non-zero exit status arrives as an error as well, but it carries the
	// command's own output, which is what explains the failure. Only a
	// transport-level failure (code < 0) has nothing to add.
	if err != nil && code < 0 {
		return fmt.Errorf("%s: %w", action, err)
	}
	if code != 0 {
		// kubectl writes several classes of failures to stdout rather than
		// stderr, so keep both streams in the message.
		detail := strings.TrimSpace(stderr)
		if out := strings.TrimSpace(stdout); out != "" {
			if detail != "" {
				detail += " | "
			}
			detail += out
		}
		if detail == "" && err != nil {
			detail = err.Error()
		}
		return fmt.Errorf("%s failed (exit %d): %s", action, code, detail)
	}
	return nil
}

// transientAPIErrors are the failures a healthy but busy control plane
// produces while k3s settles: a restarted service, an etcd election, or the
// API server dropping connections under load. They say nothing about the
// manifest being applied, so they are worth retrying instead of failing a
// deployment that is otherwise fine.
var transientAPIErrors = []string{
	"connection refused",
	"connection reset by peer",
	"no route to host",
	"i/o timeout",
	"TLS handshake timeout",
	"unexpected EOF",
	"Unable to connect to the server",
	"The connection to the server",
	"the server is currently unable to handle the request",
	"ServiceUnavailable",
	"etcdserver: request timed out",
	"etcdserver: leader changed",
	"etcdserver: no leader",
}

// isTransientAPIError reports whether a kubectl failure is the control plane
// being briefly unavailable rather than the command being wrong.
func isTransientAPIError(detail string) bool {
	for _, marker := range transientAPIErrors {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return false
}

// runCheckedRetry is runChecked for idempotent kubectl commands (apply, get,
// delete, wait): a control-plane hiccup is retried for up to apiRetryBudget,
// while a real rejection (a bad manifest, a missing object) fails at once.
func runCheckedRetry(ctx context.Context, pool ssh.Executor, host, command, action string) error {
	deadline := time.Now().Add(apiRetryBudget)
	for attempt := 1; ; attempt++ {
		err := runChecked(ctx, pool, host, command, action)
		if err == nil {
			return nil
		}
		if !isTransientAPIError(err.Error()) || time.Now().After(deadline) {
			return err
		}
		logging.Warn("%s: the control plane is not answering yet (attempt %d); retrying", action, attempt)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// makeMariaDBSecret builds the Kubernetes Secret manifest from runtime config.
// JSON is valid YAML, and encoding the values here avoids storing credentials
// in the embedded Kubernetes manifests.
