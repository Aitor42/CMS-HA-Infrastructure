package initvms

import (
	"context"
	"fmt"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/hypervisor"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
)

func (p *Phase) clientProvisioningMode() (string, error) {
	if p.opts.FastProvisioning {
		return "cloud", nil
	}
	switch mode := p.cfg.ClientProvisioningMode(); mode {
	case "pxe", "cloud":
		return mode, nil
	default:
		return "", fmt.Errorf("invalid vm.provisioning %q: want \"pxe\" or \"cloud\"", mode)
	}
}

// deployClientNodes provisions all client nodes, either through PXE (the
// deliverable path) or from the cloud image (the opt-in fast path).
func (p *Phase) deployClientNodes(ctx context.Context) error {
	mode, err := p.clientProvisioningMode()
	if err != nil {
		return err
	}
	logging.Info("Client install path: %s", mode)
	if mode == "cloud" {
		return p.deployCloudClientNodes(ctx)
	}

	logging.Info("Deploying client nodes...")

	// The jumpstart is installed from its ISO rather than over PXE, so it is
	// the one entry of the topology this phase must leave alone. CreateClientVM
	// also settles each new guest's boot order, which is not something this
	// phase can do afterwards: a running guest keeps the order it booted with.
	for _, node := range p.cfg.AllNodes() {
		if node.Name == "" || node.Name == p.cfg.Nodes.Jumpstart.Name {
			continue
		}
		if err := hypervisor.CreateClientVM(ctx, p.cfg, p.lv, node.Name); err != nil {
			return fmt.Errorf("failed to create VM %s: %w", node.Name, err)
		}
	}

	logging.Success("All client nodes deployed")
	return nil
}

// deployCloudClientNodes provisions every client node from one cloud image:
// each VM gets a qcow2 overlay and a per-node cloud-init seed ISO, so the OS is
// ready as soon as the guest boots instead of waiting for a PXE install.
func (p *Phase) deployCloudClientNodes(ctx context.Context) error {
	logging.Info("Deploying client nodes from the cloud image...")

	baseImage, err := findCloudImage(p.cfg.VM.StorageDir, p.cfg.VM.JumpstartBaseImage)
	if err != nil {
		return err
	}
	logging.Info("Using Ubuntu cloud image: %s", baseImage)

	pubKey, err := p.ensureSSHKey(p.cfg.SSH.PrivateKey)
	if err != nil {
		return fmt.Errorf("SSH key setup failed: %w", err)
	}
	passHash, err := generateRandomPasswordHash()
	if err != nil {
		return fmt.Errorf("generate admin password hash: %w", err)
	}

	for _, node := range p.cfg.AllNodes() {
		if node.Name == "" || node.Name == p.cfg.Nodes.Jumpstart.Name {
			continue
		}
		detail, nets, _, err := hypervisor.ClientVMDefinition(ctx, p.cfg, p.lv, node.Name)
		if err != nil {
			return fmt.Errorf("resolve VM %s: %w", node.Name, err)
		}
		data := cloudNodeData{
			Hostname:     detail.Name,
			PasswordHash: passHash,
			PubKey:       pubKey,
			Interfaces:   cloudInterfaces(p.cfg, detail, nets),
		}
		seedISO, err := p.createCloudNodeSeed(ctx, detail.Name, data)
		if err != nil {
			return fmt.Errorf("prepare cloud-init for %s: %w", detail.Name, err)
		}
		if err := hypervisor.CreateCloudClientVM(ctx, p.cfg, p.lv, detail.Name, baseImage, seedISO); err != nil {
			return fmt.Errorf("failed to create VM %s: %w", detail.Name, err)
		}
	}

	logging.Success("All client nodes deployed from the cloud image")
	return nil
}

// cloudInterfaces translates the topology's NIC list into cloud-init ethernet
// stanzas. The cloud image has no DHCP server to lean on before the jumpstart is
// configured, so lab-facing NICs get fixed addresses; only the WAN uplink uses
// DHCP.
func cloudInterfaces(cfg *config.Config, node config.NodeDetail, nets []libvirt.NetworkOpt) []cloudInterface {
	ifaces := make([]cloudInterface, 0, len(nets))
	for i, net := range nets {
		iface := cloudInterface{
			Name:   fmt.Sprintf("net%d", i),
			MAC:    net.Mac,
			Metric: 200,
		}
		switch net.Network {
		case "internal":
			iface.IP = node.IP
			if node.Name == cfg.Nodes.Router.Name {
				iface.IP = cfg.Network.Internal.Gateway
			}
			if node.Name != cfg.Nodes.Router.Name {
				iface.Gateway = cfg.Network.Internal.Gateway
			}
			iface.DNS = cfg.Nodes.Jumpstart.IP
		case "main":
			iface.IP = node.IPMain
			if iface.IP == "" {
				iface.IP = node.IP
			}
			if node.Name == cfg.Nodes.Router.Name {
				iface.IP = cfg.Network.Main.Gateway
			} else {
				iface.Gateway = cfg.Network.Main.Gateway
			}
			iface.DNS = cfg.Nodes.Jumpstart.IPMain
			if iface.DNS == "" {
				iface.DNS = cfg.Nodes.Jumpstart.IP
			}
		default:
			// The WAN uplink (libvirt "default") is the only interface that
			// uses DHCP and a lower route metric, matching the jumpstart.
			iface.DHCP = true
			iface.Metric = 100
		}
		if iface.DNS == "" {
			iface.DNS = firstNonEmpty(iface.Gateway, cfg.Nodes.Jumpstart.IP)
		}
		ifaces = append(ifaces, iface)
	}
	return ifaces
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// CleanupOptions specifies options for the cleanup operation.
