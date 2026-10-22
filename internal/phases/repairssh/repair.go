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

// Phase repairs SSH access and redistributes the deploy key.
type Phase struct {
	cfg  *config.Config
	pool ssh.Executor
}

// NewPhase creates a new SSH-repair Phase.
func NewPhase(cfg *config.Config, pool ssh.Executor) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

func (p *Phase) Name() string { return "03-repair-ssh" }
func (p *Phase) Description() string {
	return "Repair SSH connectivity and distribute keys across all nodes"
}

func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()

	allNodes := p.cfg.AllNodes()
	allIPs := p.cfg.AllNodeIPs()

	logging.Info("Cleaning local known_hosts before connecting...")
	if err := p.cleanKnownHosts(allIPs); err != nil {
		logging.Warn("Failed to clean known_hosts: %v", err)
	}

	logging.Info("Waiting for SSH on all nodes...")
	if err := p.pool.WaitForAllSSH(ctx, allIPs, 5*time.Minute); err != nil {
		return fmt.Errorf("failed waiting for SSH: %w", err)
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

	logging.Success("SSH repair phase completed successfully")
	return nil
}

func (p *Phase) cleanKnownHosts(ips []string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	paths := []string{filepath.Clean(filepath.Join(homeDir, ".ssh", "known_hosts"))}
	// The orchestrator's own TOFU store must be cleaned too: a re-provisioned
	// VM presents a new host key, and the pinned entry would otherwise make the
	// connection fail closed (see internal/ssh verifyHostKey).
	if kh := p.cfg.SSHKnownHostsFile(); kh != "" {
		paths = append(paths, filepath.Clean(kh))
	}

	var firstErr error
	for _, knownHostsPath := range paths {
		if err := ssh.RemoveHostsFromKnownHosts(knownHostsPath, ips); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (p *Phase) setHostnames(ctx context.Context, nodes []config.NodeSpec) error {
	tasks := make(map[string]func(ctx context.Context, pool ssh.Executor) error)
	for _, n := range nodes {
		node := n // capture loop variable
		if node.IP == "" {
			continue
		}
		tasks[node.IP] = func(c context.Context, pool ssh.Executor) error {
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

// generateHosts writes a bootstrap /etc/hosts (derived from config.yaml) to
// every node *before* Puppet runs, so the agents can resolve the Puppet server
// by name. Puppet's role::base then owns the file as the steady-state desired
// configuration (it adds 127.0.1.1 and the hot-desk entries); keep the two in
// sync by hand when adding nodes.
func (p *Phase) generateHosts(ctx context.Context, nodes []config.NodeSpec) error {
	var sb strings.Builder
	sb.WriteString("127.0.0.1 localhost\n")
	for _, n := range nodes {
		if n.IP != "" && n.FQDN != "" {
			if n.Name == "jumpstart" {
				sb.WriteString(fmt.Sprintf("%s %s %s puppet\n", n.IP, n.FQDN, n.Name))
			} else {
				sb.WriteString(fmt.Sprintf("%s %s %s\n", n.IP, n.FQDN, n.Name))
			}
		}
	}
	hostsContent := sb.String()

	tasks := make(map[string]func(ctx context.Context, pool ssh.Executor) error)
	for _, n := range nodes {
		node := n
		if node.IP == "" {
			continue
		}
		tasks[node.IP] = func(c context.Context, pool ssh.Executor) error {
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

	checkCmd := "if [ ! -f /root/.ssh/id_ed25519 ]; then mkdir -p /root/.ssh && chmod 700 /root/.ssh && ssh-keygen -t ed25519 -N '' -f /root/.ssh/id_ed25519; fi"
	_, _, code, err := p.pool.RunCommand(ctx, jumpstartIP, checkCmd)
	if err != nil || code != 0 {
		return fmt.Errorf("failed to ensure jumpstart ssh key (exit %d): %v", code, err)
	}

	pubKeyStr, _, code, err := p.pool.RunCommand(ctx, jumpstartIP, "cat /root/.ssh/id_ed25519.pub")
	if err != nil || code != 0 {
		return fmt.Errorf("failed to read jumpstart public key (exit %d): %v", code, err)
	}
	pubKeyStr = strings.TrimSpace(pubKeyStr)
	if pubKeyStr == "" {
		return fmt.Errorf("jumpstart public key is empty")
	}

	tasks := make(map[string]func(ctx context.Context, pool ssh.Executor) error)
	// Inter-node SSH must use the non-root deploy account: the installers
	// disable direct root logins (PermitRootLogin no), so authorising the
	// jumpstart key for root would be dead weight.
	deployUser := p.cfg.SSHUser()
	deployHome := "/home/" + deployUser
	if deployUser == "root" {
		deployHome = "/root"
	}
	for _, n := range nodes {
		if n.IP == jumpstartIP || n.IP == "" {
			continue
		}
		nodeIP := n.IP
		tasks[nodeIP] = func(c context.Context, pool ssh.Executor) error {
			cmd := fmt.Sprintf(`
				mkdir -p %[1]s/.ssh
				chmod 700 %[1]s/.ssh
				grep -qxF '%[2]s' %[1]s/.ssh/authorized_keys 2>/dev/null || echo '%[2]s' >> %[1]s/.ssh/authorized_keys
				chmod 600 %[1]s/.ssh/authorized_keys
				chown -R %[3]s:%[3]s %[1]s/.ssh 2>/dev/null || true
			`, deployHome, pubKeyStr, deployUser)
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
