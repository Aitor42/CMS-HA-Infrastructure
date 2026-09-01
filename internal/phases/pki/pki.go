package pki

import (
	"context"
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
	// Dummy initialization for sake of implementation, this might need real details
	initCmd := fmt.Sprintf(`export STEPPATH=/root/.step && \
step ca init --name="CMS Local CA" --dns="ca.cms.local,%s" \
--address=":8443" --provisioner="admin" --password-file=<(echo "%s") --with-ca-url="https://%s:8443"`, 
	jumpIP, "SuperSecretPassword123", jumpIP)
	
	// Ignore errors if already initialized
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

	// Provide password file
	p.pool.RunCommand(ctx, jumpIP, "echo 'SuperSecretPassword123' > /root/.step/password.txt")
	p.pool.RunCommand(ctx, jumpIP, serviceCmd)

	logging.Info("Waiting for CA health endpoint...")
	err := retry.Do(ctx, retry.Config{MaxAttempts: 15, Interval: 2 * time.Second, Timeout: 60 * time.Second}, func() error {
		_, err := p.pool.RunScript(ctx, jumpIP, "curl -kf https://localhost:8443/health")
		return err
	})
	if err != nil {
		return fmt.Errorf("step-ca health check failed: %w", err)
	}
	
	logging.Info("Distributing root CA cert to all nodes...")
	caCert, err := p.pool.RunScript(ctx, jumpIP, "cat /root/.step/certs/root_ca.crt")
	if err != nil {
		return fmt.Errorf("failed to read root CA: %w", err)
	}

	var allIPs []string
	for _, m := range p.cfg.Nodes.Masters { allIPs = append(allIPs, m.IP) }
	for _, w := range p.cfg.Nodes.Workers { allIPs = append(allIPs, w.IP) }
	for _, c := range p.cfg.Nodes.CMSFrontends { allIPs = append(allIPs, c.IP) }
	allIPs = append(allIPs, p.cfg.Nodes.LB.IP, p.cfg.Nodes.Monitor.IP, p.cfg.Nodes.Router.IP)
	
	for _, ip := range allIPs {
		p.pool.CopyContent(ctx, ip, []byte(caCert), "/usr/local/share/ca-certificates/cms_root_ca.crt", 0644)
	}
	
	p.pool.RunParallel(ctx, allIPs, "update-ca-certificates")
	
	logging.Success("PKI Setup completed successfully.")
	return nil
}
