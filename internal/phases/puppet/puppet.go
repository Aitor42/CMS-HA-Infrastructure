package puppet

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
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

func (p *Phase) Name() string { return "04-setup-puppet" }
func (p *Phase) Description() string {
	return "Install Puppet Server on Jumpstart and Puppet Agents on all nodes"
}

func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()

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

	logging.Info("Applying Puppet catalog on Jumpstart...")
	p.pool.RunCommand(ctx, jumpstartIP, "/opt/puppetlabs/bin/puppet agent -t --server jumpstart.internal.local || [ $? -eq 2 ]")

	agentNodes := p.getAgentNodes()

	logging.Info("Installing Puppet Agent on all client nodes...")
	if err := p.installAgents(ctx, agentNodes); err != nil {
		return fmt.Errorf("install puppet agents: %w", err)
	}

	logging.Info("Signing any pending agent certificates on Puppet Server...")
	p.pool.RunCommand(ctx, jumpstartIP, "puppetserver ca sign --all 2>/dev/null || true")

	logging.Info("Running first puppet agent catalog on all nodes...")
	if err := p.runFirstCatalog(ctx, agentNodes); err != nil {
		return fmt.Errorf("run first catalog: %w", err)
	}

	logging.Success("Puppet setup completed successfully")
	return nil
}

