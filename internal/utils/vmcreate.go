package utils

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
)

// EnsureDiskPathAvailable refuses to reuse a disk file that no VM claims, so a
// half-deleted lab cannot silently be overwritten.
func EnsureDiskPathAvailable(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("disk %s exists without a registered VM; refusing to overwrite it (use init-vms --recreate after confirming the data can be discarded)", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("check disk path %s: %w", path, err)
	}
	return nil
}

// CreateClientVM defines and boots one client node so it PXE-installs, using
// the RAM, the disks and the NICs the topology assigns to that node. It is the
// single creation path for client nodes: `phase init-vms` walks the whole
// topology with it, and `vm install-batches` creates one batch with it right
// before installing that batch, which is what keeps a host with little RAM
// from having to hold every guest at once.
func CreateClientVM(ctx context.Context, cfg *config.Config, lv *libvirt.Client, name string) error {
	if name == "" {
		return fmt.Errorf("client VM with an empty name")
	}

	exists, err := lv.DomainExistsChecked(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		logging.Info("VM %s already exists; leaving it intact (use --recreate to replace it)", name)
		return nil
	}

	node, nets, extraDisks, err := clientVMDefinition(ctx, cfg, lv, name)
	if err != nil {
		return err
	}

	logging.Info("Creating VM: %s (RAM: %d MB, vCPUs: %d, Disk: %d GB)",
		node.Name, node.RAMInstallMB, node.VCPUs, node.DiskGB)

	diskPath := filepath.Join(cfg.VM.StorageDir, node.Name+".qcow2")
	if err := EnsureDiskPathAvailable(diskPath); err != nil {
		return err
	}
	for _, disk := range extraDisks {
		if err := EnsureDiskPathAvailable(disk.Path); err != nil {
			return err
		}
	}

	disks := []libvirt.DiskOpt{{Path: diskPath, SizeGB: node.DiskGB}}
	disks = append(disks, extraDisks...)

	if err := lv.VirtInstall(ctx, libvirt.VirtInstallOpts{
		Name:          node.Name,
		RAM:           node.RAMInstallMB,
		VCPUs:         node.VCPUs,
		Disks:         disks,
		Networks:      nets,
		PXE:           true,
		BootOrder:     []string{"hd", "network"},
		OSVariant:     "ubuntu24.04",
		NoAutoConsole: true,
		Wait:          0,
	}); err != nil {
		return err
	}

	// libvirt runs qemu as its own user, so the disks must be readable by it.
	_ = os.Chmod(diskPath, 0666)
	for _, disk := range extraDisks {
		_ = os.Chmod(disk.Path, 0666)
	}
	return nil
}

// clientVMDefinition maps a node name to its definition and to the virtual
// networks and extra disks the topology assigns to it. The router is the only
// node with three NICs (internal, main and a WAN uplink) and a master may
// carry the extra disk DRBD replicates, so both need the node details rather
// than the summary the topology exposes elsewhere.
func clientVMDefinition(ctx context.Context, cfg *config.Config, lv *libvirt.Client, name string) (config.NodeDetail, []libvirt.NetworkOpt, []libvirt.DiskOpt, error) {
	if router := cfg.Nodes.Router; name == router.Name {
		wanNet := "default"
		if !lv.NetExists(ctx, "default") {
			wanNet = "main"
			logging.Warn("'default' network not available, using 'main' as WAN for router")
		}
		nets := []libvirt.NetworkOpt{
			{Network: "internal", Mac: router.MACInternal},
			{Network: "main", Mac: router.MACMain},
			{Network: wanNet, Mac: router.MACWAN},
		}
		return router, nets, nil, nil
	}

	// Internal network nodes.
	for _, node := range []config.NodeDetail{cfg.Nodes.Monitor, cfg.Nodes.Storage} {
		if name == node.Name {
			return node, []libvirt.NetworkOpt{{Network: "internal", Mac: node.MAC}}, nil, nil
		}
	}
	for _, master := range cfg.Nodes.Masters {
		if name == master.Name {
			var extraDisks []libvirt.DiskOpt
			if master.DRBDDiskGB > 0 {
				// The shipped topology sets this only on master1/master2, so
				// DRBD stays a bounded 2-node setup.
				extraDisks = append(extraDisks, libvirt.DiskOpt{
					Path:   filepath.Join(cfg.VM.StorageDir, master.Name+"-drbd.qcow2"),
					SizeGB: master.DRBDDiskGB,
				})
			}
			return master, []libvirt.NetworkOpt{{Network: "internal", Mac: master.MAC}}, extraDisks, nil
		}
	}
	for _, worker := range cfg.Nodes.Workers {
		if name == worker.Name {
			return worker, []libvirt.NetworkOpt{{Network: "internal", Mac: worker.MAC}}, nil, nil
		}
	}

	// Main network nodes.
	for _, lb := range cfg.Nodes.LBs {
		if name == lb.Name {
			return lb, []libvirt.NetworkOpt{{Network: "main", Mac: lb.MAC}}, nil, nil
		}
	}
	for _, cms := range cfg.Nodes.CMSFrontends {
		if name == cms.Name {
			return cms, []libvirt.NetworkOpt{{Network: "main", Mac: cms.MAC}}, nil, nil
		}
	}
	for _, hotdesk := range cfg.HotdeskSpecs() {
		if name == hotdesk.Name {
			node := config.NodeDetail{
				Name:         hotdesk.Name,
				IP:           hotdesk.IP,
				FQDN:         hotdesk.FQDN,
				MAC:          hotdesk.MAC,
				RAMInstallMB: hotdesk.RAMInstallMB,
				RAMFinalMB:   hotdesk.RAMFinalMB,
				VCPUs:        hotdesk.VCPUs,
				DiskGB:       hotdesk.DiskGB,
			}
			return node, []libvirt.NetworkOpt{{Network: "main", Mac: node.MAC}}, nil, nil
		}
	}

	return config.NodeDetail{}, nil, nil, fmt.Errorf("node %q is not part of the configured topology", name)
}
