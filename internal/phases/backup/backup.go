package backup

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/container"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/catalog"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
)

const (
	// containerName is the Garage container on the hypervisor.
	containerName = "cms-garage"
	// garageDataDir and garageConfigPath are the in-container paths the host
	// data dir and generated config are mounted at.
	garageDataDir    = "/var/lib/garage"
	garageConfigPath = "/etc/garage.toml"
)

// Phase deploys the off-DRBD backup object store (Garage) on the hypervisor.
type Phase struct {
	cfg *config.Config
}

// NewPhase creates a new backup Phase.
func NewPhase(cfg *config.Config) phases.Phase {
	return &Phase{cfg: cfg}
}

// Name returns the phase name.
func (p *Phase) Name() string { return catalog.Backup.Name }

// Description returns the phase description.
func (p *Phase) Description() string { return catalog.Backup.Description }

// Run deploys Garage as a container on the hypervisor, with its data volume
// outside the DRBD device, and makes sure the backup bucket exists.
func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()

	if !p.cfg.BackupEnabled() {
		logging.Info("Backup disabled (backup.engine is empty); skipping.")
		return nil
	}
	if engine := p.cfg.BackupEngine(); engine != "garage" {
		return fmt.Errorf("unsupported backup engine %q (supported: garage)", engine)
	}
	if p.cfg.Backup.SecretKey == "" {
		return fmt.Errorf("backup.secret_key must be set in config.yaml (encrypt with `cms-ha secrets encrypt`) so the S3 store and the cluster share a credential")
	}

	runtime, err := container.Runtime()
	if err != nil {
		return fmt.Errorf("the Garage backup store requires a container runtime: %w", err)
	}
	logging.Info("Using container runtime: %s", runtime)

	hostIP := p.cfg.HypervisorIP()
	if hostIP == "" {
		return fmt.Errorf("cannot determine the hypervisor address from network.internal.cidr")
	}
	port := p.cfg.BackupS3Port()

	dataDir := p.cfg.BackupDataDir()
	if err := ensureDataDir(dataDir); err != nil {
		return fmt.Errorf("failed to prepare Garage data dir %s: %w", dataDir, err)
	}

	cfgPath := filepath.Join(dataDir, "garage.toml")
	if err := p.ensureGarageConfig(cfgPath, hostIP, port); err != nil {
		return err
	}

	if err := p.ensureContainer(ctx, runtime, dataDir, cfgPath); err != nil {
		return err
	}
	if err := p.waitForS3(ctx, hostIP, port); err != nil {
		return err
	}
	if err := p.ensureBucket(ctx, runtime); err != nil {
		return err
	}

	logging.Success("Garage S3 backup store ready at %s (bucket %q).", p.cfg.BackupS3Endpoint(), p.cfg.BackupBucket())
	return nil
}

// ensureDataDir makes sure the Garage data directory exists and is writable by
// the account running the deploy. The default path lives under /var/lib while
// the orchestrator runs unprivileged (the deploy account carries passwordless
// sudo instead), so a fresh hypervisor needs sudo to create the directory and
// to hand it over; the config file is written there right afterwards.
func ensureDataDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil && !errors.Is(err, fs.ErrPermission) {
		return err
	}
	if err := syscall.Access(path, 2); err == nil { // W_OK
		return nil
	}

	args := []string{
		"-n", "install", "-d", "-m", "0700",
		"-o", strconv.Itoa(os.Getuid()),
		"-g", strconv.Itoa(os.Getgid()),
		path,
	}
	cmd := exec.Command("sudo", args...) // #nosec G204 -- fixed argv; the path comes from the configuration
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sudo install -d %s failed: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	if err := syscall.Access(path, 2); err != nil {
		return fmt.Errorf("%s is still not writable after sudo install: %w", path, err)
	}
	return nil
}

// writeGarageConfig renders the Garage configuration with fresh secrets. It is
// only called when no configuration exists yet, so the cluster's secrets stay
// stable across re-deploys.
func (p *Phase) writeGarageConfig(path string) error {
	tmpl, err := cms.TemplatesFS.ReadFile("templates/backup/garage.toml.tmpl")
	if err != nil {
		return fmt.Errorf("failed to read Garage config template: %w", err)
	}

	rpcSecret, err := randomHex(32)
	if err != nil {
		return err
	}
	adminToken, err := randomBase64(32)
	if err != nil {
		return err
	}
	metricsToken, err := randomBase64(32)
	if err != nil {
		return err
	}

	content := strings.NewReplacer(
		"{{RPC_SECRET}}", rpcSecret,
		"{{ADMIN_TOKEN}}", adminToken,
		"{{METRICS_TOKEN}}", metricsToken,
		"{{S3_REGION}}", p.cfg.BackupS3Region(),
		"{{S3_BIND_ADDR}}", p.cfg.HypervisorIP(),
		"{{S3_PORT}}", strconv.Itoa(p.cfg.BackupS3Port()),
	).Replace(string(tmpl))

	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return fmt.Errorf("failed to write Garage config %s: %w", path, err)
	}
	logging.Info("Generated Garage config at %s", path)
	return nil
}

// ensureGarageConfig keeps the configuration in sync with the way the container
// is run. A config written by an older deploy binds the S3 API on "[::]" inside
// a bridge-networked container; such a store answers on the hypervisor but is
// unreachable from the lab's guests, so the bind address is the marker that
// tells us the config has to be regenerated.
func (p *Phase) ensureGarageConfig(path, hostIP string, port int) error {
	want := fmt.Sprintf("api_bind_addr = %q", net.JoinHostPort(hostIP, strconv.Itoa(port)))
	content, err := os.ReadFile(path)
	switch {
	case err == nil:
		if strings.Contains(string(content), want) {
			logging.Info("Reusing existing Garage config at %s", path)
			return nil
		}
		logging.Warn("Regenerating %s: the S3 API is not bound on %s", path, net.JoinHostPort(hostIP, strconv.Itoa(port)))
	case errors.Is(err, fs.ErrNotExist):
		// First deploy: write it below.
	default:
		return fmt.Errorf("failed to read %s: %w", path, err)
	}
	return p.writeGarageConfig(path)
}

