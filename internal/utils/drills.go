package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// This file implements the operational drills: the paths an operator relies on
// but a deploy + verify never touches. A green deployment proves that the
// cluster is serving; it says nothing about whether the daily backup can be
// restored, whether the certificate timer can renew, or whether the alert
// rules are quiet on a healthy lab. Each drill exercises the real production
// path (the same CronJobs and the same renewal script) and fails loudly.

// DrillsOptions controls the functional drills.
type DrillsOptions struct {
	SkipBackup bool
	SkipTLS    bool
}

// RunDrills executes the backup/restore, TLS renewal and alert-hygiene drills.
func RunDrills(ctx context.Context, cfg *config.Config, pool *ssh.Pool, opts DrillsOptions) error {
	timer := logging.PhaseStart("Operational Drills")
	defer timer.End()

	if !opts.SkipBackup {
		if err := runBackupDrill(ctx, cfg, pool); err != nil {
			return err
		}
	}
	if !opts.SkipTLS {
		if err := runTLSRenewalDrill(ctx, cfg, pool); err != nil {
			return err
		}
	}
	if err := checkNoFiringAlerts(ctx, cfg); err != nil {
		return err
	}
	logging.Success("Operational drills passed")
	return nil
}

// runBackupDrill triggers the two production CronJobs (dump + offsite upload,
// then restore into a throwaway MariaDB) and proves that Prometheus received
// the freshness metrics the alert rules read.
func runBackupDrill(ctx context.Context, cfg *config.Config, pool *ssh.Pool) error {
	master, ok := firstMaster(cfg)
	if !ok {
		return fmt.Errorf("no master node with an address is configured")
	}

	logging.Info("Drill: triggering the MariaDB backup CronJob on %s", master.Name)
	backupJob, err := runCronJob(ctx, pool, master.IP, "mariadb-backup", "cms-backup-drill", 5*time.Minute)
	if err != nil {
		return err
	}
	defer deleteJob(ctx, pool, master.IP, backupJob)

	logging.Info("Drill: triggering the restore-test CronJob on %s", master.Name)
	restoreJob, err := runCronJob(ctx, pool, master.IP, "mariadb-restore-test", "cms-restore-drill", 10*time.Minute)
	if err != nil {
		return err
	}
	defer deleteJob(ctx, pool, master.IP, restoreJob)

	monitor := cfg.Nodes.Monitor.IP
	if monitor == "" {
		return fmt.Errorf("no monitor node with an address is configured")
	}
	base := "http://" + monitor + ":9090"
	freshAfter := time.Now().Add(-10 * time.Minute).Unix()
	for _, series := range []string{
		"cms_backup_last_success_timestamp_seconds",
		"cms_backup_restore_last_success_timestamp_seconds",
	} {
		if !waitFor(ctx, 90*time.Second, 5*time.Second, func() bool {
			values, qerr := prometheusSeries(ctx, base, series)
			if qerr != nil || len(values) == 0 {
				return false
			}
			for _, v := range values {
				if int64(v) >= freshAfter {
					return true
				}
			}
			return false
		}) {
			return fmt.Errorf("prometheus has no fresh %s series after the drill: the job succeeded but the textfile metric did not reach node_exporter", series)
		}
	}
	logging.Success("Drill: backup and restore both completed and their metrics are in Prometheus")
	return nil
}

// runCronJob creates a one-shot Job from a CronJob and waits for it to
// succeed. The job name is unique so repeated drills never collide.
func runCronJob(ctx context.Context, pool *ssh.Pool, host, cron, prefix string, timeout time.Duration) (string, error) {
	name := fmt.Sprintf("%s-%d", prefix, time.Now().Unix())
	cmd := fmt.Sprintf("kubectl create job --from=cronjob/%s %s -n cms", cron, name)
	if out, errOut, code, err := pool.RunCommand(ctx, host, cmd); err != nil || code != 0 {
		return "", fmt.Errorf("create job from cronjob/%s (exit %d): %v %s%s", cron, code, err, strings.TrimSpace(out), strings.TrimSpace(errOut))
	}
	if !waitFor(ctx, timeout, 5*time.Second, func() bool {
		out, _, _, err := pool.RunCommand(ctx, host, fmt.Sprintf("kubectl get job %s -n cms -o jsonpath='{.status.succeeded}'", name))
		return err == nil && strings.TrimSpace(out) == "1"
	}) {
		return name, fmt.Errorf("job %s did not complete within %s\n%s", name, timeout, jobDiagnostics(ctx, pool, host, name))
	}
	return name, nil
}

// jobDiagnostics collects why a drill job failed: the Job conditions, the Pod
// state and the logs of every container (including init containers).
func jobDiagnostics(ctx context.Context, pool *ssh.Pool, host, name string) string {
	const tmpl = "kubectl describe job %[1]s -n cms 2>&1; " +
		"echo '--- pods ---'; kubectl get pods -n cms -l job-name=%[1]s -o wide 2>&1; " +
		"echo '--- logs ---'; kubectl logs -n cms -l job-name=%[1]s --all-containers --tail=80 2>&1"
	out, errOut, _, err := pool.RunCommand(ctx, host, fmt.Sprintf(tmpl, name))
	if err != nil {
		return fmt.Sprintf("diagnostics unavailable: %v", err)
	}
	return strings.TrimSpace(out + "\n" + errOut)
}

