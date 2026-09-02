package utils

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// StartAllVMs boots VMs in dependency order with stagger delay.
func StartAllVMs(ctx context.Context, cfg *config.Config, l *libvirt.Client, stagger time.Duration) error {
	timer := logging.PhaseStart("Start All VMs")
	defer timer.End()

	var order []string
	order = append(order, cfg.Nodes.Router.Name)
	order = append(order, cfg.Nodes.Jumpstart.Name)
	order = append(order, cfg.Nodes.Monitor.Name)
	for _, n := range cfg.Nodes.Masters {
		order = append(order, n.Name)
	}
	for _, n := range cfg.Nodes.Workers {
		order = append(order, n.Name)
	}
	order = append(order, cfg.Nodes.Storage.Name)
	order = append(order, cfg.Nodes.LB.Name)
	for _, n := range cfg.Nodes.CMSFrontends {
		order = append(order, n.Name)
	}
	for _, hd := range cfg.HotdeskSpecs() {
		order = append(order, hd.Name)
	}

	for _, vmName := range order {
		if vmName == "" {
			continue
		}
		exists := l.DomainExists(ctx, vmName)
		if !exists {
			logging.Warn("VM %s does not exist, skipping", vmName)
			continue
		}
		state, _ := l.DomainState(ctx, vmName)
		if state != "running" {
			logging.Info("Starting VM: %s", vmName)
			if err := l.Start(ctx, vmName); err != nil {
				logging.Error("Failed to start %s: %v", vmName, err)
			} else {
				time.Sleep(stagger)
			}
		}
	}
	return nil
}

// ShrinkVMRAM resizes all VMs to production sizing.
func ShrinkVMRAM(ctx context.Context, cfg *config.Config, l *libvirt.Client) error {
	timer := logging.PhaseStart("Shrink VM RAM")
	defer timer.End()

	vms := make(map[string]int)
	vms[cfg.Nodes.Router.Name] = cfg.Nodes.Router.RAMFinalMB
	vms[cfg.Nodes.Jumpstart.Name] = cfg.Nodes.Jumpstart.RAMFinalMB
	vms[cfg.Nodes.Monitor.Name] = cfg.Nodes.Monitor.RAMFinalMB
	
	for _, n := range cfg.Nodes.Masters {
		vms[n.Name] = n.RAMFinalMB
	}
	for _, n := range cfg.Nodes.Workers {
		vms[n.Name] = n.RAMFinalMB
	}
	vms[cfg.Nodes.Storage.Name] = cfg.Nodes.Storage.RAMFinalMB
	vms[cfg.Nodes.LB.Name] = cfg.Nodes.LB.RAMFinalMB
	for _, n := range cfg.Nodes.CMSFrontends {
		vms[n.Name] = n.RAMFinalMB
	}
	for _, hd := range cfg.HotdeskSpecs() {
		vms[hd.Name] = hd.RAMFinalMB
	}

	for vm, mem := range vms {
		if vm == "" {
			continue
		}
		exists := l.DomainExists(ctx, vm)
		if !exists {
			continue
		}
		state, _ := l.DomainState(ctx, vm)
		if state == "running" {
			logging.Warn("VM %s is running, cannot shrink RAM. Shutdown first.", vm)
			continue
		}
		err := l.SetMemory(ctx, vm, int64(mem*1024))
		if err != nil {
			logging.Error("Failed to set memory for %s: %v", vm, err)
		} else {
			logging.Success("Shrank RAM for %s to %d MB", vm, mem)
		}
	}
	return nil
}

// FixBootOrder modifies VM XML to ensure hd boots before network.
func FixBootOrder(ctx context.Context, l *libvirt.Client, vmNames []string) error {
	timer := logging.PhaseStart("Fix Boot Order")
	defer timer.End()

	for _, vm := range vmNames {
		exists := l.DomainExists(ctx, vm)
		if !exists {
			continue
		}
		
		xml, err := l.DumpXML(ctx, vm)
		if err != nil {
			logging.Error("Failed to dump XML for %s: %v", vm, err)
			continue
		}

		if strings.Contains(xml, "<boot dev='network'/>") && strings.Contains(xml, "<boot dev='hd'/>") {
			xml = strings.Replace(xml, "<boot dev='network'/>", "", 1)
			xml = strings.Replace(xml, "<boot dev='hd'/>", "<boot dev='hd'/>\n    <boot dev='network'/>", 1)
			
			tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("%s-boot.xml", vm))
			if err := os.WriteFile(tmpFile, []byte(xml), 0644); err != nil {
				logging.Error("Failed to write updated XML for %s: %v", vm, err)
				continue
			}
			defer os.Remove(tmpFile)

			if err := l.Define(ctx, tmpFile); err != nil {
				logging.Error("Failed to define XML for %s: %v", vm, err)
			} else {
				logging.Success("Fixed boot order for %s (hd before network)", vm)
			}
		}
	}
	return nil
}

