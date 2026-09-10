package utils

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// CheckSSH checks SSH connectivity for all VMs and prints a table.
func CheckSSH(ctx context.Context, cfg *config.Config, s *ssh.Pool, l *libvirt.Client) error {
	timer := logging.PhaseStart("Check SSH Status")
	defer timer.End()

	nodes := cfg.AllNodes()

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
				conn, err := net.DialTimeout("tcp", net.JoinHostPort(node.IP, "22"), 2*time.Second)
				if err == nil {
					conn.Close()
					_, _, _, err := s.RunCommand(ctx, node.IP, "echo ok")
					if err == nil {
						sshStatus = "OK"
					}
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
