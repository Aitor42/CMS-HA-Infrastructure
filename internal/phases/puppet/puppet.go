package puppet

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

func (p *Phase) Name() string { return "04-setup-puppet" }
func (p *Phase) Description() string {
	return "Install Puppet Server on Jumpstart and Puppet Agents on all nodes"
}

func (p *Phase) Run(ctx context.Context) error {
	logging.PhaseStart(p.Name())

	jumpstartIP := p.cfg.Nodes.Jumpstart.IP
	if jumpstartIP == "" {
		return fmt.Errorf("jumpstart IP is empty")
	}

	logging.Info("Installing Puppet Server on Jumpstart...")
	if err := p.installPuppetServer(ctx, jumpstartIP); err != nil {
		return fmt.Errorf("install puppet server: %w", err)
	}

	logging.Info("Uploading Puppet code (manifests + modules)...")
	if err := p.uploadPuppetCode(ctx, jumpstartIP); err != nil {
		return fmt.Errorf("upload puppet code: %w", err)
	}

	agentIPs := p.getAgentIPs()

	logging.Info("Installing Puppet Agent on all client nodes...")
	if err := p.installAgents(ctx, agentIPs); err != nil {
		return fmt.Errorf("install puppet agents: %w", err)
	}

	logging.Info("Running first puppet agent catalog on all nodes...")
	if err := p.runFirstCatalog(ctx, agentIPs); err != nil {
		return fmt.Errorf("run first catalog: %w", err)
	}

	logging.Success("Puppet setup completed successfully")
	return nil
}

func (p *Phase) installPuppetServer(ctx context.Context, jumpstartIP string) error {
	cmd := `
		export DEBIAN_FRONTEND=noninteractive
		wget -q https://apt.puppet.com/puppet8-release-noble.deb -O /tmp/puppet8-release-noble.deb
		dpkg -i /tmp/puppet8-release-noble.deb || true
		apt-get update
		apt-get install -y puppetserver
		sed -i -re 's/(-Xms)[0-9a-zA-Z]+ (-Xmx)[0-9a-zA-Z]+/\1512m \2512m/' /etc/default/puppetserver
		
		# Setup autosign.conf
		mkdir -p /etc/puppetlabs/puppet
		echo "*.internal.local" > /etc/puppetlabs/puppet/autosign.conf
		echo "*.main.local" >> /etc/puppetlabs/puppet/autosign.conf
		chmod 644 /etc/puppetlabs/puppet/autosign.conf
		
		systemctl enable puppetserver
		systemctl start puppetserver
		
		# Wait for API HTTP
		for i in {1..60}; do
			if puppetserver ca list --all >/dev/null 2>&1; then
				exit 0
			fi
			sleep 5
		done
		exit 1
	`
	_, _, code, err := p.pool.RunCommand(ctx, jumpstartIP, cmd)
	if err != nil || code != 0 {
		return fmt.Errorf("failed puppet server install on jumpstart (exit %d): %v", code, err)
	}
	return nil
}

func (p *Phase) uploadPuppetCode(ctx context.Context, jumpstartIP string) error {
	localPuppetDir := "puppet" // Assuming running from project root
	remoteCodeDir := "/etc/puppetlabs/code/environments/production"

	// Create directories
	_, _, _, err := p.pool.RunCommand(ctx, jumpstartIP, fmt.Sprintf("mkdir -p %s", remoteCodeDir))
	if err != nil {
		return err
	}

	err = p.pool.CopyDir(ctx, jumpstartIP, localPuppetDir, remoteCodeDir)
	if err != nil {
		return fmt.Errorf("copy dir failed: %w", err)
	}
	
	// Fix ownership
	chownCmd := fmt.Sprintf("chown -R puppet:puppet %s", remoteCodeDir)
	p.pool.RunCommand(ctx, jumpstartIP, chownCmd)

	return nil
}

func (p *Phase) getAgentIPs() []string {
	var ips []string
	jumpstartIP := p.cfg.Nodes.Jumpstart.IP
	for _, node := range p.cfg.AllNodes() {
		if node.IP != "" && node.IP != jumpstartIP {
			ips = append(ips, node.IP)
		}
	}
	return ips
}

func (p *Phase) installAgents(ctx context.Context, agentIPs []string) error {
	cmd := `
		export DEBIAN_FRONTEND=noninteractive
		wget -q https://apt.puppet.com/puppet8-release-noble.deb -O /tmp/puppet8-release-noble.deb
		dpkg -i /tmp/puppet8-release-noble.deb || true
		apt-get update
		apt-get install -y puppet-agent
		
		ln -sf /opt/puppetlabs/bin/puppet /usr/local/bin/puppet
		grep -q "jumpstart.internal.local" /etc/hosts || echo "192.168.10.10 jumpstart.internal.local jumpstart puppet" >> /etc/hosts
		
		mkdir -p /etc/puppetlabs/puppet
		cat << 'EOF_PUPPET' > /etc/puppetlabs/puppet/puppet.conf
[main]
server = jumpstart.internal.local
environment = production
runinterval = 30m
EOF_PUPPET

		/opt/puppetlabs/bin/puppet resource service puppet ensure=running enable=true
	`
	results := p.pool.RunParallel(ctx, agentIPs, cmd)
	var errs []string
	for _, res := range results {
		if res.Err != nil || res.ExitCode != 0 {
			errs = append(errs, fmt.Sprintf("%s: (code %d) %v", res.Host, res.ExitCode, res.Err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("failed to install agents on some nodes:\n%s", strings.Join(errs, "\n"))
	}
	return nil
}

func (p *Phase) runFirstCatalog(ctx context.Context, agentIPs []string) error {
	// The puppet agent -t command exits with 0 (no changes) or 2 (changes applied).
	cmd := "/opt/puppetlabs/bin/puppet agent -t --server jumpstart.internal.local || [ $? -eq 2 ]"
	results := p.pool.RunParallel(ctx, agentIPs, cmd)
	
	var errs []string
	for _, res := range results {
		if res.Err != nil || res.ExitCode != 0 {
			errs = append(errs, fmt.Sprintf("%s: (code %d) %v", res.Host, res.ExitCode, res.Err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("failed first catalog run on some nodes:\n%s", strings.Join(errs, "\n"))
	}
	return nil
}
