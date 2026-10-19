package traffic

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/container"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

const (
	// k6ScriptPath is the embedded k6 profile executed against the CMS.
	k6ScriptPath = "templates/traffic/cms-smoke.js"
	// hostCACertPath is the lab root CA installed by the PKI phase on the
	// hypervisor. When present it is mounted into the k6 container so TLS is
	// verified instead of skipped.
	hostCACertPath = "/usr/local/share/ca-certificates/cms_root_ca.crt"
	// containerCACert is the mount point of the root CA inside the container.
	containerCACert = "/certs/cms_root_ca.crt"
)

// Options configuration for the traffic test.
type Options struct {
	Mode        string // "internal" or "external"
	TargetIP    string // override IP
	Duration    int    // seconds (default 60)
	Concurrency int    // virtual users (default 5)
	WithDB      bool
	Verbose     bool
	// K6Image overrides the pinned k6 image (air-gapped registries, CI mirror).
	K6Image string
}

// Traffic runs the k6 load profile against the CMS entry point.
type Traffic struct {
	cfg  *config.Config
	opts Options
}

// New creates a new k6 traffic runner.
func New(cfg *config.Config, opts Options) *Traffic {
	if opts.Duration <= 0 {
		opts.Duration = 60
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 5
	}
	return &Traffic{
		cfg:  cfg,
		opts: opts,
	}
}

// Run executes the k6 load profile and, optionally, a direct database check.
func (t *Traffic) Run(ctx context.Context) error {
	timer := logging.PhaseStart("Traffic Test Phase (k6)")
	defer timer.End()

	baseURL := resolveBaseURL(t.cfg, t.opts)
	logging.Info("Target URL: %s", baseURL)

	script, err := cms.TemplatesFS.ReadFile(k6ScriptPath)
	if err != nil {
		return fmt.Errorf("read embedded k6 script %s: %w", k6ScriptPath, err)
	}
	if err := t.runK6(ctx, baseURL, script); err != nil {
		return err
	}

	if t.opts.WithDB {
		if err := t.runDBCheck(ctx); err != nil {
			return err
		}
	}
	return nil
}

// resolveBaseURL derives the entry point to exercise from the options and the
// configured topology.
func resolveBaseURL(cfg *config.Config, opts Options) string {
	if opts.TargetIP != "" {
		if strings.HasPrefix(opts.TargetIP, "http://") || strings.HasPrefix(opts.TargetIP, "https://") {
			return opts.TargetIP
		}
		return "https://" + opts.TargetIP
	}
	if opts.Mode == "internal" {
		if len(cfg.Nodes.CMSFrontends) > 0 && cfg.Nodes.CMSFrontends[0].IP != "" {
			return "http://" + cfg.Nodes.CMSFrontends[0].IP
		}
		return "http://" + config.PrefixFromCIDR(cfg.Network.Main.CIDR, "192.168.20.") + "101"
	}
	if cfg.LBVIP() != "" {
		return "https://" + cfg.LBVIP()
	}
	return "https://" + config.PrefixFromCIDR(cfg.Network.Main.CIDR, "192.168.20.") + "100"
}

// k6Run groups the parameters that shape the container invocation.
type k6Run struct {
	baseURL  string
	vhost    string
	caCert   string // host path of the CA to mount, empty to skip verification
	rwURL    string // Prometheus remote-write endpoint, empty to disable
	vus      int
	duration int
}

// buildArgs renders the container runtime arguments that run k6. It is a pure
// function so the invocation can be asserted without a container runtime.
func buildArgs(image string, r k6Run) []string {
	args := []string{
		"run", "--rm", "-i",
		// The lab networks live on the hypervisor, so the container shares its
		// network namespace to reach the VIP and the internal CIDR.
		"--network", "host",
		"-e", "BASE_URL=" + r.baseURL,
		"-e", "VHOST=" + r.vhost,
	}
	if r.caCert != "" {
		args = append(args,
			"-v", r.caCert+":"+containerCACert+":ro",
			"-e", "SSL_CERT_FILE="+containerCACert,
		)
	}
	if r.rwURL != "" {
		args = append(args, "-e", "K6_PROMETHEUS_RW_SERVER_URL="+r.rwURL)
	}

	args = append(args, image, "run")
	if r.caCert == "" {
		// No CA to mount: fall back to skipping TLS verification; the
		// `cms-ha verify` command still asserts the certificate chain.
		args = append(args, "--insecure")
	}
	args = append(args, "--vus", strconv.Itoa(r.vus), "--duration", fmt.Sprintf("%ds", r.duration))
	if r.rwURL != "" {
		args = append(args, "--out", "experimental-prometheus-rw")
	}
	// Read the script from stdin (it is embedded in the binary).
	args = append(args, "-")
	return args
}

