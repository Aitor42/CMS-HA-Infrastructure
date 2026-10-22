package utils

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// WaitForAllSSH blocks until every configured node with an IP accepts an SSH
// session, or the timeout expires. It replaces retry loops made of `status ssh`
// plus a fixed sleep: the wait ends as soon as the lab is up instead of on the
// next poll boundary.
func WaitForAllSSH(ctx context.Context, cfg *config.Config, s *ssh.Pool, timeout time.Duration) error {
	var hosts []string
	seen := make(map[string]bool)
	for _, n := range cfg.AllNodes() {
		if n.Name == "" || n.IP == "" || seen[n.IP] {
			continue
		}
		seen[n.IP] = true
		hosts = append(hosts, n.IP)
	}
	if len(hosts) == 0 {
		return fmt.Errorf("no nodes with an IP address are configured")
	}

	timer := logging.PhaseStart(fmt.Sprintf("Wait for SSH on %d nodes", len(hosts)))
	defer timer.End()

	logging.Info("Waiting up to %s for SSH on every node...", timeout)
	if err := s.WaitForAllSSH(ctx, hosts, timeout); err != nil {
		return err
	}
	logging.Success("SSH is available on all %d nodes", len(hosts))
	return nil
}

// CheckSSH checks SSH connectivity for all VMs and prints a table.
func CheckSSH(ctx context.Context, cfg *config.Config, s *ssh.Pool, l *libvirt.Client) error {
	timer := logging.PhaseStart("Check SSH Status")
	defer timer.End()

	// A partial config can leave placeholder nodes with no name; there is
	// nothing to inspect for them, so skip instead of printing blank rows.
	var nodes []config.NodeSpec
	for _, n := range cfg.AllNodes() {
		if n.Name != "" {
			nodes = append(nodes, n)
		}
	}

	fmt.Printf("%-20s %-15s %-10s %-10s\n", "VM NAME", "IP", "STATE", "SSH")
	fmt.Println(strings.Repeat("-", 60))

	type nodeStatus struct {
		name      string
		ip        string
		state     string
		sshStatus string
		healthy   bool
	}

	results := make([]nodeStatus, len(nodes))
	var wg sync.WaitGroup

	for i, n := range nodes {
		idx := i
		node := n
		wg.Add(1)
		go func() {
			defer wg.Done()
			state, err := l.DomainState(ctx, node.Name)
			if err != nil {
				state = "unknown"
			}

			sshStatus := "FAIL"
			if state == "running" && node.IP != "" {
				// A single SSH command proves both reachability and
				// authentication, so no separate TCP pre-dial is needed. The
				// bounded per-node budget stops a hung SSH command from
				// blocking the whole status sweep forever.
				probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				_, _, _, err := s.RunCommand(probeCtx, node.IP, "echo ok")
				cancel()
				if err == nil {
					sshStatus = "OK"
				}
			}

			results[idx] = nodeStatus{
				name:      node.Name,
				ip:        node.IP,
				state:     state,
				sshStatus: sshStatus,
				healthy:   state == "running" && sshStatus == "OK",
			}
		}()
	}

	wg.Wait()

	allHealthy := true
	for _, res := range results {
		if !res.healthy {
			allHealthy = false
		}
		fmt.Printf("%-20s %-15s %-10s %-10s\n", res.name, res.ip, res.state, res.sshStatus)
	}

	if !allHealthy {
		return fmt.Errorf("one or more VMs are not healthy or unreachable via SSH")
	}

	return nil
}