// ensureContainer creates the Garage container, or reuses it when it already
// runs the pinned image on the host network.
func (p *Phase) ensureContainer(ctx context.Context, runtime, dataDir, cfgPath string) error {
	image := p.cfg.GarageImage()

	// Single inspect call returns the fields we need, replacing three
	// round-trips.
	if out, err := p.output(ctx, runtime, "inspect", "-f", "{{.Config.Image}}|{{.State.Running}}|{{.HostConfig.NetworkMode}}", containerName); err == nil {
		fields := strings.SplitN(strings.TrimSpace(out), "|", 3)
		current := strings.TrimSpace(fields[0])
		running := ""
		netMode := ""
		if len(fields) > 1 {
			running = strings.TrimSpace(fields[1])
		}
		if len(fields) > 2 {
			netMode = strings.TrimSpace(fields[2])
		}
		if current == image && netMode == "host" {
			if running == "true" {
				logging.Info("Garage container %s is already running.", containerName)
				return nil
			}
			logging.Info("Starting existing Garage container %s...", containerName)
			if _, err := p.output(ctx, runtime, "start", containerName); err != nil {
				return fmt.Errorf("failed to start Garage container %s: %w", containerName, err)
			}
			return nil
		}
		if current == image {
			logging.Info("Garage container %s uses %q networking; recreating it on the host network so the cluster can reach the S3 API", containerName, netMode)
		} else {
			logging.Info("Garage image changed (%q -> %q); recreating container...", current, image)
		}
		if _, err := p.output(ctx, runtime, "rm", "-f", containerName); err != nil {
			return fmt.Errorf("failed to remove old Garage container: %w", err)
		}
	}

	logging.Info("Creating Garage container from %s...", image)
	if _, err := p.output(ctx, runtime, p.garageRunArgs(image, dataDir, cfgPath)...); err != nil {
		return fmt.Errorf("failed to create Garage container: %w", err)
	}
	return nil
}

// garageRunArgs builds the `docker run` arguments. Garage runs on the host
// network: garage.toml binds the S3 API on the hypervisor's internal bridge
// address directly (never the WAN), so the store is reachable from the lab's
// guests without relying on Docker's port-publishing DNAT across the libvirt
// bridge, which the first backup drill proved does not work.
func (p *Phase) garageRunArgs(image, dataDir, cfgPath string) []string {
	return []string{
		"run", "-d",
		"--name", containerName,
		"--restart", "unless-stopped",
		"--network", "host",
		"-v", fmt.Sprintf("%s:%s", dataDir, garageDataDir),
		"-v", fmt.Sprintf("%s:%s:ro", cfgPath, garageConfigPath),
		"-e", "GARAGE_DEFAULT_ACCESS_KEY=" + p.cfg.BackupAccessKey(),
		"-e", "GARAGE_DEFAULT_SECRET_KEY=" + p.cfg.Backup.SecretKey,
		"-e", "GARAGE_DEFAULT_BUCKET=" + p.cfg.BackupBucket(),
		image,
		"/garage", "server", "--single-node", "--default-bucket",
	}
}

// waitForS3 blocks until the S3 API answers (any HTTP status means it listens).
func (p *Phase) waitForS3(ctx context.Context, hostIP string, port int) error {
	url := fmt.Sprintf("http://%s:%d/", hostIP, port)
	client := &http.Client{Timeout: 5 * time.Second}

	logging.Info("Waiting for Garage S3 endpoint at %s...", url)
	err := retry.PollContext(ctx, retry.Config{MaxAttempts: 30, Interval: 3 * time.Second, Timeout: 90 * time.Second},
		func(attemptCtx context.Context) (bool, error) {
			req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, url, nil)
			if err != nil {
				return false, err
			}
			resp, err := client.Do(req)
			if err != nil {
				return false, nil // endpoint not listening yet
			}
			_ = resp.Body.Close()
			return true, nil
		})
	if err != nil {
		return fmt.Errorf("garage S3 endpoint %s did not become ready: %w", url, err)
	}
	return nil
}

// ensureBucket creates the backup bucket and grants the default key access when
// it is missing. With `--default-bucket` Garage usually creates it on first
// start; this keeps a pre-existing volume convergent too.
func (p *Phase) ensureBucket(ctx context.Context, runtime string) error {
	bucket := p.cfg.BackupBucket()
	if _, err := p.output(ctx, runtime, "exec", containerName, "/garage", "bucket", "info", bucket); err == nil {
		return nil
	}

	logging.Info("Creating Garage bucket %q...", bucket)
	if _, err := p.output(ctx, runtime, "exec", containerName, "/garage", "bucket", "create", bucket); err != nil {
		return fmt.Errorf("failed to create Garage bucket %q: %w", bucket, err)
	}
	if _, err := p.output(ctx, runtime, "exec", containerName, "/garage", "bucket", "allow",
		"--read", "--write", "--owner", bucket, "--key", "default"); err != nil {
		logging.Warn("Could not grant the default key access to bucket %q; grant it manually: %v", bucket, err)
	}
	return nil
}

// output runs a local command and returns its stdout, wrapping failures with
// the command's stderr.
func (p *Phase) output(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// randomHex returns n random bytes hex-encoded.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to read random bytes: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// randomBase64 returns n random bytes base64-encoded.
func randomBase64(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to read random bytes: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}
