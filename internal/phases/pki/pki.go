package pki

import (
	"context"
	crypto_rand "crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// Phase implements the PKI setup phase
type Phase struct {
	cfg  *config.Config
	pool *ssh.Pool
}

// NewPhase creates a new PKI Phase
func NewPhase(cfg *config.Config, pool *ssh.Pool) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

// Description returns the phase description
func (p *Phase) Description() string {
	return "Sets up Internal CA with step-ca"
}

// Name returns the phase name
func (p *Phase) Name() string {
	return "Internal CA Setup"
}

// Run runs the setup
func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()
	
	jumpIP := p.cfg.Nodes.Jumpstart.IP
	
	logging.Info("Installing step-cli and step-ca on Jumpstart...")
	installCmd := "apt-get update && apt-get install -y step-cli step-ca"
	if _, _, _, err := p.pool.RunCommand(ctx, jumpIP, installCmd); err != nil {
		return fmt.Errorf("failed to install step-ca: %w", err)
	}
	
	logging.Info("Initializing step-ca...")
	caPort := p.cfg.PKI.CAPort
	if caPort == 0 {
		caPort = 8443
	}
	caPass := p.cfg.PKI.ProvisionerPassword
	if caPass == "" {
		b := make([]byte, 16)
		if _, err := crypto_rand.Read(b); err == nil {
			caPass = hex.EncodeToString(b)
		} else {
			caPass = "StepCA-Pr0v1s10ner!"
		}
	}
	caDomain := p.cfg.PKI.Domain
	if caDomain == "" {
		caDomain = "ca.internal.local"
	}

	// Prepare step directory and write password file securely with 0600 permissions
	p.pool.RunCommand(ctx, jumpIP, "mkdir -p /root/.step && chmod 700 /root/.step")
	if err := p.pool.CopyContent(ctx, jumpIP, []byte(caPass+"\n"), "/root/.step/password.txt", 0600); err != nil {
		return fmt.Errorf("failed to upload CA password: %w", err)
	}

	initCmd := fmt.Sprintf(`export STEPPATH=/root/.step && \
step ca init --name="CMS Local CA" --dns="%s,%s" \
--address=":%d" --provisioner="admin" --password-file=/root/.step/password.txt --with-ca-url="https://%s:%d"`,
		caDomain, jumpIP, caPort, jumpIP, caPort)

	// Run initialization (ignore if already initialized)
	p.pool.RunCommand(ctx, jumpIP, initCmd)

	logging.Info("Starting step-ca service...")
	serviceCmd := `cat << 'EOF' > /etc/systemd/system/step-ca.service
[Unit]
Description=step-ca
After=network.target

[Service]
ExecStart=/usr/bin/step-ca /root/.step/config/ca.json --password-file /root/.step/password.txt
Restart=on-failure
Environment="STEPPATH=/root/.step"

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload && systemctl enable --now step-ca`

	p.pool.RunCommand(ctx, jumpIP, serviceCmd)

	logging.Info("Waiting for CA health endpoint...")
	err := retry.Do(ctx, retry.Config{MaxAttempts: 15, Interval: 2 * time.Second, Timeout: 60 * time.Second}, func() error {
		_, _, _, err := p.pool.RunCommand(ctx, jumpIP, fmt.Sprintf("curl -kf https://localhost:%d/health", caPort))
		return err
	})
	if err != nil {
		return fmt.Errorf("step-ca health check failed: %w", err)
	}
	
	logging.Info("Distributing root CA cert to all nodes...")
	caCert, _, _, err := p.pool.RunCommand(ctx, jumpIP, "cat /root/.step/certs/root_ca.crt")
	if err != nil {
		return fmt.Errorf("failed to read root CA: %w", err)
	}

	var allIPs []string
	for _, node := range p.cfg.AllNodes() {
		if node.IP != "" && node.IP != jumpIP {
			allIPs = append(allIPs, node.IP)
		}
	}
	
	tasks := make(map[string]func(ctx context.Context, pool *ssh.Pool) error)
	certBytes := []byte(caCert)
	for _, ip := range allIPs {
		nodeIP := ip
		tasks[nodeIP] = func(c context.Context, pool *ssh.Pool) error {
			return pool.CopyContent(c, nodeIP, certBytes, "/usr/local/share/ca-certificates/cms_root_ca.crt", 0644)
		}
	}
	for _, r := range p.pool.RunParallelFunc(ctx, tasks) {
		if r.Err != nil {
			return fmt.Errorf("failed to distribute root CA to %s: %w", r.Host, r.Err)
		}
	}
	
	p.pool.RunParallel(ctx, allIPs, "update-ca-certificates")
	
	logging.Success("PKI Setup completed successfully.")
	return nil
}
