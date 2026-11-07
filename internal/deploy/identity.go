package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
)

// Identity binds progress to the local host, deployment inputs and executable.
// ConfigSHA256 deliberately excludes credentials: hashing low-entropy passwords
// into an artifact would allow offline guessing. Secret rotation requires
// explicit convergence, not a resume that skips completed phases.
type Identity struct {
	ConfigSHA256 string `json:"config_sha256"`
	BinarySHA256 string `json:"binary_sha256"`
	HostSHA256   string `json:"host_sha256"`
	Build        string `json:"build"`
}

func configFingerprint(cfg *config.Config, opts DeployOpts) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("deployment configuration is missing")
	}
	redacted := *cfg
	redacted.Database.Password = ""
	redacted.Database.RootPassword = ""
	redacted.Database.ExporterPassword = ""
	redacted.WordPress.AdminPassword = ""
	redacted.Monitoring.GrafanaAdminPassword = ""
	redacted.PKI.ProvisionerPassword = ""
	redacted.Backup.AccessKey = ""
	redacted.Backup.SecretKey = ""
	// A resume may tune timeouts/concurrency without changing desired state.
	redacted.Deploy = config.DeployConfig{}
	data, err := json.Marshal(struct {
		Config           config.Config
		SkipVMCreate     bool
		FastProvisioning bool
	}{redacted, opts.SkipVMCreate, opts.FastProvisioning})
	if err != nil {
		return "", fmt.Errorf("fingerprint deployment configuration: %w", err)
	}
	return digest(data), nil
}

func deploymentIdentity(cfg *config.Config, opts DeployOpts) (*Identity, error) {
	fingerprint, err := configFingerprint(cfg, opts)
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("identify orchestrator executable: %w", err)
	}
	file, err := os.Open(executable)
	if err != nil {
		return nil, fmt.Errorf("read orchestrator executable: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return nil, fmt.Errorf("hash orchestrator executable: %w", err)
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("identify deployment host: %w", err)
	}
	machineID, err := os.ReadFile("/etc/machine-id")
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("identify deployment machine: %w", err)
	}
	build := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		build = info.Main.Version + " (" + info.GoVersion + ")"
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" || setting.Key == "vcs.modified" {
				build += " " + setting.Key + "=" + setting.Value
			}
		}
	}
	return &Identity{
		ConfigSHA256: fingerprint,
		BinarySHA256: hex.EncodeToString(hash.Sum(nil)),
		HostSHA256:   digest([]byte(hostname + "\x00" + strings.TrimSpace(string(machineID)))),
		Build:        build,
	}, nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// bindIdentity refuses to trust legacy or foreign progress before preflight
// can mutate the host. An explicit new state file is required for migration.
func (s *State) bindIdentity(current *Identity, opts DeployOpts) error {
	if (opts.Resume || opts.FromPhase != "") && (len(s.CompletedPhases) > 0 || len(s.AssumedPhases) > 0) {
		if s.Identity == nil {
			return fmt.Errorf("checkpoint has no deployment identity; do not resume legacy progress blindly: inspect the environment and use --from-phase with a new --state-file")
		}
		if s.Identity.ConfigSHA256 != current.ConfigSHA256 || s.Identity.BinarySHA256 != current.BinarySHA256 || s.Identity.HostSHA256 != current.HostSHA256 {
			return fmt.Errorf("checkpoint identity mismatch (configuration, executable or host changed); inspect the environment and use --from-phase with a new --state-file")
		}
	}
	s.Identity = current
	return nil
}
