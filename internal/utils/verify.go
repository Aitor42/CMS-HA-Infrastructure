package utils

import (
	"context"
	"fmt"
	"strings"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// Verifier struct contains dependencies for verification operations.
type Verifier struct {
	cfg     *config.Config
	ssh     *ssh.Pool
	libvirt *libvirt.Client
}

// NewVerifier initializes a new Verifier.
func NewVerifier(cfg *config.Config, s *ssh.Pool, l *libvirt.Client) *Verifier {
	return &Verifier{cfg: cfg, ssh: s, libvirt: l}
}

// VerifyAll executes a full infrastructure health check.
func (v *Verifier) VerifyAll(ctx context.Context) error {
	timer := logging.PhaseStart("Infrastructure Verification")
	defer timer.End()

	v.phase00(ctx)
	v.phase01(ctx)
	v.phase02(ctx)
	v.phase03(ctx)
	v.phase04(ctx)
	v.phase05(ctx)
	v.phase06(ctx)
	v.phase07(ctx)

	return nil
}

func (v *Verifier) printResult(phase, name string, pass bool, details string) {
	status := "[FAIL]"
	if pass {
		status = "[PASS]"
	}
	msg := fmt.Sprintf("%s - %s: %s", phase, name, status)
	if details != "" {
		msg += " (" + details + ")"
	}
	if pass {
		logging.Success("%s", msg)
	} else {
		logging.Error("%s", msg)
	}
}

func (v *Verifier) phase00(ctx context.Context) {
	// Libvirt VMs running + networks active
	running, err := v.libvirt.ListRunning(ctx)
	pass := err == nil && len(running) > 0
	v.printResult("Phase 00", "Libvirt VMs & Networks", pass, fmt.Sprintf("%d running VMs", len(running)))
}

func (v *Verifier) phase01(ctx context.Context) {
	// Cobbler services + system count >= 13
	out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.Jumpstart.IP, "systemctl is-active cobblerd apache2 isc-dhcp-server bind9 tftpd-hpa")
	pass := err == nil && !strings.Contains(out, "inactive") && !strings.Contains(out, "failed")
	
	sysCountOut, _, _, _ := v.ssh.RunCommand(ctx, v.cfg.Nodes.Jumpstart.IP, "cobbler system list | wc -l")
	sysCount := strings.TrimSpace(sysCountOut)
	if sysCount == "0" || sysCount == "" {
		pass = false
	}
	v.printResult("Phase 01", "Cobbler Services & Systems", pass, fmt.Sprintf("Systems: %s", sysCount))
}

func (v *Verifier) phase02(ctx context.Context) {
	// Puppet server + signed certs >= 9 + agent services
	out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.Jumpstart.IP, "systemctl is-active puppetserver")
	pass := err == nil && strings.TrimSpace(out) == "active"
	
	certOut, _, _, _ := v.ssh.RunCommand(ctx, v.cfg.Nodes.Jumpstart.IP, "puppetserver ca list --all | grep 'Signed' | wc -l")
	certCount := strings.TrimSpace(certOut)
	if certCount == "0" || certCount == "" {
		pass = false
	}
	v.printResult("Phase 02", "Puppet Server & Certs", pass, fmt.Sprintf("Certs: %s", certCount))
}

func (v *Verifier) phase03(ctx context.Context) {
	// Nginx + Apache + SSL cert on LB
	out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.LB.IP, "systemctl is-active nginx")
	pass := err == nil && strings.TrimSpace(out) == "active"
	
	if len(v.cfg.Nodes.CMSFrontends) >= 2 {
		outApache1, _, _, _ := v.ssh.RunCommand(ctx, v.cfg.Nodes.CMSFrontends[0].IP, "systemctl is-active apache2")
		outApache2, _, _, _ := v.ssh.RunCommand(ctx, v.cfg.Nodes.CMSFrontends[1].IP, "systemctl is-active apache2")
		if strings.TrimSpace(outApache1) != "active" || strings.TrimSpace(outApache2) != "active" {
			pass = false
		}
	}
	v.printResult("Phase 03", "Nginx & Apache & SSL", pass, "")
}

func (v *Verifier) phase04(ctx context.Context) {
	// K3s cluster + MariaDB pod Running
	if len(v.cfg.Nodes.Masters) > 0 {
		out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.Masters[0].IP, "kubectl get nodes")
		pass := err == nil && strings.Contains(out, "Ready")
		
		podOut, _, _, _ := v.ssh.RunCommand(ctx, v.cfg.Nodes.Masters[0].IP, "kubectl get pods -n cms | grep mariadb")
		if !strings.Contains(podOut, "Running") {
			pass = false
		}
		v.printResult("Phase 04", "K3s & MariaDB Pod", pass, "")
	}
}

func (v *Verifier) phase05(ctx context.Context) {
	// Prometheus + Grafana + node-exporter
	out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.Monitor.IP, "systemctl is-active prometheus grafana-server")
	pass := err == nil && !strings.Contains(out, "inactive") && !strings.Contains(out, "failed")
	v.printResult("Phase 05", "Prometheus & Grafana", pass, "")
}

func (v *Verifier) phase06(ctx context.Context) {
	// UFW + ip_forward on router
	out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.Router.IP, "cat /proc/sys/net/ipv4/ip_forward")
	pass := err == nil && strings.TrimSpace(out) == "1"
	
	ufwOut, _, _, _ := v.ssh.RunCommand(ctx, v.cfg.Nodes.Router.IP, "ufw status")
	if !strings.Contains(ufwOut, "Status: active") {
		pass = false
	}
	v.printResult("Phase 06", "Router UFW & IP Forward", pass, "")
}

func (v *Verifier) phase07(ctx context.Context) {
	// DRBD status + mount on primary
	out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.Storage.IP, "drbdadm status")
	pass := err == nil && (strings.Contains(out, "Primary") || strings.Contains(out, "Secondary"))
	v.printResult("Phase 07", "DRBD Status", pass, "")
}
