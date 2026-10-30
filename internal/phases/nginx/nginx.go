package nginx

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/catalog"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/shell"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

const (
	nfsExportPath = "/srv/nfs/wp-uploads"
	uploadsMount  = "/var/www/html/wp-content/uploads"
)

// Retry budgets are variables so tests can exercise the failure paths without
// waiting for the production backoff. The VIP election usually settles in a
// couple of seconds; the HTTPS probe tolerates WordPress still warming up.
var (
	vipOwnershipRetry = retry.Config{
		MaxAttempts: 30,
		Interval:    2 * time.Second,
		MaxInterval: 5 * time.Second,
		Timeout:     90 * time.Second,
	}
	httpsProbeRetry = retry.Config{
		MaxAttempts: 12,
		Interval:    5 * time.Second,
		MaxInterval: 20 * time.Second,
		Timeout:     120 * time.Second,
	}
)

// Phase implements the NGINX/WordPress setup phase.
type Phase struct {
	cfg  *config.Config
	pool ssh.Executor
}

// NewPhase creates a new Nginx Phase.
func NewPhase(cfg *config.Config, pool ssh.Executor) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

// Description returns the phase description.
func (p *Phase) Description() string { return catalog.Nginx.Description }

// Name returns the phase name.
func (p *Phase) Name() string { return catalog.Nginx.Name }

