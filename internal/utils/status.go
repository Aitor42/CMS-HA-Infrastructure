package utils

import (
	"context"
	"fmt"
	"net"
	"strings"
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

	allHealthy := true

	for _, node := range nodes {
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

		if state != "running" || sshStatus != "OK" {
			allHealthy = false
		}

		fmt.Printf("%-20s %-15s %-10s %-10s\n", node.Name, node.IP, state, sshStatus)
	}

	if !allHealthy {
		return fmt.Errorf("one or more VMs are not healthy or unreachable via SSH")
	}

	return nil
}
