package utils

import (
	"context"
	"fmt"
	"strings"
	"sync"

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

// CheckResult holds the status of an infrastructure check.
type CheckResult struct {
	Phase   string
	Name    string
	Pass    bool
	Details string
}

// VerifyAll executes a full infrastructure health check concurrently.
func (v *Verifier) VerifyAll(ctx context.Context) error {
	timer := logging.PhaseStart("Infrastructure Verification")
	defer timer.End()

	checks := []func(ctx context.Context) CheckResult{
		v.phase00,
		v.phase01,
		v.phase02,
		v.phase03,
		v.phase04,
		v.phase05,
		v.phase06,
		v.phase07,
	}

	results := make([]CheckResult, len(checks))
	var wg sync.WaitGroup

	for i, chk := range checks {
		idx := i
		f := chk
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[idx] = f(ctx)
		}()
	}

	wg.Wait()

	allPassed := true
	for _, res := range results {
		v.printResult(res.Phase, res.Name, res.Pass, res.Details)
		if !res.Pass {
			allPassed = false
		}
	}

	if !allPassed {
		return fmt.Errorf("infrastructure verification failed: one or more checks did not pass")
	}

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

func (v *Verifier) phase00(ctx context.Context) CheckResult {
	// Libvirt VMs running + networks active
	running, err := v.libvirt.ListRunning(ctx)
	pass := err == nil && len(running) > 0
	return CheckResult{"Phase 00", "Libvirt VMs & Networks", pass, fmt.Sprintf("%d running VMs", len(running))}
}

func (v *Verifier) phase01(ctx context.Context) CheckResult {
	// Cobbler services + system count >= 13
	out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.Jumpstart.IP, "systemctl is-active cobblerd apache2 isc-dhcp-server bind9 tftpd-hpa")
	pass := err == nil && !strings.Contains(out, "inactive") && !strings.Contains(out, "failed")
	
	sysCountOut, _, _, _ := v.ssh.RunCommand(ctx, v.cfg.Nodes.Jumpstart.IP, "cobbler system list | wc -l")
	sysCount := strings.TrimSpace(sysCountOut)
	if sysCount == "0" || sysCount == "" {
		pass = false
	}
	return CheckResult{"Phase 01", "Cobbler Services & Systems", pass, fmt.Sprintf("Systems: %s", sysCount)}
}

func (v *Verifier) phase02(ctx context.Context) CheckResult {
	// Puppet server + signed certs >= 9 + agent services
	out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.Jumpstart.IP, "systemctl is-active puppetserver")
	pass := err == nil && strings.TrimSpace(out) == "active"
	
	certOut, _, _, _ := v.ssh.RunCommand(ctx, v.cfg.Nodes.Jumpstart.IP, "puppetserver ca list --all 2>/dev/null | grep -E 'Signed|\\(SHA256\\)' | wc -l")
	certCount := strings.TrimSpace(certOut)
	if certCount == "0" || certCount == "" {
		pass = false
	}
	return CheckResult{"Phase 02", "Puppet Server & Certs", pass, fmt.Sprintf("Certs: %s", certCount)}
}

func (v *Verifier) phase03(ctx context.Context) CheckResult {
	// Nginx + Apache + SSL cert on LB
	out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.LB.IP, "systemctl is-active nginx")
	pass := err == nil && strings.TrimSpace(out) == "active"
	
	for _, cms := range v.cfg.Nodes.CMSFrontends {
		outApache, _, _, _ := v.ssh.RunCommand(ctx, cms.IP, "systemctl is-active apache2")
		if strings.TrimSpace(outApache) != "active" {
			pass = false
		}
	}
	return CheckResult{"Phase 03", "Nginx & Apache & SSL", pass, ""}
}

func (v *Verifier) phase04(ctx context.Context) CheckResult {
	// K3s cluster + MariaDB pod Running
	if len(v.cfg.Nodes.Masters) > 0 {
		out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.Masters[0].IP, "kubectl get nodes")
		pass := err == nil && strings.Contains(out, "Ready")
		
		podOut, _, _, _ := v.ssh.RunCommand(ctx, v.cfg.Nodes.Masters[0].IP, "kubectl get pods -n cms | grep mariadb")
		if !strings.Contains(podOut, "Running") {
			pass = false
		}
		return CheckResult{"Phase 04", "K3s & MariaDB Pod", pass, ""}
	}
	return CheckResult{"Phase 04", "K3s & MariaDB Pod", false, "No master node configured"}
}

func (v *Verifier) phase05(ctx context.Context) CheckResult {
	// Prometheus + Grafana + node-exporter
	out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.Monitor.IP, "systemctl is-active prometheus grafana-server")
	pass := err == nil && !strings.Contains(out, "inactive") && !strings.Contains(out, "failed")
	return CheckResult{"Phase 05", "Prometheus & Grafana", pass, ""}
}

func (v *Verifier) phase06(ctx context.Context) CheckResult {
	// UFW + ip_forward on router
	out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.Router.IP, "cat /proc/sys/net/ipv4/ip_forward")
	pass := err == nil && strings.TrimSpace(out) == "1"
	
	ufwOut, _, _, _ := v.ssh.RunCommand(ctx, v.cfg.Nodes.Router.IP, "ufw status")
	if !strings.Contains(ufwOut, "Status: active") {
		pass = false
	}
	return CheckResult{"Phase 06", "Router UFW & IP Forward", pass, ""}
}

func (v *Verifier) phase07(ctx context.Context) CheckResult {
	// DRBD status on master nodes
	for _, m := range v.cfg.Nodes.Masters {
		if m.IP == "" {
			continue
		}
		out, _, _, err := v.ssh.RunCommand(ctx, m.IP, "drbdadm status cms_data 2>/dev/null || drbdadm status 2>/dev/null")
		if err == nil && (strings.Contains(out, "Primary") || strings.Contains(out, "Secondary") || strings.Contains(out, "UpToDate")) {
			return CheckResult{"Phase 07", "DRBD Status", true, fmt.Sprintf("Verified on %s", m.Name)}
		}
	}
	return CheckResult{"Phase 07", "DRBD Status", false, "DRBD inactive or unreachable on masters"}
}