// Run converges the CMS frontends and verifies the load balancers, frontends,
// NFS export and HTTPS path.
func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()

	// The Puppet phase (04) already converged every node and fails the deploy
	// if one does not, so re-running it here would only re-apply an identical
	// catalog. This phase checks the preconditions and converges only the
	// frontends, whose catalog depends on state that exists by now (the
	// database from the Kubernetes phase, the NFS export from the storage
	// node).
	var lbIPs []string
	for _, lb := range p.cfg.Nodes.LBs {
		if lb.IP == "" {
			continue
		}
		lbIPs = append(lbIPs, lb.IP)
	}

	// Sequential on purpose: the catalog installs WordPress (apt + unpack), and
	// two frontends converging at the same time was the original install race.
	logging.Info("Converging CMS frontends sequentially (database and uploads now exist)...")
	for _, cms := range p.cfg.Nodes.CMSFrontends {
		if cms.IP == "" {
			continue
		}
		logging.Info("Converging CMS frontend %s (%s)...", cms.Name, cms.IP)
		if _, _, err := phases.RunPuppetAgentCmd(ctx, p.pool, cms.IP, cms.Name, phases.PuppetAgentCommandWithService("")); err != nil {
			return err
		}
	}

	logging.Info("Verifying Nginx and keepalived on LB nodes...")
	resLB := p.pool.RunParallel(ctx, lbIPs, withDiagnostics("nginx -t && systemctl is-active nginx && systemctl is-active keepalived"))
	for _, r := range resLB {
		if r.Err != nil || r.ExitCode != 0 {
			return fmt.Errorf("nginx/keepalived verification failed on %s (exit %d): %v\n%s",
				r.Host, r.ExitCode, r.Err, strings.TrimSpace(r.Output))
		}
	}

	logging.Info("Verifying Apache configuration on CMS nodes in parallel...")
	var cmsIPs []string
	for _, cms := range p.cfg.Nodes.CMSFrontends {
		if cms.IP != "" {
			cmsIPs = append(cmsIPs, cms.IP)
		}
	}
	resApache := p.pool.RunParallel(ctx, cmsIPs, withDiagnostics("apache2ctl -t && systemctl is-active apache2"))
	for _, r := range resApache {
		if r.Err != nil || r.ExitCode != 0 {
			return fmt.Errorf("apache verification failed on %s (exit %d): %v\n%s",
				r.Host, r.ExitCode, r.Err, strings.TrimSpace(r.Output))
		}
	}

	if p.cfg.Nodes.Storage.IP != "" {
		logging.Info("Verifying NFS server export on Storage node...")
		// `exportfs -s` prints the effective options. Grepping only the path
		// accepted an export with root_squash (or any other option set): the
		// frontends need all_squash with uid/gid 33 so both the root and the
		// www-data writers map to the directory owner.
		exportCmd := withDiagnostics(fmt.Sprintf(
			"systemctl is-active nfs-kernel-server && "+
				"exportfs -s | grep -F -- %s | grep -q all_squash && "+
				"exportfs -s | grep -F -- %s | grep -q anonuid=33 && "+
				"exportfs -s | grep -F -- %s | grep -q anongid=33",
			shell.Quote(nfsExportPath), shell.Quote(nfsExportPath), shell.Quote(nfsExportPath)))
		if out, stderr, code, err := p.pool.RunCommand(ctx, p.cfg.Nodes.Storage.IP, exportCmd); err != nil || code != 0 {
			return fmt.Errorf("storage node %s is not exporting %s with all_squash,anonuid=33,anongid=33 (exit %d): %v %s%s",
				p.cfg.Nodes.Storage.Name, nfsExportPath, code, err,
				strings.TrimSpace(out), strings.TrimSpace(stderr))
		}
	}

	logging.Info("Verifying shared NFS uploads mount on CMS nodes in parallel...")
	// mountpoint alone accepts any filesystem mounted at the path. Assert the
	// source and type too, otherwise a local directory left behind by a failed
	// mount would pass while the frontends silently stopped sharing uploads.
	mountCmd := withDiagnostics(fmt.Sprintf(
		"mountpoint -q %s && findmnt -n -o SOURCE,FSTYPE -T %s | awk -v src=%s '$1 == src && ($2 == \"nfs\" || $2 == \"nfs4\") { ok = 1 } END { exit !ok }'",
		shell.Quote(uploadsMount), shell.Quote(uploadsMount),
		shell.Quote(p.cfg.Nodes.Storage.IP+":"+nfsExportPath)))
	resMount := p.pool.RunParallel(ctx, cmsIPs, mountCmd)
	for _, r := range resMount {
		if r.Err != nil || r.ExitCode != 0 {
			return fmt.Errorf("nfs mount verification failed on %s (exit %d): expected %s:%s mounted at %s\n%s",
				r.Host, r.ExitCode, p.cfg.Nodes.Storage.IP, nfsExportPath, uploadsMount, strings.TrimSpace(r.Output))
		}
	}

	logging.Info("Verifying NFS uploads are writable by Apache and root...")
	uploadWriteCheck := withDiagnostics(fmt.Sprintf(
		"sudo -u www-data bash -c 'touch %s/.cms-ha-www-data-test && rm -f %s/.cms-ha-www-data-test' && "+
			"sudo -H bash -c 'touch %s/.cms-ha-root-test && rm -f %s/.cms-ha-root-test'",
		uploadsMount, uploadsMount, uploadsMount, uploadsMount))
	resUpload := p.pool.RunParallel(ctx, cmsIPs, uploadWriteCheck)
	for _, r := range resUpload {
		if r.Err != nil || r.ExitCode != 0 {
			return fmt.Errorf("nfs upload write verification failed on %s (exit %d): %v\n%s",
				r.Host, r.ExitCode, r.Err, strings.TrimSpace(r.Output))
		}
	}

	logging.Info("Verifying exactly one load balancer holds the VIP %s...", p.cfg.LBVIP())
	if err := p.verifyVIPOwnership(ctx, lbIPs); err != nil {
		return err
	}

	logging.Info("Testing HTTPS reachability on every Load Balancer...")
	if err := p.verifyHTTPS(ctx, lbIPs); err != nil {
		return err
	}

	logging.Success("NGINX & WordPress Setup completed successfully.")
	return nil
}

// withDiagnostics redirects the command's stderr to stdout. RunParallel only
// keeps stdout, so without this a failed `nginx -t` reported just an exit code
// and the actual reason (bad directive, missing certificate) was lost.
func withDiagnostics(cmd string) string {
	return "exec 2>&1; " + cmd
}