// deleteJob removes a drill job; the drill's evidence is already in the logs
// and in Prometheus, so leaving completed pods around only clutters the lab.
func deleteJob(ctx context.Context, pool *ssh.Pool, host, name string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if _, _, _, err := pool.RunCommand(cleanupCtx, host, fmt.Sprintf("kubectl delete job %s -n cms --ignore-not-found --wait=false", name)); err != nil {
		logging.Warn("Could not delete drill job %s: %v", name, err)
	}
}

// runTLSRenewalDrill runs the production renewal script on a balancer with a
// lowered threshold, so the CA is really contacted over mTLS and the
// certificate is really rotated. The backup balancer is used so the VIP holder
// keeps serving the current certificate while the drill runs.
func runTLSRenewalDrill(ctx context.Context, cfg *config.Config, pool *ssh.Pool) error {
	if len(cfg.Nodes.LBs) == 0 {
		return fmt.Errorf("no load balancer is configured")
	}
	lb := cfg.Nodes.LBs[len(cfg.Nodes.LBs)-1]
	if lb.IP == "" {
		return fmt.Errorf("load balancer %s has no address", lb.Name)
	}

	logging.Info("Drill: renewing the TLS certificate on %s", lb.Name)
	// RENEW_FORCE=1 makes the production script call `step ca renew --force`:
	// step rejects an --expires-in above the certificate lifetime, so this is
	// the only deterministic way to contact the CA and rotate the certificate.
	out, errOut, code, err := pool.RunCommand(ctx, lb.IP, "RENEW_FORCE=1 /usr/local/bin/cms-tls-renew.sh")
	combined := strings.TrimSpace(out + "\n" + errOut)
	if err != nil || code != 0 {
		return fmt.Errorf("TLS renewal failed on %s (exit %d): %v\n%s", lb.Name, code, err, combined)
	}
	if !strings.Contains(combined, "certificate renewed") {
		return fmt.Errorf("TLS renewal on %s did not rotate the certificate:\n%s", lb.Name, combined)
	}
	if _, _, code, err := pool.RunCommand(ctx, lb.IP, "systemctl is-active nginx"); err != nil || code != 0 {
		return fmt.Errorf("nginx is not active on %s after the certificate renewal", lb.Name)
	}
	if !checkLBReachability(ctx, lb.IP, cfg.TrafficVHost()) {
		return fmt.Errorf("%s does not answer HTTPS after the certificate renewal", lb.Name)
	}
	logging.Success("Drill: %s rotated its certificate over mTLS and reloaded nginx", lb.Name)
	return nil
}

// checkNoFiringAlerts asserts that a healthy lab has no firing alerts. It runs
// after the drills so a stale BackupRestoreTestFailing (the metric was missing
// because the drill had never run) has time to resolve.
func checkNoFiringAlerts(ctx context.Context, cfg *config.Config) error {
	monitor := cfg.Nodes.Monitor.IP
	if monitor == "" {
		return fmt.Errorf("no monitor node with an address is configured")
	}
	base := "http://" + monitor + ":9090"
	query := url.Values{"query": {`ALERTS{alertstate="firing"}`}}

	var firing []string
	var queryErr error
	ok := waitFor(ctx, 90*time.Second, 5*time.Second, func() bool {
		firing = firing[:0]
		queryErr = nil
		status, body, err := httpGet(ctx, base+"/api/v1/query?"+query.Encode(), "", functionalTimeout, false)
		if err != nil {
			queryErr = err
			return false
		}
		if status != http.StatusOK {
			queryErr = fmt.Errorf("prometheus returned HTTP %d", status)
			return false
		}
		var payload struct {
			Data struct {
				Result []struct {
					Metric map[string]string `json:"metric"`
				} `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			queryErr = err
			return false
		}
		for _, r := range payload.Data.Result {
			name := r.Metric["alertname"]
			if name == "" {
				name = "unknown"
			}
			firing = append(firing, name)
		}
		return len(firing) == 0
	})
	if !ok {
		if len(firing) > 0 {
			return fmt.Errorf("the lab is healthy but %d alert(s) are firing: %s", len(firing), strings.Join(firing, ", "))
		}
		if queryErr != nil {
			return fmt.Errorf("could not query prometheus for firing alerts: %w", queryErr)
		}
		return fmt.Errorf("could not query prometheus for firing alerts")
	}
	logging.Success("Drill: no alert is firing on the healthy lab")
	return nil
}

func firstMaster(cfg *config.Config) (config.NodeDetail, bool) {
	for _, m := range cfg.Nodes.Masters {
		if m.IP != "" {
			return m, true
		}
	}
	return config.NodeDetail{}, false
}
