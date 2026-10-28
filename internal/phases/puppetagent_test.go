package phases

import (
	"strings"
	"testing"
)

func TestPuppetAgentCommandWithServiceKeepsTheDaemonRunning(t *testing.T) {
	cmd := PuppetAgentCommandWithService("puppet.lab.example")
	for _, want := range []string{
		"systemctl stop puppet",
		"puppet agent -t --server puppet.lab.example",
		"systemctl start puppet",
		"systemctl is-active --quiet puppet",
		"systemctl start puppet 2>/dev/null || true",
		"exit $code",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("PuppetAgentCommandWithService() is missing %q:\n%s", want, cmd)
		}
	}
}
