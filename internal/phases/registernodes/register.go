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

// Phase registers the client nodes with Cobbler.
type Phase struct {
	cfg  *config.Config
	pool *ssh.Pool
}

// NewPhase creates a new node-registration Phase.
func NewPhase(cfg *config.Config, pool *ssh.Pool) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

func (p *Phase) Name() string        { return "02-register-nodes" }
func (p *Phase) Description() string { return "Register all nodes in Cobbler for PXE boot" }

func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()

	jumpIP := p.cfg.Nodes.Jumpstart.IP

	var scriptBuilder strings.Builder
	scriptBuilder.WriteString("set -e\n")

	mainPrefix := config.PrefixFromCIDR(p.cfg.Network.Main.CIDR, "192.168.20.")

	for _, node := range p.cfg.AllNodes() {
		if node.Name == p.cfg.Nodes.Jumpstart.Name || node.Name == "" {
			continue
		}

		mac := node.MAC
		if mac == "" {
			// A zero-MAC cobbler system will never PXE-boot reliably and can
			// collide silently with another entry; refuse to register it.
			return fmt.Errorf("node %s has no MAC address; cannot register it in Cobbler", node.Name)
		}

		cobblerServerIP := jumpIP
		if strings.HasPrefix(node.IP, mainPrefix) {
			if p.cfg.Nodes.Jumpstart.IPMain != "" {
				cobblerServerIP = p.cfg.Nodes.Jumpstart.IPMain
			} else {
				cobblerServerIP = mainPrefix + "10"
			}
		}

		// console=ttyS0 comes first so the kernel still logs to the serial
		// port (which the host can read through the domain's pty) while
		// console=tty0 stays last, keeping /dev/console on the VGA where the
		// installer draws its screen.
		kernelOpts := fmt.Sprintf("autoinstall ds=nocloud-net;s=http://%s/cblr/svc/op/autoinstall/system/%s/ netboot=nfs nfsroot=%s:/var/www/cobbler/distro_mirror/ubuntu-24.04 boot=casper ip=dhcp console=ttyS0 console=tty0",
			cobblerServerIP, node.Name, cobblerServerIP)

		logging.Info("Registering node %s...", node.Name)
		scriptBuilder.WriteString(fmt.Sprintf("cobbler system remove --name=%s 2>/dev/null || true\n", node.Name))
		scriptBuilder.WriteString(fmt.Sprintf("cobbler system add --name=%s --profile=ubuntu-24.04-x86_64 --hostname=%s --ip-address=%s --mac=%s --interface=ens3 --static=1 --server=%s --kernel-options=\"%s\" --autoinstall-meta='hostname=%s' --netboot-enabled=1\n",
			node.Name, node.FQDN, node.IP, mac, cobblerServerIP, kernelOpts, node.Name))
	}

	logging.Info("Synchronizing Cobbler...")
	scriptBuilder.WriteString("cobbler sync\n")

	if _, err := p.pool.RunScript(ctx, jumpIP, scriptBuilder.String()); err != nil {
		return fmt.Errorf("failed to register nodes in Cobbler: %w", err)
	}

	logging.Success("Nodes registered successfully")
	return nil
}
