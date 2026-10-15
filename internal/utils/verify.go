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
	// At least one libvirt VM is running.
	running, err := v.libvirt.ListRunning(ctx)
	pass := err == nil && len(running) > 0
	return CheckResult{"Phase 00", "Libvirt VMs & Networks", pass, fmt.Sprintf("%d running VMs", len(running))}
}

func (v *Verifier) phase01(ctx context.Context) CheckResult {
	// Cobbler services + system count in single SSH session.
	// bind9 and named are aliases for the same resolver on Ubuntu 24.04;
	// checking both tolerates whichever alias systemd registers.
	cmd := `for unit in cobblerd apache2 isc-dhcp-server bind9 named tftpd-hpa; do systemctl is-active $unit 2>/dev/null; done | sort -u; echo "---CMS_DELIM---"; cobbler system list | wc -l`
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
	pass := err == nil && serviceStatesActive(servicesOut)
	if sysCount == "0" || sysCount == "" {
		pass = false
	}
	details := fmt.Sprintf("Systems: %s", sysCount)
	return CheckResult{"Phase 01", "Cobbler Services & Systems", pass, details}
}

// serviceStatesActive checks every `systemctl is-active` state token; a
// multi-unit invocation prints one state per line and the SSH output carries
// one state per line (sorted), with no extra noise.
func serviceStatesActive(out string) bool {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return false
	}
	for _, f := range fields {
		if f != "active" {
			return false
		}
	}
	return true
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
	// Nginx/keepalived + CA-issued cert on the LBs; apache2 + uploads mount on
	// the CMS frontends.
	var lbIPs []string
	for _, lb := range v.cfg.Nodes.LBs {
		if lb.IP != "" {
			lbIPs = append(lbIPs, lb.IP)
		}
	}
	pass := true
	var issues []string

	// The load balancers must serve a certificate issued by the internal CA
	// (not the self-signed bootstrap) that is valid for at least
	// pki.cert_min_valid_days. `-checkend` makes openssl exit non-zero when the
	// certificate expires before the given number of seconds.
	minDays := v.cfg.LBTLSCertMinValidDays()
	caName := v.cfg.PKICommonName()
	lbCmd := fmt.Sprintf("systemctl is-active nginx && systemctl is-active keepalived && "+
		"openssl x509 -in /etc/nginx/ssl/cms.crt -noout -issuer -checkend %d", minDays*86400)

	resLB := v.ssh.RunParallel(ctx, lbIPs, lbCmd)
	for _, r := range resLB {
		if r.Err != nil || r.ExitCode != 0 {
			pass = false
			issues = append(issues, fmt.Sprintf("%s: services or certificate check failed", r.Host))
			continue
		}
		if !strings.Contains(r.Output, caName) {
			pass = false
			issues = append(issues, fmt.Sprintf("%s: certificate not issued by %q", r.Host, caName))
		}
	}

	var cmsIPs []string
	for _, cms := range v.cfg.Nodes.CMSFrontends {
		if cms.IP != "" {
			cmsIPs = append(cmsIPs, cms.IP)
		}
	}
	resApache := v.ssh.RunParallel(ctx, cmsIPs, "systemctl is-active apache2 && mountpoint -q /var/www/html/wp-content/uploads")
	for _, r := range resApache {
		if r.Err != nil || r.ExitCode != 0 {
			pass = false
			issues = append(issues, fmt.Sprintf("%s: apache2 or uploads mount failed", r.Host))
		}
	}

	details := ""
	if len(issues) > 0 {
		details = strings.Join(issues, "; ")
	} else if len(lbIPs) > 0 {
		details = fmt.Sprintf("certificate issued by %q, valid > %d days", caName, minDays)
	}
	return CheckResult{"Phase 03", "Nginx & Apache & SSL", pass, details}
}

func (v *Verifier) phase04(ctx context.Context) CheckResult {
	// K3s cluster + MariaDB pod Running in single SSH session
	if len(v.cfg.Nodes.Masters) > 0 {
		cmd := `kubectl get nodes; echo "---CMS_DELIM---"; kubectl get pods -n cms | grep mariadb; echo "---CMS_DELIM---"; kubectl -n kube-system get pods -o name 2>/dev/null | grep -c 'pod/etcd-' || true`
		out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.Masters[0].IP, cmd)
		parts := strings.Split(out, "---CMS_DELIM---")
		nodesOut := ""
		podOut := ""
		etcdOut := ""
		if len(parts) >= 2 {
			nodesOut = parts[0]
			podOut = parts[1]
		} else {
			nodesOut = out
		}
		if len(parts) >= 3 {
			etcdOut = strings.TrimSpace(parts[2])
		}
		pass := err == nil && strings.Contains(nodesOut, "Ready") && strings.Contains(podOut, "Running")
		// etcd must report one member per configured master, otherwise the
		// cluster has no real quorum even though the nodes look Ready.
		if want := fmt.Sprintf("%d", len(v.cfg.Nodes.Masters)); etcdOut != want {
			pass = false
		}
		return CheckResult{"Phase 04", "K3s & MariaDB Pod", pass, fmt.Sprintf("etcd members: %s", etcdOut)}
	}
	return CheckResult{"Phase 04", "K3s & MariaDB Pod", false, "No master node configured"}
}

func (v *Verifier) phase05(ctx context.Context) CheckResult {
	// Prometheus + Grafana: every listed unit must be active.
	out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.Monitor.IP, "systemctl is-active prometheus grafana-server")
	pass := err == nil && serviceStatesActive(out)
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
