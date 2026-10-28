package verify

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// Verifier holds the dependencies needed to check the deployment.
type Verifier struct {
	cfg     *config.Config
	ssh     ssh.Executor
	libvirt *libvirt.Client
}

// NewVerifier initializes a new Verifier.
func NewVerifier(cfg *config.Config, s ssh.Executor, l *libvirt.Client) *Verifier {
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
		v.phase08,
		v.phase10,
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
	running, err := v.libvirt.ListRunning(ctx)
	pass := err == nil && len(running) > 0
	return CheckResult{"Phase 00", "Libvirt VMs & Networks", pass, fmt.Sprintf("%d running VMs", len(running))}
}

func (v *Verifier) phase01(ctx context.Context) CheckResult {
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

// countReadyNodes counts the nodes reported Ready in `kubectl get nodes`
// output. "NotReady" contains the substring "Ready", so the negative form has
// to be excluded explicitly, and the header line does not match either.
func countReadyNodes(nodesOut string) int {
	ready := 0
	for _, line := range strings.Split(nodesOut, "\n") {
		if strings.Contains(line, "NotReady") || !strings.Contains(line, "Ready") {
			continue
		}
		ready++
	}
	return ready
}

func (v *Verifier) phase02(ctx context.Context) CheckResult {
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
	if len(v.cfg.Nodes.Masters) == 0 {
		return CheckResult{"Phase 04", "K3s & MariaDB Pod", false, "No master node configured"}
	}

	// Query the exact StatefulSet pod, not every pod carrying app=mariadb:
	// backup and restore CronJob pods share that label and can be Running
	// while the database itself is Pending or CrashLooping. The etcd listing is
	// parsed by name below; piping it through `wc -l` would also count a stale
	// node object left behind by an earlier join.
	cmd := `kubectl --request-timeout=10s get nodes --no-headers; echo "---CMS_DELIM---"; kubectl --request-timeout=10s get pod mariadb-0 -n cms -o jsonpath='{.status.phase}' 2>/dev/null || true; echo "---CMS_DELIM---"; kubectl --request-timeout=10s get nodes -l node-role.kubernetes.io/etcd --no-headers`

	expectedNodes := make([]config.NodeDetail, 0, len(v.cfg.Nodes.Masters)+len(v.cfg.Nodes.Workers))
	expectedNodes = append(expectedNodes, v.cfg.Nodes.Masters...)
	expectedNodes = append(expectedNodes, v.cfg.Nodes.Workers...)
	wantNodes := len(expectedNodes)
	wantEtcd := len(v.cfg.Nodes.Masters)

	readyNodes := 0
	etcdMembers := 0
	mariadbRunning := false
	problem := ""
	pass := false

	// A k3s restart - the process can be lost to the OOM killer on the small CI
	// guests - leaves the API server unreachable for a few seconds. A single
	// shot would fail the whole verification for that blip, so retry within a
	// bounded budget, mirroring the deploy phase's own etcd check.
	_ = retry.DoContext(ctx, retry.Config{MaxAttempts: 20, Interval: 3 * time.Second, MaxInterval: 6 * time.Second, Timeout: 90 * time.Second}, func(attemptCtx context.Context) error {
		out, errOut, code, err := v.ssh.RunCommand(attemptCtx, v.cfg.Nodes.Masters[0].IP, cmd)
		parts := strings.Split(out, "---CMS_DELIM---")
		nodesOut := out
		podOut := ""
		etcdOut := ""
		if len(parts) >= 2 {
			nodesOut = parts[0]
			podOut = strings.TrimSpace(parts[1])
		}
		if len(parts) >= 3 {
			etcdOut = parts[2]
		}

		ready := readyNodeNames(nodesOut)
		readyNodes = 0
		var missingNodes []string
		for _, node := range expectedNodes {
			if ready[node.Name] || ready[node.FQDN] {
				readyNodes++
			} else {
				missingNodes = append(missingNodes, node.Name)
			}
		}

		// etcd must report every configured master by name, otherwise the
		// cluster has no real quorum even though the nodes look Ready. K3s
		// keeps etcd inside the server process, so members are the nodes
		// carrying the etcd role label rather than kube-system pods. Extra
		// stale node objects are tolerated.
		members := etcdMemberNames(etcdOut)
		etcdMembers = 0
		var missingEtcd []string
		for _, node := range v.cfg.Nodes.Masters {
			if members[node.Name] || members[node.FQDN] {
				etcdMembers++
			} else {
				missingEtcd = append(missingEtcd, node.Name)
			}
		}

		mariadbRunning = podOut == "Running"
		pass = err == nil && code == 0 && wantNodes > 0 &&
			len(missingNodes) == 0 && len(missingEtcd) == 0 && mariadbRunning
		if pass {
			problem = ""
			return nil
		}

		switch {
		case err != nil:
			problem = fmt.Sprintf("ssh: %v", err)
		case code != 0:
			problem = fmt.Sprintf("kubectl failed with exit code %d", code)
		case len(missingNodes) > 0:
			problem = fmt.Sprintf("nodes not Ready: %s", strings.Join(missingNodes, ", "))
		case len(missingEtcd) > 0:
			problem = fmt.Sprintf("etcd members missing: %s", strings.Join(missingEtcd, ", "))
		case !mariadbRunning:
			problem = fmt.Sprintf("mariadb-0 is not Running (phase %q)", podOut)
		default:
			problem = "cluster check failed"
		}
		if msg := strings.TrimSpace(errOut); msg != "" {
			// kubectl repeats the same message for each call in the session.
			lines := strings.Split(msg, "\n")
			problem += ": " + strings.TrimSpace(lines[len(lines)-1])
		}
		return fmt.Errorf("%s", problem)
	})

	details := fmt.Sprintf("ready nodes: %d/%d, etcd members: %d/%d, MariaDB: %t", readyNodes, wantNodes, etcdMembers, wantEtcd, mariadbRunning)
	if !pass && problem != "" {
		details += "; " + problem
	}
	return CheckResult{"Phase 04", "K3s & MariaDB Pod", pass, details}
}

// readyNodeNames parses `kubectl get nodes --no-headers` and returns the names
// whose STATUS column starts with Ready. "NotReady" contains "Ready", so the
// prefix check is deliberate; cordoned nodes still count.
func readyNodeNames(out string) map[string]bool {
	ready := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[1], "Ready") {
			continue
		}
		ready[fields[0]] = true
	}
	return ready
}

// etcdMemberNames returns the node names carrying the etcd role label. The
// name is enough; a stale object with the label must not make a missing member
// look present, so callers compare against the configured names.
func etcdMemberNames(out string) map[string]bool {
	members := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		members[fields[0]] = true
	}
	return members
}

func (v *Verifier) phase05(ctx context.Context) CheckResult {
	out, _, _, err := v.ssh.RunCommand(ctx, v.cfg.Nodes.Monitor.IP, "systemctl is-active prometheus grafana-server")
	pass := err == nil && serviceStatesActive(out)
	return CheckResult{"Phase 05", "Prometheus & Grafana", pass, ""}
}

func (v *Verifier) phase06(ctx context.Context) CheckResult {
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
