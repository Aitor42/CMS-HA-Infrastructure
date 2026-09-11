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
	// Cobbler services + system count in single SSH session
	cmd := `systemctl is-active cobblerd apache2 isc-dhcp-server bind9 tftpd-hpa; echo "---CMS_DELIM---"; cobbler system list | wc -l`
	out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.Jumpstart.IP, cmd)
	parts := strings.Split(out, "---CMS_DELIM---")
	servicesOut := ""
	sysCount := ""
	if len(parts) >= 2 {
		servicesOut = parts[0]
		sysCount = strings.TrimSpace(parts[1])
	} else {
		servicesOut = out
	}
	pass := err == nil && !strings.Contains(servicesOut, "inactive") && !strings.Contains(servicesOut, "failed")
	if sysCount == "0" || sysCount == "" {
		pass = false
	}
	return CheckResult{"Phase 01", "Cobbler Services & Systems", pass, fmt.Sprintf("Systems: %s", sysCount)}
}

func (v *Verifier) phase02(ctx context.Context) CheckResult {
	// Puppet server + signed certs in single SSH session
	cmd := `systemctl is-active puppetserver; echo "---CMS_DELIM---"; puppetserver ca list --all 2>/dev/null | grep -E 'Signed|\(SHA256\)' | wc -l`
	out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.Jumpstart.IP, cmd)
	parts := strings.Split(out, "---CMS_DELIM---")
	svcOut := ""
	certCount := ""
	if len(parts) >= 2 {
		svcOut = strings.TrimSpace(parts[0])
		certCount = strings.TrimSpace(parts[1])
	} else {
		svcOut = strings.TrimSpace(out)
	}
	pass := err == nil && svcOut == "active"
	if certCount == "0" || certCount == "" {
		pass = false
	}
	return CheckResult{"Phase 02", "Puppet Server & Certs", pass, fmt.Sprintf("Certs: %s", certCount)}
}

func (v *Verifier) phase03(ctx context.Context) CheckResult {
	// Nginx + Apache + SSL cert on LB
	out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.LB.IP, "systemctl is-active nginx")
	pass := err == nil && strings.TrimSpace(out) == "active"
	
	var cmsIPs []string
	for _, cms := range v.cfg.Nodes.CMSFrontends {
		if cms.IP != "" {
			cmsIPs = append(cmsIPs, cms.IP)
		}
	}
	resApache := v.ssh.RunParallel(ctx, cmsIPs, "systemctl is-active apache2")
	for _, r := range resApache {
		if r.Err != nil || strings.TrimSpace(r.Output) != "active" {
			pass = false
		}
	}
	return CheckResult{"Phase 03", "Nginx & Apache & SSL", pass, ""}
}

func (v *Verifier) phase04(ctx context.Context) CheckResult {
	// K3s cluster + MariaDB pod Running in single SSH session
	if len(v.cfg.Nodes.Masters) > 0 {
		cmd := `kubectl get nodes; echo "---CMS_DELIM---"; kubectl get pods -n cms | grep mariadb`
		out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.Masters[0].IP, cmd)
		parts := strings.Split(out, "---CMS_DELIM---")
		nodesOut := ""
		podOut := ""
		if len(parts) >= 2 {
			nodesOut = parts[0]
			podOut = parts[1]
		} else {
			nodesOut = out
		}
		pass := err == nil && strings.Contains(nodesOut, "Ready") && strings.Contains(podOut, "Running")
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
	// UFW + ip_forward on router in single SSH session
	cmd := `cat /proc/sys/net/ipv4/ip_forward; echo "---CMS_DELIM---"; ufw status`
	out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.Router.IP, cmd)
	parts := strings.Split(out, "---CMS_DELIM---")
	fwdOut := ""
	ufwOut := ""
	if len(parts) >= 2 {
		fwdOut = strings.TrimSpace(parts[0])
		ufwOut = parts[1]
	} else {
		fwdOut = strings.TrimSpace(out)
	}
	pass := err == nil && fwdOut == "1" && strings.Contains(ufwOut, "Status: active")
	return CheckResult{"Phase 06", "Router UFW & IP Forward", pass, ""}
}

func (v *Verifier) phase07(ctx context.Context) CheckResult {
	// DRBD status on master nodes concurrently
	var masterIPs []string
	for _, m := range v.cfg.Nodes.Masters {
		if m.IP != "" {
			masterIPs = append(masterIPs, m.IP)
		}
	}
	if len(masterIPs) == 0 {
		return CheckResult{"Phase 07", "DRBD Status", false, "No master nodes configured"}
	}
	res := v.ssh.RunParallel(ctx, masterIPs, "drbdadm status cms_data 2>/dev/null || drbdadm status 2>/dev/null")
	for _, r := range res {
		if r.Err == nil && (strings.Contains(r.Output, "Primary") || strings.Contains(r.Output, "Secondary") || strings.Contains(r.Output, "UpToDate")) {
			return CheckResult{"Phase 07", "DRBD Status", true, fmt.Sprintf("Verified on %s", r.Host)}
		}
	}
	return CheckResult{"Phase 07", "DRBD Status", false, "DRBD inactive or unreachable on masters"}
}
