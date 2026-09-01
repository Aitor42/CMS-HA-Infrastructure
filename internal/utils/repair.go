package utils

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// SyncClocks sets date on all VMs to host time and restarts timesyncd.
func SyncClocks(ctx context.Context, cfg *config.Config, s *ssh.Pool) error {
	timer := logging.PhaseStart("Sync Clocks")
	defer timer.End()

	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	cmd := fmt.Sprintf("date -s '%s' && systemctl restart systemd-timesyncd", now)

	ips := cfg.AllNodeIPs()

	for _, ip := range ips {
		if ip == "" {
			continue
		}
		_, _, _, err := s.RunCommand(ctx, ip, cmd)
		if err != nil {
			logging.Warn("Failed to sync clock on %s: %v", ip, err)
		} else {
			logging.Info("Synced clock on %s", ip)
		}
	}

	return nil
}

// RepairK8s syncs clocks, cleans k3s processes, restarts nodes, and verifies.
func RepairK8s(ctx context.Context, cfg *config.Config, s *ssh.Pool) error {
	timer := logging.PhaseStart("Repair Kubernetes")
	defer timer.End()

	SyncClocks(ctx, cfg, s)

	var nodes []string
	for _, n := range cfg.Nodes.Masters {
		nodes = append(nodes, n.IP)
	}
	for _, n := range cfg.Nodes.Workers {
		nodes = append(nodes, n.IP)
	}

	// Stop k3s
	for _, ip := range nodes {
		if ip == "" {
			continue
		}
		s.RunCommand(ctx, ip, "systemctl stop k3s k3s-agent || true")
		s.RunCommand(ctx, ip, "killall containerd || true")
	}

	if len(cfg.Nodes.Masters) >= 2 {
		// Restart master1
		logging.Info("Restarting Master 1...")
		s.RunCommand(ctx, cfg.Nodes.Masters[0].IP, "systemctl start k3s")
		time.Sleep(15 * time.Second)

		// Restart master2
		logging.Info("Restarting Master 2...")
		s.RunCommand(ctx, cfg.Nodes.Masters[1].IP, "systemctl start k3s")
		time.Sleep(10 * time.Second)
	}

	// Restart workers
	logging.Info("Restarting Workers...")
	for _, w := range cfg.Nodes.Workers {
		s.RunCommand(ctx, w.IP, "systemctl start k3s-agent")
	}
	time.Sleep(20 * time.Second)

	// Verify
	if len(cfg.Nodes.Masters) > 0 {
		out, _, _, err := s.RunCommand(ctx, cfg.Nodes.Masters[0].IP, "kubectl get nodes")
		if err != nil {
			logging.Error("Failed to verify nodes: %v", err)
			return err
		}
		if strings.Contains(out, "NotReady") {
			logging.Warn("Some nodes are still NotReady")
		} else {
			logging.Success("All nodes are Ready")
		}
	}

	return nil
}
