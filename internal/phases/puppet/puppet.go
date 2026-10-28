package puppet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/catalog"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/shell"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

const defaultJumpstartFQDN = "jumpstart.internal.local"

// Phase installs the Puppet server and converges every node.
type Phase struct {
	cfg  *config.Config
	pool ssh.Executor
}

// NewPhase creates a new Puppet Phase.
func NewPhase(cfg *config.Config, pool ssh.Executor) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

// jumpstartServerName is the Puppet server name used by every agent. It must
// follow the configured FQDN: the autosign policy is generated from the same
// value, so a hardcoded name would leave a deployment with a custom domain
// unable to establish TLS with the server.
func (p *Phase) jumpstartServerName() string {
	if fqdn := strings.TrimSpace(p.cfg.Nodes.Jumpstart.FQDN); fqdn != "" {
		return fqdn
	}
	return defaultJumpstartFQDN
}

func (p *Phase) Name() string        { return catalog.Puppet.Name }
func (p *Phase) Description() string { return catalog.Puppet.Description }

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
	stdout, stderr, err := phases.RunPuppetAgentCmd(ctx, p.pool, jumpstartIP, "jumpstart", phases.PuppetAgentCommand(p.jumpstartServerName()))
	if err != nil {
		// Jumpstart convergence bootstraps the puppetserver; letting a failed
		// catalog slide here makes every later phase fail with confusing
		// errors much further down the line.
		logging.Warn("puppet agent output on %s (tail):\n%s", jumpstartIP, logging.Tail(stdout, 10))
		return fmt.Errorf("puppet agent failed on jumpstart %s: %v\n%s", jumpstartIP, err, logging.Tail(stderr, 10))
	}

	agentNodes := p.getAgentNodes()

	// Autosign only the FQDNs this configuration is about to bring up. A
	// wildcard (*.internal.local) would let any host that can reach the
	// Puppet server obtain a catalog; exact names keep the trust boundary at
	// the configured topology.
	if err := p.writeAutosignPolicy(ctx, jumpstartIP, agentNodes); err != nil {
		return fmt.Errorf("write exact Puppet autosign policy: %w", err)
	}

	logging.Info("Installing Puppet Agent on all client nodes...")
	if err := p.installAgents(ctx, agentNodes); err != nil {
		return fmt.Errorf("install puppet agents: %w", err)
	}
	if err := p.installRuntimeSecretFiles(ctx); err != nil {
		return fmt.Errorf("install runtime credentials for CMS and database exporter: %w", err)
	}

	logging.Info("Running first puppet agent catalog on all nodes...")
	if err := p.runFirstCatalog(ctx, agentNodes); err != nil {
		return fmt.Errorf("run first catalog: %w", err)
	}

	logging.Success("Puppet setup completed successfully")
	return nil
}

// databaseHosts returns the MariaDB NodePort endpoints in failover order. The
// WordPress configuration uses the list to probe both DRBD peers instead of
// hardcoding one address in the Puppet manifest.
func (p *Phase) databaseHosts() string {
	port := p.cfg.Database.Port
	if port == 0 {
		port = 30306
	}
	hosts := make([]string, 0, 2)
	for _, master := range p.cfg.Nodes.Masters {
		if master.IP == "" {
			continue
		}
		hosts = append(hosts, fmt.Sprintf("%s:%d", master.IP, port))
		if len(hosts) == 2 {
			break
		}
	}
	return strings.Join(hosts, ",")
}

