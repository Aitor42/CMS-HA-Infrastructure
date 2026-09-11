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

// SyncClocks sets date on all VMs to host time and restarts chrony / timesyncd.
func SyncClocks(ctx context.Context, cfg *config.Config, s *ssh.Pool) error {
	timer := logging.PhaseStart("Sync Clocks")
	defer timer.End()

	now := time.Now().Unix()
	cmd := fmt.Sprintf("date -s @%d && (systemctl restart chrony 2>/dev/null && chronyc makestep 2>/dev/null || systemctl restart systemd-timesyncd 2>/dev/null || true)", now)

	var validIPs []string
	for _, ip := range cfg.AllNodeIPs() {
		if ip != "" {
			validIPs = append(validIPs, ip)
		}
	}

	results := s.RunParallel(ctx, validIPs, cmd)
	for _, res := range results {
		if res.Err != nil {
			logging.Warn("Failed to sync clock on %s: %v", res.Host, res.Err)
		} else {
			logging.Info("Synced clock on %s", res.Host)
		}
	}

	return nil
}

// RepairK8s syncs clocks, cleans k3s processes, restarts nodes, and verifies.
func RepairK8s(ctx context.Context, cfg *config.Config, s *ssh.Pool) error {
	timer := logging.PhaseStart("Repair Kubernetes")
	defer timer.End()

	SyncClocks(ctx, cfg, s)

	// Clean up stopped / hung k3s processes across all cluster nodes concurrently
	var clusterIPs []string
	for _, n := range cfg.Nodes.Masters {
		if n.IP != "" {
			clusterIPs = append(clusterIPs, n.IP)
		}
	}
	for _, n := range cfg.Nodes.Workers {
		if n.IP != "" {
			clusterIPs = append(clusterIPs, n.IP)
		}
	}

	logging.Info("Stopping and cleaning hung K3s processes across cluster nodes...")
	cleanupCmd := "systemctl stop k3s k3s-agent 2>/dev/null || true; killall -9 k3s k3s-server k3s-agent 2>/dev/null || true"
	s.RunParallel(ctx, clusterIPs, cleanupCmd)

	// Start K3s masters concurrently to establish quorum
	var masterIPs []string
	for _, m := range cfg.Nodes.Masters {
		if m.IP != "" {
			masterIPs = append(masterIPs, m.IP)
		}
	}
	if len(masterIPs) > 0 {
		logging.Info("Starting K3s on master nodes in parallel to reach quorum...")
		s.RunParallel(ctx, masterIPs, "systemctl start k3s")
		if !sleepCtx(ctx, 5*time.Second) {
			return ctx.Err()
		}
	}

	// Start K3s agents on workers concurrently
	var workerIPs []string
	for _, w := range cfg.Nodes.Workers {
		if w.IP != "" {
			workerIPs = append(workerIPs, w.IP)
		}
	}
	if len(workerIPs) > 0 {
		logging.Info("Starting K3s agents on worker nodes...")
		s.RunParallel(ctx, workerIPs, "systemctl start k3s-agent")
		if !sleepCtx(ctx, 5*time.Second) {
			return ctx.Err()
		}
	}

	// Verify
	if len(masterIPs) > 0 {
		logging.Info("Waiting for cluster stability and verifying Kubernetes status...")
		if !sleepCtx(ctx, 5*time.Second) {
			return ctx.Err()
		}
		out, _, _, err := s.RunCommand(ctx, masterIPs[0], "kubectl get nodes")
		if err != nil {
			logging.Error("Failed to verify nodes: %v", err)
			return err
		}
		if strings.Contains(out, "NotReady") {
			logging.Warn("Some nodes are still NotReady")
		} else {
			logging.Success("All nodes are Ready:\n%s", out)
		}
	}

	return nil
}
