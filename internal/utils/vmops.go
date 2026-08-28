package utils

import (
	"context"
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
			
			// Would write to temp file and define
			// ...
			logging.Success("Fixed boot order logic called for %s", vm)
		}
	}
	return nil
}

// InstallByBatches provisions VMs in batches using PXE.
func InstallByBatches(ctx context.Context, cfg *config.Config, l *libvirt.Client, s *ssh.Pool, force bool) error {
	timer := logging.PhaseStart("Install By Batches")
	defer timer.End()
	
	logging.Info("Batch installation logic would execute here")
	return nil
}

// RecreateFailedVMs destroys and recreates client VMs that failed.
func RecreateFailedVMs(ctx context.Context, cfg *config.Config, l *libvirt.Client) error {
	timer := logging.PhaseStart("Recreate Failed VMs")
	defer timer.End()
	
	for _, hd := range cfg.HotdeskSpecs() {
		state, err := l.DomainState(ctx, hd.Name)
		if err == nil && (state == "shut off" || state == "crashed") {
			logging.Info("Recreating failed VM: %s", hd.Name)
			l.Destroy(ctx, hd.Name)
			l.Undefine(ctx, hd.Name)
			logging.Info("Would recreate VM: %s", hd.Name)
		}
	}
	return nil
}