func (p *Phase) installPuppetServer(ctx context.Context, jumpstartIP string) error {
	p.pool.WaitForAptLock(ctx, jumpstartIP)
	cmd := `
		export DEBIAN_FRONTEND=noninteractive
		
		# Ensure puppet user and group exist
		id -u puppet >/dev/null 2>&1 || adduser --system --group --home /var/lib/puppet --shell /bin/false puppet
		mkdir -p /usr/share/puppet/modules

		# Setup official Puppet 8 repository if not present
		if ! dpkg -l | grep -q puppet8-release; then
			wget -q https://apt.puppet.com/puppet8-release-noble.deb -O /tmp/puppet8-release-noble.deb
			dpkg -i /tmp/puppet8-release-noble.deb || true
			apt-get update
		fi

		# Install puppet-agent and puppetserver
		dpkg -s puppet-agent >/dev/null 2>&1 || apt-get install -y puppet-agent
		dpkg -s puppetserver >/dev/null 2>&1 || apt-get install -y puppetserver

		# Ensure puppet binaries are in PATH
		ln -sf /opt/puppetlabs/bin/puppet /usr/local/bin/puppet

		# Configure 512m memory limit for memory-constrained environments
		sed -i -re 's/(-Xms)[0-9a-zA-Z]+ (-Xmx)[0-9a-zA-Z]+/\1512m \2512m/' /etc/default/puppetserver

		# Link puppet vendor ruby libraries into puppetserver's jruby path
		mkdir -p /usr/lib/puppetserver/ruby/vendor_ruby
		if [ -d /opt/puppetlabs/puppet/lib/ruby/vendor_ruby ]; then
			for f in /opt/puppetlabs/puppet/lib/ruby/vendor_ruby/*; do
				ln -sf "$f" /usr/lib/puppetserver/ruby/vendor_ruby/
			done
		fi

		# Ensure compatibility symlinks between Debian and Puppetlabs paths
		mkdir -p /etc/puppetlabs/code /etc/puppetlabs/puppet /etc/puppetlabs/puppetserver /var/lib/puppet
		[ -e /etc/puppet/code ] || ln -sfn /etc/puppetlabs/code /etc/puppet/code
		[ -e /var/lib/puppet/ssl ] || ln -sfn /etc/puppet/ssl /var/lib/puppet/ssl
		[ -e /etc/puppetlabs/puppet/ssl ] || ln -sfn /etc/puppet/ssl /etc/puppetlabs/puppet/ssl
		[ -e /etc/puppetlabs/puppetserver/puppetserver ] || ln -sfn /etc/puppet/puppetserver /etc/puppetlabs/puppetserver/puppetserver
		chown -R puppet:puppet /etc/puppet /etc/puppetlabs /var/lib/puppet 2>/dev/null || true

		# Setup autosign.conf
		echo "*.internal.local" > /etc/puppetlabs/puppet/autosign.conf
		echo "*.main.local" >> /etc/puppetlabs/puppet/autosign.conf
		chmod 644 /etc/puppetlabs/puppet/autosign.conf
		ln -sf /etc/puppetlabs/puppet/autosign.conf /etc/puppet/autosign.conf

		# Ensure local puppet alias in /etc/hosts
		grep -q "puppet" /etc/hosts || echo "127.0.0.1 puppet" >> /etc/hosts

		systemctl enable puppetserver
		if ! systemctl is-active --quiet puppetserver; then
			systemctl start puppetserver
		fi

		puppetserver ca list --all >/dev/null || puppetserver ca setup || true

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
	localPuppetDir := "puppet"
	if _, err := os.Stat(localPuppetDir); os.IsNotExist(err) {
		for _, candidate := range []string{"../puppet", "../../puppet"} {
			if _, err := os.Stat(candidate); err == nil {
				localPuppetDir = candidate
				break
			}
		}
	}
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
	
	// Fix ownership and permissions
	chownCmd := fmt.Sprintf("chown -R puppet:puppet %s && chmod -R u+rwX,go+rX %s", remoteCodeDir, remoteCodeDir)
	p.pool.RunCommand(ctx, jumpstartIP, chownCmd)

	return nil
}

func (p *Phase) getAgentNodes() []config.NodeSpec {
	var nodes []config.NodeSpec
	jumpstartIP := p.cfg.Nodes.Jumpstart.IP
	for _, node := range p.cfg.AllNodes() {
		if node.IP != "" && node.IP != jumpstartIP {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

func (p *Phase) installAgents(ctx context.Context, nodes []config.NodeSpec) error {
	jumpstartIP := p.cfg.Nodes.Jumpstart.IP
	tasks := make(map[string]func(ctx context.Context, pool *ssh.Pool) error)

	for _, n := range nodes {
		nodeSpec := n
		tasks[nodeSpec.IP] = func(ctx context.Context, pool *ssh.Pool) error {
			cmd := fmt.Sprintf(`
				export DEBIAN_FRONTEND=noninteractive
				cloud-init status --wait 2>/dev/null || true
				for i in {1..30}; do
					if ! fuser /var/lib/dpkg/lock-frontend >/dev/null 2>&1 && ! fuser /var/lib/apt/lists/lock >/dev/null 2>&1; then
						break
					fi
					sleep 2
				done
				if ! dpkg -l | grep -q puppet8-release; then
					wget -q https://apt.puppet.com/puppet8-release-noble.deb -O /tmp/puppet8-release-noble.deb
					dpkg -i /tmp/puppet8-release-noble.deb || true
					apt-get update
				fi
				dpkg -s puppet-agent >/dev/null 2>&1 || apt-get install -o Dpkg::Options::="--force-confold" -o Dpkg::Options::="--force-confdef" -y puppet-agent
				
				ln -sf /opt/puppetlabs/bin/puppet /usr/local/bin/puppet
				grep -q "jumpstart.internal.local" /etc/hosts || echo "%s jumpstart.internal.local jumpstart puppet" >> /etc/hosts
				
				mkdir -p /etc/puppetlabs/puppet
				cat << 'EOF_PUPPET' > /etc/puppetlabs/puppet/puppet.conf
[main]
server = jumpstart.internal.local
environment = production
runinterval = 30m
certname = %s
EOF_PUPPET

				systemctl enable puppet || true
			`, jumpstartIP, nodeSpec.FQDN)

			_, _, code, err := pool.RunCommand(ctx, nodeSpec.IP, cmd)
			if err != nil || code != 0 {
				return fmt.Errorf("exit code %d: %w", code, err)
			}
			return nil
		}
	}

	results := p.pool.RunParallelFunc(ctx, tasks)
	var errs []string
	for _, res := range results {
		if res.Err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", res.Host, res.Err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("failed to install agents on some nodes:\n%s", strings.Join(errs, "\n"))
	}
	return nil
}

func (p *Phase) runFirstCatalog(ctx context.Context, nodes []config.NodeSpec) error {
	var errs []string
	var mu sync.Mutex
	jumpstartIP := p.cfg.Nodes.Jumpstart.IP

	concurrency := 4
	if envC := os.Getenv("PUPPET_CONCURRENCY"); envC != "" {
		if c, err := strconv.Atoi(envC); err == nil && c > 0 {
			concurrency = c
		}
	}
	if concurrency > len(nodes) {
		concurrency = len(nodes)
	}

	logging.Info("Running first catalog in parallel across %d nodes (concurrency limit: %d)...", len(nodes), concurrency)

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for _, n := range nodes {
		node := n
		wg.Add(1)
		go func(spec config.NodeSpec) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			logging.Info("Applying Puppet catalog on %s (%s)...", spec.Name, spec.IP)

			// Stop background daemon to avoid catalog run lock contention
			p.pool.RunCommand(ctx, spec.IP, "systemctl stop puppet 2>/dev/null || true")

			cmd := `
				/opt/puppetlabs/bin/puppet agent -t --server jumpstart.internal.local
				code=$?
				if [ $code -eq 0 ] || [ $code -eq 2 ]; then
					systemctl start puppet || true
					exit 0
				fi
				exit $code
			`
			nodeCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			stdout, stderr, code, err := p.pool.RunCommand(nodeCtx, spec.IP, cmd)

			// Self-healing: if SSL certificate does not match private key, revoke and purge
			combinedOutput := stdout + " " + stderr
			if code != 0 && strings.Contains(combinedOutput, "does not match its private key") {
				logging.Warn("Certificate mismatch detected on %s (%s). Performing self-healing SSL reset...", spec.Name, spec.FQDN)
				p.pool.RunCommand(ctx, jumpstartIP, fmt.Sprintf("puppetserver ca clean --certname %s 2>/dev/null || true", spec.FQDN))
				p.pool.RunCommand(ctx, spec.IP, "rm -rf /etc/puppetlabs/puppet/ssl /etc/puppet/ssl")
				// Retry agent run
				stdout, stderr, code, err = p.pool.RunCommand(nodeCtx, spec.IP, cmd)
			}

			if err != nil || code != 0 {
				mu.Lock()
				errs = append(errs, fmt.Sprintf("%s (%s): exit %d: %v\nSTDOUT: %s\nSTDERR: %s", spec.Name, spec.IP, code, err, strings.TrimSpace(stdout), strings.TrimSpace(stderr)))
				mu.Unlock()
			} else {
				logging.Success("Node %s converged successfully", spec.Name)
			}
		}(node)
	}

	wg.Wait()

	if len(errs) > 0 {
		return fmt.Errorf("failed catalog run on some nodes:\n%s", strings.Join(errs, "\n"))
	}
	return nil
}
