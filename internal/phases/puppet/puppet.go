package puppet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/catalog"
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
	if err := config.RequireSecrets(
		config.RequiredSecret{Name: "database.password", Value: p.cfg.Database.Password},
		config.RequiredSecret{Name: "database.exporter_password", Value: p.cfg.Database.ExporterPassword},
		config.RequiredSecret{Name: "wordpress.admin_password", Value: p.cfg.WordPress.AdminPassword},
	); err != nil {
		return err
	}

	jumpstartIP := p.cfg.Nodes.Jumpstart.IP
	if jumpstartIP == "" {
		return fmt.Errorf("jumpstart IP is empty")
	}

	agentNodes := p.getAgentNodes()

	// The server install and the agent installs are independent: an agent only
	// needs its package and puppet.conf, not a reachable server. Running them
	// together keeps the server install off the critical path. The autosign
	// policy is still written before the first agent requests a catalog.
	logging.Info("Installing Puppet Server on Jumpstart and Puppet Agent on %d client nodes...", len(agentNodes))
	if err := p.installServerAndAgents(ctx, jumpstartIP, agentNodes); err != nil {
		return err
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

	// Autosign only the FQDNs this configuration is about to bring up. A
	// wildcard (*.internal.local) would let any host that can reach the
	// Puppet server obtain a catalog; exact names keep the trust boundary at
	// the configured topology.
	if err := p.writeAutosignPolicy(ctx, jumpstartIP, agentNodes); err != nil {
		return fmt.Errorf("write exact Puppet autosign policy: %w", err)
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

// installServerAndAgents installs the Puppet server and every client agent
// concurrently. Agent installation only writes the package and puppet.conf, so
// it does not depend on a running server; overlapping the two saves the whole
// server install (apt, service start and CA bootstrap) from the deploy time.
// Both errors are reported, but the server error wins because a missing server
// makes the later catalog fail for every node.
func (p *Phase) installServerAndAgents(ctx context.Context, jumpstartIP string, nodes []config.NodeSpec) error {
	var wg sync.WaitGroup
	var serverErr, agentsErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := p.installPuppetServer(ctx, jumpstartIP); err != nil {
			serverErr = fmt.Errorf("install puppet server: %w", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := p.installAgents(ctx, nodes); err != nil {
			agentsErr = fmt.Errorf("install puppet agents: %w", err)
		}
	}()
	wg.Wait()

	if serverErr != nil {
		return serverErr
	}
	return agentsErr
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

	// site.pp and the Hiera data are generated from config.yaml: the committed
	// copies describe the default lab, but node names, addresses, domains and
	// the deploy account are configurable. Uploading the static files would
	// classify renamed nodes as role::base and reset a custom ssh.user to
	// ubuntu on every catalog.
	if err := p.writeGeneratedPuppetData(ctx, jumpstartIP, remoteCodeDir); err != nil {
		return err
	}

	chownCmd := fmt.Sprintf("chown -R puppet:puppet %s && chmod -R u+rwX,go+rX %s && test -f %s/manifests/site.pp && test -f %s/hiera.yaml && test -d %s/modules", remoteCodeDir, remoteCodeDir, remoteCodeDir, remoteCodeDir, remoteCodeDir)
	if _, stderr, code, err := p.pool.RunCommand(ctx, jumpstartIP, chownCmd); err != nil || code != 0 {
		return fmt.Errorf("secure/verify remote code (exit %d): %v %s", code, err, strings.TrimSpace(stderr))
	}

	return nil
}

// writeGeneratedPuppetData renders the node classifier, Hiera data and
// Prometheus scrape configuration from the configured topology.
func (p *Phase) writeGeneratedPuppetData(ctx context.Context, jumpstartIP, remoteCodeDir string) error {
	if _, stderr, code, err := p.pool.RunCommand(ctx, jumpstartIP, "mkdir -p "+remoteCodeDir+"/data "+remoteCodeDir+"/modules/role/files/prometheus"); err != nil || code != 0 {
		return fmt.Errorf("create generated data directories (exit %d): %v %s", code, err, strings.TrimSpace(stderr))
	}

	site, err := p.generatedSitePP()
	if err != nil {
		return err
	}
	if err := p.pool.CopyContent(ctx, jumpstartIP, site, remoteCodeDir+"/manifests/site.pp", 0644); err != nil {
		return fmt.Errorf("upload generated site.pp: %w", err)
	}

	data, err := p.generatedHieraData()
	if err != nil {
		return err
	}
	if err := p.pool.CopyContent(ctx, jumpstartIP, data, remoteCodeDir+"/data/common.yaml", 0644); err != nil {
		return fmt.Errorf("upload generated Hiera data: %w", err)
	}

	prometheus, err := p.generatedPrometheusConfig()
	if err != nil {
		return err
	}
	if err := p.pool.CopyContent(ctx, jumpstartIP, prometheus, remoteCodeDir+"/modules/role/files/prometheus/prometheus.yml", 0644); err != nil {
		return fmt.Errorf("upload generated prometheus.yml: %w", err)
	}
	return nil
}

// hostsContent mirrors the bootstrap /etc/hosts that 03-repair-ssh writes, so
// Puppet's steady state does not replace it with the default lab's addresses.
func (p *Phase) hostsContent() string {
	caHost := strings.TrimSpace(p.cfg.PKI.Domain)
	if caHost == "" {
		caHost = "ca.internal.local"
	}
	var b strings.Builder
	for _, n := range p.cfg.AllNodes() {
		if n.IP == "" || n.FQDN == "" {
			continue
		}
		if n.Name == p.cfg.Nodes.Jumpstart.Name {
			fmt.Fprintf(&b, "%s\t%s %s puppet %s ca\n", n.IP, n.FQDN, n.Name, caHost)
			continue
		}
		fmt.Fprintf(&b, "%s\t%s %s\n", n.IP, n.FQDN, n.Name)
	}
	if vip := p.cfg.LBVIP(); vip != "" {
		domain := strings.TrimSpace(p.cfg.Network.Main.Domain)
		if domain == "" {
			domain = "main.local"
		}
		fmt.Fprintf(&b, "%s\tmain-lb.%s main-lb\n", vip, domain)
	}
	return b.String()
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
		if name := certnameForSpec(n); name != "" {
			names[name] = struct{}{}
		}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	return strings.Join(sorted, "\n") + "\n"
}

// certnameForSpec is the certname a node presents to the Puppet server. The
// generated classifier uses the same FQDN-else-name fallback, so autosign and
// node classification always agree.
func certnameForSpec(n config.NodeSpec) string {
	if fqdn := strings.TrimSpace(n.FQDN); fqdn != "" {
		return fqdn
	}
	return strings.TrimSpace(n.Name)
}

func (p *Phase) installAgents(ctx context.Context, nodes []config.NodeSpec) error {
	jumpstartIP := p.cfg.Nodes.Jumpstart.IP
	server := p.jumpstartServerName()
	tasks := make(map[string]func(ctx context.Context, pool ssh.Executor) error)

	for _, n := range nodes {
		nodeSpec := n
		// The generated classifier keys nodes by FQDN and falls back to the
		// short name. The agent's certname must use the same value or an
		// FQDN-less node is classified as role::base (or fails to start with
		// an empty certname).
		certname := certnameForSpec(nodeSpec)
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
			`, ssh.AptLockWaitSnippet, server, jumpstartIP, server, server, certname)

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
