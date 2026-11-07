package phases

import (
	"context"
	"fmt"

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

// PuppetAgentCommandWithService runs the canonical one-shot convergence with
// the daemon stopped, then restarts and verifies it on both success and
// failure. A manual `agent -t` while the daemon is enabled can otherwise leave
// Puppet stopped after a failed catalog, so the node silently stops enforcing
// its configuration.
func PuppetAgentCommandWithService(server string) string {
	return fmt.Sprintf(`set +e
systemctl stop puppet 2>/dev/null || true
%s
code=$?
if [ $code -eq 0 ] || [ $code -eq 2 ]; then
	if ! systemctl start puppet; then
		echo "puppet service failed to start after a successful catalog" >&2
		exit 1
	fi
	if ! systemctl is-active --quiet puppet; then
		echo "puppet service is not active after a successful catalog" >&2
		exit 1
	fi
	exit 0
fi
systemctl start puppet 2>/dev/null || true
exit $code
`, PuppetAgentCommand(server))
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
