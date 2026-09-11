package registernodes

import (
	"context"
	"fmt"
	"strings"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

type Phase struct {
	cfg  *config.Config
	pool *ssh.Pool
}

func NewPhase(cfg *config.Config, pool *ssh.Pool) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

func (p *Phase) Name() string        { return "02-register-nodes" }
func (p *Phase) Description() string { return "Register all nodes in Cobbler for PXE boot" }

func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()

	jumpIP := p.cfg.Nodes.Jumpstart.IP
	
	// Pre-fetch all existing Cobbler systems in a single command (O(1) roundtrip)
	logging.Info("Querying existing Cobbler systems...")
	out, _, _, _ := p.pool.RunCommand(ctx, jumpIP, "cobbler system list")
	existingSystems := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		if name != "" {
			existingSystems[name] = true
		}
	}

	var scriptBuilder strings.Builder
	scriptBuilder.WriteString("set -e\n")

	for _, node := range p.cfg.AllNodes() {
		if node.Name == p.cfg.Nodes.Jumpstart.Name || node.Name == "" {
			continue
		}
		
		cmd := "cobbler system add"
		if existingSystems[node.Name] {
			cmd = "cobbler system edit"
		}
		
		mac := node.MAC
		if mac == "" {
			mac = "00:00:00:00:00:00" // Fallback if MAC isn't populated
		}
		
		logging.Info("Registering node %s (%s)...", node.Name, cmd)
		scriptBuilder.WriteString(fmt.Sprintf("%s --name=%s --profile=ubuntu-24.04-x86_64 --hostname=%s --ip-address=%s --mac-address=%s --autoinstall-meta='hostname=%s' --netboot-enabled=1\n", 
			cmd, node.Name, node.Name, node.IP, mac, node.Name))
	}
	
	logging.Info("Synchronizing Cobbler...")
	scriptBuilder.WriteString("cobbler sync\n")
	
	if _, err := p.pool.RunScript(ctx, jumpIP, scriptBuilder.String()); err != nil {
		return fmt.Errorf("failed to register nodes in Cobbler: %w", err)
	}

	logging.Success("Nodes registered successfully")
	return nil
}