// InstallByBatches provisions VMs in batches using PXE.
func InstallByBatches(ctx context.Context, cfg *config.Config, l *libvirt.Client, s *ssh.Pool, force bool) error {
	timer := logging.PhaseStart("Install By Batches")
	defer timer.End()

	type nodeInfo struct {
		name         string
		ip           string
		ramInstallMB int
		ramFinalMB   int
	}

	// Batch 1: Network & Management
	batch1 := []nodeInfo{
		{cfg.Nodes.Router.Name, cfg.Nodes.Router.IP, cfg.Nodes.Router.RAMInstallMB, cfg.Nodes.Router.RAMFinalMB},
		{cfg.Nodes.Monitor.Name, cfg.Nodes.Monitor.IP, cfg.Nodes.Monitor.RAMInstallMB, cfg.Nodes.Monitor.RAMFinalMB},
		{cfg.Nodes.Storage.Name, cfg.Nodes.Storage.IP, cfg.Nodes.Storage.RAMInstallMB, cfg.Nodes.Storage.RAMFinalMB},
	}

	// Batch 2: K8s Masters & Workers
	var batch2 []nodeInfo
	for _, m := range cfg.Nodes.Masters {
		batch2 = append(batch2, nodeInfo{m.Name, m.IP, m.RAMInstallMB, m.RAMFinalMB})
	}
	for _, w := range cfg.Nodes.Workers {
		batch2 = append(batch2, nodeInfo{w.Name, w.IP, w.RAMInstallMB, w.RAMFinalMB})
	}

	// Batch 3: Load Balancer, CMS Frontends & Hotdesks
	batch3 := []nodeInfo{
		{cfg.Nodes.LB.Name, cfg.Nodes.LB.IP, cfg.Nodes.LB.RAMInstallMB, cfg.Nodes.LB.RAMFinalMB},
	}
	for _, c := range cfg.Nodes.CMSFrontends {
		batch3 = append(batch3, nodeInfo{c.Name, c.IP, c.RAMInstallMB, c.RAMFinalMB})
	}
	for _, hd := range cfg.HotdeskSpecs() {
		batch3 = append(batch3, nodeInfo{hd.Name, hd.IP, hd.RAMInstallMB, hd.RAMFinalMB})
	}

	batches := [][]nodeInfo{batch1, batch2, batch3}
	batchNames := []string{"1: Network/Management", "2: Kubernetes & DB", "3: CMS Frontends & Workstations"}

	for i, batch := range batches {
		logging.Info("Starting Batch %s...", batchNames[i])

		// 1. Boot all VMs in this batch
		var ips []string
		for _, node := range batch {
			if l.DomainExists(ctx, node.name) {
				state, _ := l.DomainState(ctx, node.name)
				if state != "running" {
					l.Start(ctx, node.name)
				}
			}
			ips = append(ips, node.ip)
		}

		// 2. Wait for SSH across the batch
		logging.Info("Waiting for SSH on all VMs in Batch %d...", i+1)
		if err := s.WaitForAllSSH(ctx, ips, 15*time.Minute); err != nil {
			logging.Warn("Some VMs in batch %d timed out on SSH: %v", i+1, err)
		}

		// 3. Fix boot order and shrink RAM
		for _, node := range batch {
			FixBootOrder(ctx, l, []string{node.name})
			l.Shutdown(ctx, node.name)
		}

		time.Sleep(10 * time.Second)

		for _, node := range batch {
			l.Destroy(ctx, node.name)
			l.SetMemory(ctx, node.name, int64(node.ramFinalMB*1024))
			l.Start(ctx, node.name)
			logging.Success("Batch %d: %s resized to %d MB and running", i+1, node.name, node.ramFinalMB)
		}
	}

	logging.Success("All batches provisioned and resized")
	return nil
}

// RecreateFailedVMs destroys and recreates client VMs that failed.
func RecreateFailedVMs(ctx context.Context, cfg *config.Config, l *libvirt.Client) error {
	timer := logging.PhaseStart("Recreate Failed VMs")
	defer timer.End()

	for _, node := range cfg.AllNodes() {
		if node.Name == cfg.Nodes.Jumpstart.Name {
			continue
		}
		state, err := l.DomainState(ctx, node.Name)
		if err != nil || state == "shut off" || state == "crashed" {
			logging.Info("Cleaning failed VM: %s (state: %s)", node.Name, state)
			l.Destroy(ctx, node.Name)
			l.Undefine(ctx, node.Name)
			os.Remove(filepath.Join(cfg.VM.StorageDir, node.Name+".qcow2"))
			os.Remove(filepath.Join(cfg.VM.StorageDir, node.Name+"-drbd.qcow2"))
		}
	}

	logging.Success("Failed VMs cleaned up. Run 'cms-ha phase init-vms --nodes-only' to recreate.")
	return nil
}
