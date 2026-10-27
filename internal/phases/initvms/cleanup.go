package initvms

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
)

type CleanupOptions struct {
	VMsOnly      bool
	NetworksOnly bool
}

// Cleanup destroys all VMs and/or virtual networks managed by the infrastructure.
func Cleanup(ctx context.Context, cfg *config.Config, lv *libvirt.Client, opts CleanupOptions) error {
	p := &Phase{cfg: cfg, lv: lv}
	return p.cleanupWithOptions(ctx, opts)
}

// cleanup destroys all VMs and networks.
func (p *Phase) cleanup(ctx context.Context) error {
	return p.cleanupWithOptions(ctx, CleanupOptions{})
}

func (p *Phase) cleanupWithOptions(ctx context.Context, opts CleanupOptions) error {
	if opts.VMsOnly && opts.NetworksOnly {
		return fmt.Errorf("VMsOnly and NetworksOnly are mutually exclusive")
	}

	timer := logging.PhaseStart("Cleanup VMs and Networks")
	defer timer.End()

	var errs []error

	if !opts.NetworksOnly {
		logging.Info("Destroying all VMs and disk images...")
		allNodes := p.cfg.AllNodes()
		// One `virsh list --all` snapshot: a libvirt query failure is not
		// evidence the domains are absent, so abort VM destruction rather than
		// risk deleting the disks of live VMs.
		states, err := p.lv.DomainStates(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("list domains: %w", err))
		} else {
			for _, node := range allNodes {
				if node.Name == "" {
					continue
				}
				state, exists := states[node.Name]
				if err := p.destroyVM(ctx, node.Name, state, exists); err != nil {
					errs = append(errs, fmt.Errorf("remove VM %s: %w", node.Name, err))
				}
			}
		}
	}

	if !opts.VMsOnly {
		logging.Info("Destroying virtual networks...")
		for _, net := range []string{"internal", "main"} {
			exists, err := p.lv.NetExistsChecked(ctx, net)
			if err != nil {
				errs = append(errs, fmt.Errorf("check network %s: %w", net, err))
				continue
			}
			if !exists {
				continue
			}
			_ = p.lv.NetDestroy(ctx, net) // An inactive network may already be stopped.
			if err := p.lv.NetUndefine(ctx, net); err != nil {
				errs = append(errs, fmt.Errorf("undefine network %s: %w", net, err))
			}
		}
	}

	if err := errors.Join(errs...); err != nil {
		return err
	}
	logging.Success("Cleanup completed")
	return nil
}

// destroyVM safely destroys and undefines a VM if it exists, and cleans up its
// disks. exists/state come from a single caller-side domain snapshot.
func (p *Phase) destroyVM(ctx context.Context, name, state string, exists bool) error {
	if exists {
		logging.Info("Removing existing VM: %s", name)
		if state != "shut off" && state != "crashed" {
			if err := p.lv.Destroy(ctx, name); err != nil {
				return fmt.Errorf("stop domain: %w", err)
			}
		}
		if err := p.lv.Undefine(ctx, name); err != nil {
			return fmt.Errorf("undefine domain: %w", err)
		}
	}
	for _, disk := range []string{
		filepath.Join(p.cfg.VM.StorageDir, name+".qcow2"),
		filepath.Join(p.cfg.VM.StorageDir, name+"-drbd.qcow2"),
		filepath.Join(p.cfg.VM.StorageDir, name+"-seed.iso"),
	} {
		if err := os.Remove(disk); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove disk %s: %w", disk, err)
		}
	}
	if err := os.RemoveAll(filepath.Join(p.cfg.VM.StorageDir, "cloud-init-"+name)); err != nil {
		return fmt.Errorf("remove cloud-init dir for %s: %w", name, err)
	}
	if name == p.cfg.Nodes.Jumpstart.Name {
		seed := filepath.Join(p.cfg.VM.StorageDir, "seed.iso")
		if err := os.Remove(seed); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove seed ISO: %w", err)
		}
		for _, dir := range []string{"autoinstall", "cloud-init"} {
			if err := os.RemoveAll(filepath.Join(p.cfg.VM.StorageDir, dir)); err != nil {
				return fmt.Errorf("remove %s dir: %w", dir, err)
			}
		}
	}
	return nil
}

// ensureSSHKey returns the public key for the given private key path, generating it if needed.
func (p *Phase) ensureSSHKey(keyPath string) (string, error) {
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		logging.Info("Generating SSH key at %s...", keyPath)
		if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
			return "", fmt.Errorf("failed to create SSH dir: %w", err)
		}
		cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", filepath.Clean(keyPath)) // #nosec G204 -- keyPath is within configured safe storage directory
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("ssh-keygen failed: %v: %s", err, out)
		}
	}

	cleanedKeyPath := filepath.Clean(keyPath + ".pub")
	pubKeyBytes, err := os.ReadFile(cleanedKeyPath)
	if err != nil {
		return "", fmt.Errorf("failed to read public key: %w", err)
	}
	return strings.TrimSpace(string(pubKeyBytes)), nil
}

// findUbuntuISO searches for an Ubuntu 24.04 ISO in the VM storage directory.
