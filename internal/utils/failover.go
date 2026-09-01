package utils

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// FailoverOpts options for failover testing.
type FailoverOpts struct {
	SkipRestore bool
}

// FailoverTester struct contains dependencies for failover tests.
type FailoverTester struct {
	cfg     *config.Config
	ssh     *ssh.Pool
	libvirt *libvirt.Client
}

// NewFailoverTester initializes a new FailoverTester.
func NewFailoverTester(cfg *config.Config, s *ssh.Pool, l *libvirt.Client) *FailoverTester {
	return &FailoverTester{cfg: cfg, ssh: s, libvirt: l}
}

func getPrefix(cidr string) string {
	parts := strings.Split(cidr, ".")
	if len(parts) >= 3 {
		return fmt.Sprintf("%s.%s.%s.", parts[0], parts[1], parts[2])
	}
	return "192.168.10."
}

// Run executes the failover scenarios.
func (f *FailoverTester) Run(ctx context.Context, opts FailoverOpts) error {
	timer := logging.PhaseStart("Failover Chaos Engineering Tests")
	defer timer.End()

	f.testDRBDFailover(ctx, opts)
	f.testCMSFailover(ctx, opts)
	f.testK3sWorkerFailover(ctx, opts)

	return nil
}

func (f *FailoverTester) testDRBDFailover(ctx context.Context, opts FailoverOpts) {
	logging.Info("Scenario 1: DRBD Master Failover")
	start := time.Now()
	pass := true

	master1 := f.cfg.Nodes.Masters[0]
	master2 := f.cfg.Nodes.Masters[1]

	// Shutdown master1
	f.libvirt.Shutdown(ctx, master1.Name)
	time.Sleep(30 * time.Second)

	// Verify master2 promotes & MariaDB migrates
	out, _, _, err := f.ssh.RunCommand(ctx, master2.IP, "kubectl get pods -n cms | grep mariadb")
	if err != nil || !strings.Contains(out, "Running") {
		pass = false
		logging.Warn("MariaDB pod not running on master2")
	}

	// Verify HTTP 200
	resp, err := http.Get(fmt.Sprintf("http://%s120", getPrefix(f.cfg.Network.Main.CIDR)))
	if err != nil || resp.StatusCode != 200 {
		pass = false
		logging.Warn("CMS frontend not returning HTTP 200")
	}

	if !opts.SkipRestore {
		f.libvirt.Start(ctx, master1.Name)
		time.Sleep(30 * time.Second)
	}

	duration := time.Since(start)
	f.printSummary("DRBD Master Failover", pass, duration)
}

func (f *FailoverTester) testCMSFailover(ctx context.Context, opts FailoverOpts) {
	logging.Info("Scenario 2: CMS Frontend Failover")
	start := time.Now()
	pass := true

	cms1 := f.cfg.Nodes.CMSFrontends[0]

	// Shutdown cms1
	f.libvirt.Shutdown(ctx, cms1.Name)
	time.Sleep(15 * time.Second)

	// Verify HTTP 200 (routes to cms2)
	resp, err := http.Get(fmt.Sprintf("http://%s120", getPrefix(f.cfg.Network.Main.CIDR)))
	if err != nil || resp.StatusCode != 200 {
		pass = false
		logging.Warn("CMS frontend not returning HTTP 200 after cms1 shutdown")
	}

	if !opts.SkipRestore {
		f.libvirt.Start(ctx, cms1.Name)
		time.Sleep(15 * time.Second)
	}

	duration := time.Since(start)
	f.printSummary("CMS Frontend Failover", pass, duration)
}

func (f *FailoverTester) testK3sWorkerFailover(ctx context.Context, opts FailoverOpts) {
	logging.Info("Scenario 3: K3s Worker Failover")
	start := time.Now()
	pass := true

	worker1 := f.cfg.Nodes.Workers[0]
	worker2 := f.cfg.Nodes.Workers[1]
	master2 := f.cfg.Nodes.Masters[1]

	// Shutdown worker1
	f.libvirt.Shutdown(ctx, worker1.Name)
	time.Sleep(45 * time.Second) // wait for pod eviction

	// Verify pods reschedule to worker2
	out, _, _, err := f.ssh.RunCommand(ctx, master2.IP, "kubectl get pods -o wide -A | grep -v Terminating")
	if err != nil || !strings.Contains(out, worker2.Name) {
		pass = false
		logging.Warn("Pods not rescheduled to worker2")
	}

	if !opts.SkipRestore {
		f.libvirt.Start(ctx, worker1.Name)
		time.Sleep(30 * time.Second)
	}

	duration := time.Since(start)
	f.printSummary("K3s Worker Failover", pass, duration)
}

func (f *FailoverTester) printSummary(name string, pass bool, duration time.Duration) {
	status := "FAIL"
	if pass {
		status = "PASS"
	}
	msg := fmt.Sprintf("Scenario: %-25s | Status: %-4s | Duration: %s", name, status, duration.Round(time.Second))
	if pass {
		logging.Success("%s", msg)
	} else {
		logging.Error("%s", msg)
	}
}