// installRuntimeSecretFiles puts credentials on the nodes that consume them,
// without exposing them as Puppet facts or embedding them in Puppet code.
func (p *Phase) installRuntimeSecretFiles(ctx context.Context) error {
	if p.cfg.Database.Password == "" || p.cfg.Database.ExporterPassword == "" || p.cfg.WordPress.AdminPassword == "" {
		return fmt.Errorf("database.password, database.exporter_password, and wordpress.admin_password must be set")
	}
	credentials, err := json.Marshal(map[string]map[string]string{
		"cms_ha": {
			"database_name":              p.cfg.Database.Name,
			"database_user":              p.cfg.Database.User,
			"database_password":          p.cfg.Database.Password,
			"database_exporter_password": p.cfg.Database.ExporterPassword,
			"wordpress_admin_password":   p.cfg.WordPress.AdminPassword,
			"database_hosts":             p.databaseHosts(),
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
	tasks := make(map[string]func(context.Context, ssh.Executor) error, len(nodes))
	for ip, node := range nodes {
		node := node
		nodeIP := ip
		tasks[nodeIP] = func(ctx context.Context, pool ssh.Executor) error {
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
		set -euo pipefail
		shopt -s nullglob
		export DEBIAN_FRONTEND=noninteractive
		
		# Ensure puppet user and group exist
		id -u puppet >/dev/null 2>&1 || adduser --system --group --home /var/lib/puppet --shell /bin/false puppet
		mkdir -p /usr/share/puppet/modules

		# Setup official Puppet 8 repository if not present
		if ! dpkg -s puppet8-release >/dev/null 2>&1; then
			wget --timeout=30 --tries=3 -q https://apt.puppet.com/puppet8-release-noble.deb -O /tmp/puppet8-release-noble.deb
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

		# The exact autosign policy is written by the phase once the node
		# list is known; never seed it with a wildcard.
		install -m 0644 /dev/null /etc/puppetlabs/puppet/autosign.conf
		ln -sf /etc/puppetlabs/puppet/autosign.conf /etc/puppet/autosign.conf

		grep -q "puppet" /etc/hosts || echo "127.0.0.1 puppet" >> /etc/hosts

		systemctl enable puppetserver
		systemctl restart puppetserver
		systemctl is-active --quiet puppetserver

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

	if _, stderr, code, err := p.pool.RunCommand(ctx, jumpstartIP, fmt.Sprintf("mkdir -p %s", remoteCodeDir)); err != nil || code != 0 {
		return fmt.Errorf("create remote code directory (exit %d): %v %s", code, err, strings.TrimSpace(stderr))
	}

	// Replace the previous environment instead of layering files on top of it.
	// Stale manifests left behind by a rename or deletion have caused Puppet to
	// keep applying code that is no longer in the repository.
	clearCmd := fmt.Sprintf("find %s -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +", remoteCodeDir)
	if _, stderr, code, err := p.pool.RunCommand(ctx, jumpstartIP, clearCmd); err != nil || code != 0 {
		return fmt.Errorf("clear remote code directory (exit %d): %v %s", code, err, strings.TrimSpace(stderr))
	}

	if err := p.pool.CopyDir(ctx, jumpstartIP, localPuppetDir, remoteCodeDir); err != nil {
		return fmt.Errorf("copy dir failed: %w", err)
	}

	chownCmd := fmt.Sprintf("chown -R puppet:puppet %s && chmod -R u+rwX,go+rX %s && test -f %s/manifests/site.pp && test -d %s/modules", remoteCodeDir, remoteCodeDir, remoteCodeDir, remoteCodeDir)
	if _, stderr, code, err := p.pool.RunCommand(ctx, jumpstartIP, chownCmd); err != nil || code != 0 {
		return fmt.Errorf("secure/verify remote code (exit %d): %v %s", code, err, strings.TrimSpace(stderr))
	}

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

// writeAutosignPolicy replaces Puppet's wildcard autosign with the exact
// certnames this deployment is allowed to bring up. The file is written before
// the agents request their certificates, so no other host in either internal
// domain can obtain a signed catalog.
func (p *Phase) writeAutosignPolicy(ctx context.Context, jumpstartIP string, nodes []config.NodeSpec) error {
	policy := autosignPolicy(p.cfg.Nodes.Jumpstart.FQDN, nodes)
	const path = "/etc/puppetlabs/puppet/autosign.conf"
	if err := p.pool.CopyContent(ctx, jumpstartIP, []byte(policy), path, 0644); err != nil {
		return fmt.Errorf("copy autosign policy: %w", err)
	}
	if _, stderr, code, err := p.pool.RunCommand(ctx, jumpstartIP,
		"chown root:root "+path+" && chmod 0644 "+path); err != nil || code != 0 {
		return fmt.Errorf("secure autosign policy (exit %d): %v %s", code, err, strings.TrimSpace(stderr))
	}
	return nil
}

// autosignPolicy returns one exact FQDN per line, sorted for deterministic
// output. It intentionally never emits a wildcard.
func autosignPolicy(jumpstartFQDN string, nodes []config.NodeSpec) string {
	if strings.TrimSpace(jumpstartFQDN) == "" {
		jumpstartFQDN = defaultJumpstartFQDN
	}
	names := map[string]struct{}{jumpstartFQDN: {}}
	for _, n := range nodes {
		if fqdn := strings.TrimSpace(n.FQDN); fqdn != "" {
			names[fqdn] = struct{}{}
		}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	return strings.Join(sorted, "\n") + "\n"
}

func (p *Phase) installAgents(ctx context.Context, nodes []config.NodeSpec) error {
	jumpstartIP := p.cfg.Nodes.Jumpstart.IP
	server := p.jumpstartServerName()
	tasks := make(map[string]func(ctx context.Context, pool ssh.Executor) error)

	for _, n := range nodes {
		nodeSpec := n
		tasks[nodeSpec.IP] = func(ctx context.Context, pool ssh.Executor) error {
			cmd := fmt.Sprintf(`
				set -euo pipefail
				export DEBIAN_FRONTEND=noninteractive
				%s
				if ! dpkg -s puppet8-release >/dev/null 2>&1; then
					wget --timeout=30 --tries=3 -q https://apt.puppet.com/puppet8-release-noble.deb -O /tmp/puppet8-release-noble.deb
					dpkg -i /tmp/puppet8-release-noble.deb || true
					apt-get update
				fi
				dpkg -s puppet-agent >/dev/null 2>&1 || apt-get install -o Dpkg::Options::="--force-confold" -o Dpkg::Options::="--force-confdef" -y puppet-agent

				ln -sf /opt/puppetlabs/bin/puppet /usr/local/bin/puppet
				grep -q "%s" /etc/hosts || echo "%s %s jumpstart puppet" >> /etc/hosts

				mkdir -p /etc/puppetlabs/puppet
				cat << 'EOF_PUPPET' > /etc/puppetlabs/puppet/puppet.conf
[main]
server = %s
environment = production
runinterval = 30m
certname = %s
EOF_PUPPET

				systemctl enable puppet
			`, ssh.AptLockWaitSnippet, server, jumpstartIP, server, server, nodeSpec.FQDN)

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

// catalogConcurrency resolves the first-catalog concurrency: the
// PUPPET_CONCURRENCY environment variable wins, then deploy.puppet_concurrency,
// and finally an automatic value derived from the host's CPU count (4 to 8).
// The measured work was ~1,255s across 12 nodes with a limit of 4, so the
// scheduling tail (376s vs a 314s ideal) is what a wider window removes.
func (p *Phase) catalogConcurrency() (int, string) {
	if envC := strings.TrimSpace(os.Getenv("PUPPET_CONCURRENCY")); envC != "" {
		if c, err := strconv.Atoi(envC); err == nil && c > 0 {
			return c, "PUPPET_CONCURRENCY"
		}
		logging.Warn("Ignoring invalid PUPPET_CONCURRENCY=%q (expected a positive integer)", envC)
	}
	if c := p.cfg.Deploy.PuppetConcurrency; c > 0 {
		return c, "config"
	}
	c := runtime.NumCPU() / 2
	if c < 4 {
		c = 4
	}
	if c > 8 {
		c = 8
	}
	return c, "auto"
}

// firstCatalogCommand runs one agent catalog with the daemon stopped to avoid
// the catalog lock, then always leaves the service running. A failed catalog
// still restarts the daemon so the node keeps retrying instead of silently
// drifting with Puppet disabled.
func firstCatalogCommand(server string) string {
	return fmt.Sprintf(`
		set +e
		systemctl stop puppet 2>/dev/null || true
		/opt/puppetlabs/bin/puppet agent -t --server %s
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
	`, shell.Quote(server))
}

// isPuppetSSLResetError identifies failures that can be repaired by deleting
// the agent's stale certificate and asking the server for a new one. A key
// mismatch is the historical case; signature/verify failures are the same
// class of stale or partially rotated SSL state.
func isPuppetSSLResetError(stdout, stderr string) bool {
	combined := strings.ToLower(stdout + "\n" + stderr)
	for _, marker := range []string{
		"does not match its private key",
		"certificate signature failure",
		"certificate verify failed",
	} {
		if strings.Contains(combined, marker) {
			return true
		}
	}
	return false
}

func (p *Phase) runFirstCatalog(ctx context.Context, nodes []config.NodeSpec) error {
	var errs []string
	var mu sync.Mutex
	jumpstartIP := p.cfg.Nodes.Jumpstart.IP
	server := p.jumpstartServerName()
	cmd := firstCatalogCommand(server)

	concurrency, source := p.catalogConcurrency()
	if concurrency > len(nodes) {
		concurrency = len(nodes)
	}

	logging.Info("Running first catalog in parallel across %d nodes (concurrency limit: %d, source: %s)...", len(nodes), concurrency, source)

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

			nodeCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			stdout, stderr, code, err := p.pool.RunCommand(nodeCtx, spec.IP, cmd)

			// Self-healing: remove the stale client certificate and its server
			// record, then retry once. This covers both a private-key mismatch
			// and the signature/verify failures left by a partial CA rotation.
			if code != 0 && isPuppetSSLResetError(stdout, stderr) {
				logging.Warn("Stale Puppet SSL state detected on %s (%s). Performing self-healing reset...", spec.Name, spec.FQDN)
				if spec.FQDN != "" {
					cleanCmd := fmt.Sprintf("puppetserver ca clean --certname %s", shell.Quote(spec.FQDN))
					if _, cleanStderr, cleanCode, cleanErr := p.pool.RunCommand(nodeCtx, jumpstartIP, cleanCmd); cleanErr != nil || cleanCode != 0 {
						logging.Warn("Could not clean Puppet certificate for %s on jumpstart (exit %d): %v %s", spec.FQDN, cleanCode, cleanErr, logging.Tail(cleanStderr, 5))
					}
				}
				if _, resetStderr, resetCode, resetErr := p.pool.RunCommand(nodeCtx, spec.IP, "rm -rf /etc/puppetlabs/puppet/ssl /etc/puppet/ssl"); resetErr != nil || resetCode != 0 {
					logging.Warn("Could not remove stale Puppet SSL directory on %s (exit %d): %v %s", spec.IP, resetCode, resetErr, logging.Tail(resetStderr, 5))
				}
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
