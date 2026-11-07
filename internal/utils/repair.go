package utils

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// SyncClocks sets date on all VMs to host time and restarts chrony / timesyncd.
func SyncClocks(ctx context.Context, cfg *config.Config, s ssh.Executor) error {
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
	failed := 0
	for _, res := range results {
		if res.Err != nil {
			logging.Warn("Failed to sync clock on %s: %v", res.Host, res.Err)
			failed++
		} else {
			logging.Info("Synced clock on %s", res.Host)
		}
	}
	// Report a total failure instead of silently succeeding: a repair command
	// that touched nothing must not look like it worked.
	if len(validIPs) > 0 && failed == len(validIPs) {
		return fmt.Errorf("failed to sync the clock on all %d host(s)", failed)
	}

	return nil
}

// RepairK8s syncs clocks, cleans k3s processes, restarts nodes, and verifies.
func RepairK8s(ctx context.Context, cfg *config.Config, s ssh.Executor) error {
	timer := logging.PhaseStart("Repair Kubernetes")
	defer timer.End()

	SyncClocks(ctx, cfg, s)
	if err := ctx.Err(); err != nil {
		return err
	}

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
	for _, res := range s.RunParallel(ctx, clusterIPs, cleanupCmd) {
		if res.Err != nil {
			logging.Warn("k3s cleanup issue on %s (exit %d): %v %s", res.Host, res.ExitCode, res.Err, strings.TrimSpace(res.Output))
		}
	}

	var masterIPs []string
	for _, m := range cfg.Nodes.Masters {
		if m.IP != "" {
			masterIPs = append(masterIPs, m.IP)
		}
	}
	if len(masterIPs) > 0 {
		logging.Info("Starting K3s on master nodes in parallel to reach quorum...")
		for _, res := range s.RunParallel(ctx, masterIPs, "systemctl start k3s") {
			if res.Err != nil || res.ExitCode != 0 {
				logging.Warn("failed to start k3s on %s (exit %d): %v %s", res.Host, res.ExitCode, res.Err, strings.TrimSpace(res.Output))
			}
		}
		if !retry.Wait(ctx, 90*time.Second, 3*time.Second, func() bool {
			out, _, code, err := s.RunCommand(ctx, masterIPs[0], "kubectl get nodes --no-headers")
			return err == nil && code == 0 && strings.TrimSpace(out) != ""
		}) {
			logging.Warn("K3s API server did not become ready within 90s")
		}
	}

	var workerIPs []string
	for _, w := range cfg.Nodes.Workers {
		if w.IP != "" {
			workerIPs = append(workerIPs, w.IP)
		}
	}
	if len(workerIPs) > 0 {
		logging.Info("Starting K3s agents on worker nodes...")
		for _, res := range s.RunParallel(ctx, workerIPs, "systemctl start k3s-agent") {
			if res.Err != nil || res.ExitCode != 0 {
				logging.Warn("failed to start k3s-agent on %s (exit %d): %v %s", res.Host, res.ExitCode, res.Err, strings.TrimSpace(res.Output))
			}
		}
		if len(masterIPs) > 0 && !retry.Wait(ctx, 120*time.Second, 3*time.Second, func() bool {
			out, _, code, err := s.RunCommand(ctx, masterIPs[0], "kubectl get nodes --no-headers")
			return err == nil && code == 0 && strings.TrimSpace(out) != "" && !strings.Contains(out, "NotReady")
		}) {
			logging.Warn("Not all nodes reached Ready within 120s")
		}
	}

	if len(masterIPs) > 0 {
		logging.Info("Verifying Kubernetes status...")
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
