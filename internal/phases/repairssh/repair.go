package repairssh

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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

func (p *Phase) Name() string { return "03-repair-ssh" }
func (p *Phase) Description() string {
	return "Repair SSH connectivity and distribute keys across all nodes"
}

func (p *Phase) Run(ctx context.Context) error {
	logging.PhaseStart(p.Name())
	
	allNodes := p.cfg.AllNodes()
	allIPs := p.cfg.AllNodeIPs()
	
	logging.Info("Waiting for SSH on all nodes...")
	if err := p.pool.WaitForAllSSH(ctx, allIPs, 5*time.Minute); err != nil {
		return fmt.Errorf("failed waiting for SSH: %w", err)
	}
	
	logging.Info("Cleaning local known_hosts...")
	if err := p.cleanKnownHosts(allIPs); err != nil {
		logging.Warn("Failed to clean known_hosts: %v", err)
	}
	
	logging.Info("Setting hostnames on all nodes...")
	if err := p.setHostnames(ctx, allNodes); err != nil {
		return fmt.Errorf("failed setting hostnames: %w", err)
	}
	
	logging.Info("Generating and distributing /etc/hosts...")
	if err := p.generateHosts(ctx, allNodes); err != nil {
		return fmt.Errorf("failed generating hosts file: %w", err)
	}
	
	logging.Info("Propagating Jumpstart SSH key to all nodes...")
	if err := p.propagateSSHKeys(ctx, allNodes); err != nil {
		return fmt.Errorf("failed propagating SSH keys: %w", err)
	}
	
	logging.Info("Cleaning Puppet CA...")
	if err := p.cleanPuppetCA(ctx, allNodes); err != nil {
		logging.Warn("Failed cleaning Puppet CA: %v", err)
	}
	
	logging.Success("SSH repair phase completed successfully")
	return nil
}

func (p *Phase) cleanKnownHosts(ips []string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	
	knownHostsPath := filepath.Join(homeDir, ".ssh", "known_hosts")
	data, err := os.ReadFile(knownHostsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	
	lines := strings.Split(string(data), "\n")
	var newLines []string
	
	for _, line := range lines {
		keep := true
		for _, ip := range ips {
			if strings.Contains(line, ip) {
				keep = false
				break
			}
		}
		if keep {
			newLines = append(newLines, line)
		}
	}
	
	return os.WriteFile(knownHostsPath, []byte(strings.Join(newLines, "\n")), 0600)
}

func (p *Phase) setHostnames(ctx context.Context, nodes []config.NodeSpec) error {
	tasks := make(map[string]func(ctx context.Context, pool *ssh.Pool) error)
	for _, n := range nodes {
		node := n // capture loop variable
		if node.IP == "" {
			continue
		}
		tasks[node.IP] = func(c context.Context, pool *ssh.Pool) error {
			cmd := fmt.Sprintf("hostnamectl set-hostname %s", node.FQDN)
			_, _, code, err := pool.RunCommand(c, node.IP, cmd)
			if err != nil || code != 0 {
				return fmt.Errorf("failed to set hostname on %s (exit %d): %v", node.IP, code, err)
			}
			return nil
		}
	}
	
	results := p.pool.RunParallelFunc(ctx, tasks)
	for _, res := range results {
		if res.Err != nil {
			return res.Err
		}
	}
	return nil
}

func (p *Phase) generateHosts(ctx context.Context, nodes []config.NodeSpec) error {
	var sb strings.Builder
	sb.WriteString("127.0.0.1 localhost\n")
	for _, n := range nodes {
		if n.IP != "" && n.FQDN != "" {
			sb.WriteString(fmt.Sprintf("%s %s %s\n", n.IP, n.FQDN, n.Name))
		}
	}
	hostsContent := sb.String()
	
	tasks := make(map[string]func(ctx context.Context, pool *ssh.Pool) error)
	for _, n := range nodes {
		node := n
		if node.IP == "" {
			continue
		}
		tasks[node.IP] = func(c context.Context, pool *ssh.Pool) error {
			return pool.CopyContent(c, node.IP, []byte(hostsContent), "/etc/hosts", 0644)
		}
	}
	
	results := p.pool.RunParallelFunc(ctx, tasks)
	for _, res := range results {
		if res.Err != nil {
			return res.Err
		}
	}
	return nil
}

func (p *Phase) propagateSSHKeys(ctx context.Context, nodes []config.NodeSpec) error {
	jumpstartIP := p.cfg.Nodes.Jumpstart.IP
	if jumpstartIP == "" {
		return fmt.Errorf("jumpstart IP is empty")
	}
	
	// Check if jumpstart has an ssh key, if not generate it
	checkCmd := "if [ ! -f /root/.ssh/id_ed25519 ]; then mkdir -p /root/.ssh && chmod 700 /root/.ssh && ssh-keygen -t ed25519 -N '' -f /root/.ssh/id_ed25519; fi"
	_, _, code, err := p.pool.RunCommand(ctx, jumpstartIP, checkCmd)
	if err != nil || code != 0 {
		return fmt.Errorf("failed to ensure jumpstart ssh key (exit %d): %v", code, err)
	}
	
	// Read jumpstart public key
	pubKeyStr, _, code, err := p.pool.RunCommand(ctx, jumpstartIP, "cat /root/.ssh/id_ed25519.pub")
	if err != nil || code != 0 {
		return fmt.Errorf("failed to read jumpstart public key (exit %d): %v", code, err)
	}
	pubKeyStr = strings.TrimSpace(pubKeyStr)
	if pubKeyStr == "" {
		return fmt.Errorf("jumpstart public key is empty")
	}
	
	tasks := make(map[string]func(ctx context.Context, pool *ssh.Pool) error)
	for _, n := range nodes {
		if n.IP == jumpstartIP || n.IP == "" {
			continue
		}
		nodeIP := n.IP
		tasks[nodeIP] = func(c context.Context, pool *ssh.Pool) error {
			cmd := fmt.Sprintf(`
				mkdir -p /root/.ssh
				chmod 700 /root/.ssh
				grep -qxF '%s' /root/.ssh/authorized_keys 2>/dev/null || echo '%s' >> /root/.ssh/authorized_keys
				chmod 600 /root/.ssh/authorized_keys
			`, pubKeyStr, pubKeyStr)
			_, _, code, err := pool.RunCommand(c, nodeIP, cmd)
			if err != nil || code != 0 {
				return fmt.Errorf("failed to add pubkey on %s (exit %d): %v", nodeIP, code, err)
			}
			return nil
		}
	}
	
	results := p.pool.RunParallelFunc(ctx, tasks)
	for _, res := range results {
		if res.Err != nil {
			return res.Err
		}
	}
	return nil
}

func (p *Phase) cleanPuppetCA(ctx context.Context, nodes []config.NodeSpec) error {
	jumpstartIP := p.cfg.Nodes.Jumpstart.IP
	if jumpstartIP != "" {
		p.pool.RunCommand(ctx, jumpstartIP, "puppetserver ca clean --all 2>/dev/null || true")
	}
	
	tasks := make(map[string]func(ctx context.Context, pool *ssh.Pool) error)
	for _, n := range nodes {
		nodeIP := n.IP
		if nodeIP == "" {
			continue
		}
		tasks[nodeIP] = func(c context.Context, pool *ssh.Pool) error {
			pool.RunCommand(c, nodeIP, "find /etc/puppet /etc/puppetlabs -name *.pem -delete 2>/dev/null || true")
			return nil
		}
	}
	p.pool.RunParallelFunc(ctx, tasks)
	return nil
}
