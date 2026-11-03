package registernodes

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/catalog"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/cobbler"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/shell"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// Phase registers the client nodes with Cobbler.
type Phase struct {
	cfg  *config.Config
	pool ssh.Executor
}

// NewPhase creates a new node-registration Phase.
func NewPhase(cfg *config.Config, pool ssh.Executor) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

func (p *Phase) Name() string        { return catalog.RegisterNodes.Name }
func (p *Phase) Description() string { return catalog.RegisterNodes.Description }

func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()

	jumpIP := p.cfg.Nodes.Jumpstart.IP
	if jumpIP == "" {
		return fmt.Errorf("nodes.jumpstart.ip is empty; cannot register nodes in Cobbler")
	}

	script, err := p.registrationScript()
	if err != nil {
		return err
	}

	logging.Info("Synchronizing Cobbler...")
	if _, err := p.pool.RunScript(ctx, jumpIP, script); err != nil {
		return fmt.Errorf("failed to register nodes in Cobbler: %w", err)
	}

	logging.Success("Nodes registered successfully")
	return nil
}

// registrationScript builds the idempotent `cobbler system remove/add` payload.
// It returns an error instead of emitting a half-valid script when a node is
// missing the fields Cobbler needs.
func (p *Phase) registrationScript() (string, error) {
	jumpIP := p.cfg.Nodes.Jumpstart.IP
	jumpIPMain := p.cfg.Nodes.Jumpstart.IPMain
	// Classify nodes by actual subnet membership. Comparing textual prefixes
	// breaks as soon as main.cidr is not a /24 (for example 10.20.0.0/23,
	// where 10.20.1.5 is inside the network but does not share the prefix).
	_, mainNet, err := net.ParseCIDR(strings.TrimSpace(p.cfg.Network.Main.CIDR))
	if err != nil {
		return "", fmt.Errorf("network.main.cidr is invalid: %w", err)
	}

	var scriptBuilder strings.Builder
	scriptBuilder.WriteString("set -e\n")

	for _, node := range p.cfg.AllNodes() {
		if node.Name == p.cfg.Nodes.Jumpstart.Name || node.Name == "" {
			continue
		}

		mac := node.MAC
		if mac == "" {
			// A zero-MAC cobbler system will never PXE-boot reliably and can
			// collide silently with another entry; refuse to register it.
			return "", fmt.Errorf("node %s has no MAC address; cannot register it in Cobbler", node.Name)
		}
		if node.FQDN == "" {
			return "", fmt.Errorf("node %s has no FQDN; Cobbler cannot set its hostname", node.Name)
		}

		cobblerServerIP := jumpIP
		if ip := net.ParseIP(node.IP); ip != nil && mainNet.Contains(ip) {
			if jumpIPMain == "" {
				// Guessing <main-prefix>10 would point main-network clients at
				// an address that may belong to another node.
				return "", fmt.Errorf("node %s is on the main network but nodes.jumpstart.ip_main is empty", node.Name)
			}
			cobblerServerIP = jumpIPMain
		}

		// console=ttyS0 comes first so the kernel still logs to the serial
		// port (which the host can read through the domain's pty) while
		// console=tty0 stays last, keeping /dev/console on the VGA where the
		// installer draws its screen.
		kernelOpts := fmt.Sprintf("autoinstall ds=nocloud-net;s=http://%s/cblr/svc/op/autoinstall/system/%s/ netboot=nfs nfsroot=%s:%s boot=casper ip=dhcp console=ttyS0 console=tty0",
			cobblerServerIP, node.Name, cobblerServerIP, cobbler.MirrorPath)

		logging.Info("Registering node %s...", node.Name)
		scriptBuilder.WriteString(fmt.Sprintf("cobbler system remove --name=%s 2>/dev/null || true\n", shell.Quote(node.Name)))
		scriptBuilder.WriteString(fmt.Sprintf("cobbler system add --name=%s --profile=%s --hostname=%s --ip-address=%s --mac=%s --interface=ens3 --static=1 --server=%s --kernel-options=%s --autoinstall-meta=%s --netboot-enabled=1\n",
			shell.Quote(node.Name),
			shell.Quote(cobbler.ProfileName),
			shell.Quote(node.FQDN),
			shell.Quote(node.IP),
			shell.Quote(mac),
			shell.Quote(cobblerServerIP),
			shell.Quote(kernelOpts),
			shell.Quote("hostname="+node.Name),
		))
	}

	scriptBuilder.WriteString("cobbler sync\n")
	return scriptBuilder.String(), nil
}
