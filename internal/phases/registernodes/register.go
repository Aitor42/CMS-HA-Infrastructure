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
	
	for _, node := range p.cfg.AllNodes() {
		if node.Name == p.cfg.Nodes.Jumpstart.Name {
			continue
		}
		
		logging.Info("Registering node %s...", node.Name)
		
		// Check if exists
		out, _, code, _ := p.pool.RunCommand(ctx, jumpIP, fmt.Sprintf("cobbler system report --name=%s", node.Name))
		
		cmd := "cobbler system add"
		if code == 0 && !strings.Contains(out, "No system found") {
			cmd = "cobbler system edit"
		}
		
		mac := node.MAC
		if mac == "" {
			mac = "00:00:00:00:00:00" // Fallback if MAC isn't populated
		}
		
		fullCmd := fmt.Sprintf("%s --name=%s --profile=ubuntu-24.04-x86_64 --hostname=%s --ip-address=%s --mac-address=%s --autoinstall-meta='hostname=%s' --netboot-enabled=1", 
			cmd, node.Name, node.Name, node.IP, mac, node.Name)
			
		_, err := p.pool.RunScript(ctx, jumpIP, fullCmd)
		if err != nil {
			logging.Error("Failed to register node %s: %v", node.Name, err)
		}
	}
	
	logging.Info("Synchronizing Cobbler...")
	_, _, code, err := p.pool.RunCommand(ctx, jumpIP, "cobbler sync")
	if err != nil || code != 0 {
		return fmt.Errorf("failed to sync cobbler: %w", err)
	}

	logging.Success("Nodes registered successfully")
	return nil
}
