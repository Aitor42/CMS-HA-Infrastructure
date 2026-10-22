package puppet

import (
	"context"
	"encoding/json"
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

// Phase installs the Puppet server and converges every node.
type Phase struct {
	cfg  *config.Config
	pool *ssh.Pool
}

// NewPhase creates a new Puppet Phase.
func NewPhase(cfg *config.Config, pool *ssh.Pool) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

func (p *Phase) Name() string { return "04-setup-puppet" }
func (p *Phase) Description() string {
	return "Install Puppet Server on Jumpstart and Puppet Agents on all nodes"
}

// tailLines returns the last few non-empty lines of s for error reporting.
func tailLines(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n\r"), "\n")
	if len(lines) > 10 {
		lines = lines[len(lines)-10:]
	}
	return strings.Join(lines, "\n")
}

func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()
	if p.cfg.Database.Password == "" || p.cfg.Database.ExporterPassword == "" || p.cfg.WordPress.AdminPassword == "" {
		return fmt.Errorf("database.password, database.exporter_password, and wordpress.admin_password must be set in config.yaml")
	}
	for name, secret := range map[string]string{
		"database.password":          p.cfg.Database.Password,
		"database.exporter_password": p.cfg.Database.ExporterPassword,
		"wordpress.admin_password":   p.cfg.WordPress.AdminPassword,
	} {
		if strings.ContainsAny(secret, "\r\n") {
			return fmt.Errorf("%s cannot contain line breaks", name)
		}
	}

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
	stdout, stderr, err := phases.RunPuppetAgentCmd(ctx, p.pool, jumpstartIP, "jumpstart", phases.PuppetAgentCommand("jumpstart.internal.local"))
	if err != nil {
		// Jumpstart convergence bootstraps the puppetserver; letting a failed
		// catalog slide here makes every later phase fail with confusing
		// errors much further down the line.
		logging.Warn("puppet agent output on %s (tail):\n%s", jumpstartIP, tailLines(stdout))
		return fmt.Errorf("puppet agent failed on jumpstart %s: %v\n%s", jumpstartIP, err, tailLines(stderr))
	}

	agentNodes := p.getAgentNodes()

	logging.Info("Installing Puppet Agent on all client nodes...")
	if err := p.installAgents(ctx, agentNodes); err != nil {
		return fmt.Errorf("install puppet agents: %w", err)
	}
	if err := p.installRuntimeSecretFiles(ctx); err != nil {
		return fmt.Errorf("install runtime credentials for CMS and database exporter: %w", err)
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

// installRuntimeSecretFiles puts credentials on the nodes that consume them,
// without exposing them as Puppet facts or embedding them in Puppet code.
func (p *Phase) installRuntimeSecretFiles(ctx context.Context) error {
	if p.cfg.Database.Password == "" || p.cfg.Database.ExporterPassword == "" || p.cfg.WordPress.AdminPassword == "" {
		return fmt.Errorf("database.password, database.exporter_password, and wordpress.admin_password must be set")
	}
	credentials, err := json.Marshal(map[string]map[string]string{
		"cms_ha": {
			"database_password":          p.cfg.Database.Password,
			"database_exporter_password": p.cfg.Database.ExporterPassword,
			"wordpress_admin_password":   p.cfg.WordPress.AdminPassword,
		},
	})
	if err != nil {
		return fmt.Errorf("marshal runtime facts: %w", err)
	}
	exporterCredentials, err := json.Marshal(map[string]map[string]string{
		"cms_ha": {"database_exporter_password": p.cfg.Database.ExporterPassword},
	})
	if err != nil {
		return fmt.Errorf("marshal exporter credentials: %w", err)
	}

	type target struct {
		name        string
		credentials []byte
	}
	nodes := make(map[string]target)
	for _, master := range p.cfg.Nodes.Masters {
		if master.IP != "" {
			nodes[master.IP] = target{name: master.Name, credentials: exporterCredentials}
		}
	}
	for _, frontend := range p.cfg.Nodes.CMSFrontends {
		if frontend.IP != "" {
			nodes[frontend.IP] = target{name: frontend.Name, credentials: credentials}
		}
	}
	tasks := make(map[string]func(context.Context, *ssh.Pool) error, len(nodes))
	for ip, node := range nodes {
		node := node
		nodeIP := ip
		tasks[nodeIP] = func(ctx context.Context, pool *ssh.Pool) error {
			_, _, code, err := pool.RunCommand(ctx, nodeIP, "install -d -m 0700 /etc/cms-ha")
			if err != nil {
				return fmt.Errorf("prepare credentials directory on %s (%s): %w", node.name, nodeIP, err)
			}
			if code != 0 {
				return fmt.Errorf("prepare credentials directory on %s (%s) exited with code %d", node.name, nodeIP, code)
			}
			if err := pool.CopyContent(ctx, nodeIP, node.credentials, "/etc/cms-ha/runtime-secrets.json", 0600); err != nil {
				return fmt.Errorf("copy runtime credentials to %s (%s): %w", node.name, nodeIP, err)
			}
			return nil
		}
	}
	for _, res := range p.pool.RunParallelFunc(ctx, tasks) {
		if res.Err != nil {
			return res.Err
		}
	}
	return nil
}

func (p *Phase) installPuppetServer(ctx context.Context, jumpstartIP string) error {
	if err := p.pool.WaitForAptLock(ctx, jumpstartIP); err != nil {
		return fmt.Errorf("wait for apt lock on jumpstart: %w", err)
	}
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

	_, _, _, err := p.pool.RunCommand(ctx, jumpstartIP, fmt.Sprintf("mkdir -p %s", remoteCodeDir))
	if err != nil {
		return err
	}

	err = p.pool.CopyDir(ctx, jumpstartIP, localPuppetDir, remoteCodeDir)
	if err != nil {
		return fmt.Errorf("copy dir failed: %w", err)
	}

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
				set -e
				export DEBIAN_FRONTEND=noninteractive
				%s
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

				systemctl enable puppet
			`, ssh.AptLockWaitSnippet, jumpstartIP, nodeSpec.FQDN)

			_, stderr, code, err := pool.RunCommand(ctx, nodeSpec.IP, cmd)
			if err != nil || code != 0 {
				msg := strings.TrimSpace(stderr)
				if msg == "" {
					msg = "(no stderr)"
				}
				return fmt.Errorf("exit code %d: %s: %w", code, msg, err)
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

			// Stop the background daemon first (same session) to avoid catalog
			// run lock contention.
			cmd := `
				systemctl stop puppet 2>/dev/null || true
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
			if code != 0 && (strings.Contains(stdout, "does not match its private key") || strings.Contains(stderr, "does not match its private key")) {
				logging.Warn("Certificate mismatch detected on %s (%s). Performing self-healing SSL reset...", spec.Name, spec.FQDN)
				p.pool.RunCommand(ctx, jumpstartIP, fmt.Sprintf("puppetserver ca clean --certname %s 2>/dev/null || true", spec.FQDN))
				p.pool.RunCommand(ctx, spec.IP, "rm -rf /etc/puppetlabs/puppet/ssl /etc/puppet/ssl")
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