// runK6 executes the embedded profile inside the pinned k6 container.
func (t *Traffic) runK6(ctx context.Context, baseURL string, script []byte) error {
	runtime, err := container.Runtime()
	if err != nil {
		return fmt.Errorf("the k6 load test requires a container runtime: %w", err)
	}

	image := t.opts.K6Image
	if image == "" {
		image = t.cfg.K6Image()
	}

	caCert := ""
	if _, statErr := os.Stat(hostCACertPath); statErr == nil {
		caCert = hostCACertPath
	} else if t.opts.Verbose {
		logging.Warn("Root CA %s not found; k6 will skip TLS verification", hostCACertPath)
	}

	args := buildArgs(image, k6Run{
		baseURL:  baseURL,
		vhost:    t.cfg.TrafficVHost(),
		caCert:   caCert,
		rwURL:    t.cfg.Traffic.PrometheusRWURL,
		vus:      t.opts.Concurrency,
		duration: t.opts.Duration,
	})

	logging.Info("Running k6 (%s): vus=%d, duration=%ds", image, t.opts.Concurrency, t.opts.Duration)
	cmd := exec.CommandContext(ctx, runtime, args...)
	cmd.Stdin = bytes.NewReader(script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("k6 load test failed: %w", err)
	}
	logging.Success("k6 load test completed")
	return nil
}

// runDBCheck asserts WordPress data is reachable through the database service
// (an end-to-end check the HTTP profile cannot cover). It validates the
// orchestrator→NodePort network path with a raw TCP dial and then runs a real
// query through the MariaDB pod's own client via SSH + `kubectl exec`, so no SQL
// driver is embedded in the binary.
func (t *Traffic) runDBCheck(ctx context.Context) error {
	logging.Info("Direct MariaDB SQL check")
	dbHost := config.PrefixFromCIDR(t.cfg.Network.Internal.CIDR, "192.168.10.") + "10"
	masterHost := dbHost
	if len(t.cfg.Nodes.Masters) > 0 && t.cfg.Nodes.Masters[0].IP != "" {
		dbHost = t.cfg.Nodes.Masters[0].IP
		masterHost = t.cfg.Nodes.Masters[0].IP
	}
	dbPort := t.cfg.Database.Port
	if dbPort == 0 {
		dbPort = 30306
	}
	dbName := t.cfg.Database.Name
	if dbName == "" {
		dbName = "wordpress"
	}
	dbUser := t.cfg.Database.User
	if dbUser == "" {
		dbUser = "wp_user"
	}
	if t.cfg.Database.Password == "" {
		return fmt.Errorf("database.password is required for the --with-db check")
	}

	// End-to-end: the database NodePort must be reachable from the orchestrator.
	if err := dialTCP(ctx, dbHost, dbPort, 10*time.Second); err != nil {
		return fmt.Errorf("DB direct check failed: database NodePort %s is unreachable: %w", net.JoinHostPort(dbHost, strconv.Itoa(dbPort)), err)
	}

	pool, err := newTrafficSSHPool(t.cfg)
	if err != nil {
		return fmt.Errorf("DB direct check failed: %w", err)
	}
	defer pool.Close()

	if err := testDBQueries(ctx, pool, masterHost, dbUser, t.cfg.Database.Password, dbName); err != nil {
		return fmt.Errorf("DB direct queries failed: %w", err)
	}
	logging.Success("DB queries executed successfully")
	return nil
}

func newTrafficSSHPool(cfg *config.Config) (*ssh.Pool, error) {
	timeout := cfg.SSH.ConnectTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	return ssh.NewPoolWithOptions(cfg.SSH.PrivateKey, ssh.Options{
		User:           cfg.SSHUser(),
		KnownHostsFile: cfg.SSHKnownHostsFile(),
		ConnectTimeout: timeout,
	})
}

// dialTCP verifies that host:port accepts a TCP connection within timeout.
func dialTCP(ctx context.Context, host string, port int, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return conn.Close()
}

// testDBQueries runs a query against the MariaDB pod using the `mysql` client
// that ships inside the pod image (reached through kubectl on a control-plane
// node). A non-empty result proves WordPress data is loaded.
func testDBQueries(ctx context.Context, pool *ssh.Pool, master, user, password, dbName string) error {
	const query = "SELECT option_value FROM wp_options LIMIT 5"
	// Resolve the pod by label so the check does not hard-code the StatefulSet
	// ordinal, then run the client inside it (the only place a mysql client is
	// guaranteed to exist). MYSQL_PWD avoids the password landing in argv.
	script := fmt.Sprintf(
		"POD=$(kubectl get pod -n cms -l app=mariadb -o jsonpath='{.items[0].metadata.name}') && "+
			"kubectl exec -n cms \"$POD\" -- env MYSQL_PWD=%s mysql -h 127.0.0.1 -P 3306 -u %s %s -N -B -e %s",
		shellQuote(password), shellQuote(user), shellQuote(dbName), shellQuote(query))

	stdout, stderr, exitCode, err := pool.RunCommand(ctx, master, script)
	if err != nil || exitCode != 0 {
		return fmt.Errorf("query through MariaDB pod (exit %d): %v %s", exitCode, err, strings.TrimSpace(stderr))
	}
	if strings.TrimSpace(stdout) == "" {
		return fmt.Errorf("test query returned no data: wp_options is empty or the database is not loaded")
	}
	return nil
}

// shellQuote single-quotes s for a POSIX shell, escaping embedded quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