// verifyVIPOwnership requires exactly one balancer to hold the keepalived VIP.
// The phase used to check only that keepalived was active on both nodes, which
// also passes when neither node owns the VIP (no MASTER) or both do (VRRP
// split). Both cases break the router DNAT path while every service still looks
// healthy, so wait for the VRRP election to settle and then count holders.
func (p *Phase) verifyVIPOwnership(ctx context.Context, lbIPs []string) error {
	vip := p.cfg.LBVIP()
	if vip == "" {
		return fmt.Errorf("network.main.vip is empty; cannot verify keepalived ownership")
	}
	if len(lbIPs) == 0 {
		return fmt.Errorf("no load balancer with an address is configured")
	}

	cmd := withDiagnostics("ip -4 -o addr show")

	var holders []string
	err := retry.PollContext(ctx, vipOwnershipRetry, func(ctx context.Context) (bool, error) {
		holders = holders[:0]
		res := p.pool.RunParallel(ctx, lbIPs, cmd)
		for _, r := range res {
			if r.Err != nil || r.ExitCode != 0 {
				continue
			}
			if outputHasIPv4(r.Output, vip) {
				holders = append(holders, r.Host)
			}
		}
		return len(holders) == 1, nil
	})
	if err != nil {
		return fmt.Errorf("keepalived VIP %s must be held by exactly one load balancer, holders: %v: %w",
			vip, holders, err)
	}
	logging.Info("VIP %s is held by %s", vip, holders[0])
	return nil
}

// verifyHTTPS probes the configured vhost on every balancer. The phase runs
// before the PKI phase, so the balancers still serve the self-signed bootstrap
// certificate and the probe must skip verification (`-k`); the verify phase
// later asserts the CA-issued certificate. WordPress may still be warming up
// after the frontend convergence, so the probe retries instead of warning and
// returning success.
func (p *Phase) verifyHTTPS(ctx context.Context, lbIPs []string) error {
	vhost := p.cfg.TrafficVHost()
	if len(lbIPs) == 0 {
		return fmt.Errorf("no load balancer with an address is configured")
	}
	resolve := shell.Quote(vhost + ":443:127.0.0.1")
	url := shell.Quote("https://" + vhost + "/")
	cmd := withDiagnostics(fmt.Sprintf(
		"curl -s -k -f --max-time 10 --resolve %s -o /dev/null -w '%%{http_code}' %s",
		resolve, url))

	for _, ip := range lbIPs {
		var last string
		err := retry.DoContext(ctx, httpsProbeRetry, func(ctx context.Context) error {
			out, stderr, code, err := p.pool.RunCommand(ctx, ip, cmd)
			last = strings.TrimSpace(out + " " + stderr)
			if err != nil {
				return err
			}
			if code != 0 {
				return fmt.Errorf("curl exited %d: %s", code, last)
			}
			// `-f` already rejects 4xx/5xx; require an explicit 2xx/3xx so a
			// truncated or empty -w output cannot pass as success.
			if !strings.HasPrefix(last, "2") && !strings.HasPrefix(last, "3") {
				return fmt.Errorf("unexpected HTTP status %q", last)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("HTTPS check on %s for vhost %s failed: %w", ip, vhost, err)
		}
		logging.Info("HTTPS %s answers on %s", vhost, ip)
	}
	return nil
}

// outputHasIPv4 reports whether `ip -4 -o addr show` output contains addr.
// Comparing the address column (prefix length stripped) avoids matching a
// longer address that merely starts with the VIP.
func outputHasIPv4(output, addr string) bool {
	if addr == "" {
		return false
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if f != "inet" || i+1 >= len(fields) {
				continue
			}
			if got, _, _ := strings.Cut(fields[i+1], "/"); got == addr {
				return true
			}
		}
	}
	return false
}
