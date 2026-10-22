package phases

import (
	"context"
	"fmt"
	"strings"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// PuppetAgentCommand builds the canonical one-shot agent command. `agent -t`
// (test mode + detailed-exitcodes) exits 2 when it applied changes, so the
// shell guard turns that expected outcome into success.
func PuppetAgentCommand(server string) string {
	cmd := "/opt/puppetlabs/bin/puppet agent -t"
	if server != "" {
		cmd += " --server " + server
	}
	return cmd + " || [ $? -eq 2 ]"
}

// RunPuppetAgentCmd runs a puppet agent convergence on host with a custom
// command (e.g. a dedicated puppetserver name) and interprets the standard
// exit codes: 0 = clean run, 2 = changes applied; everything else fails and
// brings back stdout/stderr for richer caller diagnostics.
func RunPuppetAgentCmd(ctx context.Context, pool ssh.Executor, host, label, cmd string) (stdout, stderr string, err error) {
	stdout, stderr, code, runErr := pool.RunCommand(ctx, host, cmd)
	if runErr == nil && (code == 0 || code == 2) {
		return stdout, stderr, nil
	}
	return stdout, stderr, fmt.Errorf("puppet agent failed on %s %s (exit code %d): %v", label, host, code, runErr)
}

// RunPuppetAgent runs the canonical agent convergence on one host.
func RunPuppetAgent(ctx context.Context, pool ssh.Executor, host, label string) (string, string, error) {
	return RunPuppetAgentCmd(ctx, pool, host, label, PuppetAgentCommand(""))
}

// RunPuppetAgentParallel converges hosts concurrently and aggregates every
// failure (SSH-level error or unexpected exit code) into one descriptive
// error; tolerant callers decide whether to warn or abort.
func RunPuppetAgentParallel(ctx context.Context, pool ssh.Executor, hosts []string) error {
	var targets []string
	for _, h := range hosts {
		if h != "" {
			targets = append(targets, h)
		}
	}
	if len(targets) == 0 {
		return nil
	}

	var failures []string
	for _, r := range pool.RunParallel(ctx, targets, PuppetAgentCommand("")) {
		switch {
		case r.Err != nil:
			failures = append(failures, fmt.Sprintf("%s: %v", r.Host, r.Err))
		case r.ExitCode != 0 && r.ExitCode != 2:
			failures = append(failures, fmt.Sprintf("%s: exit code %d", r.Host, r.ExitCode))
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("puppet agent failed on %d/%d nodes:\n%s", len(failures), len(targets), strings.Join(failures, "\n"))
}
