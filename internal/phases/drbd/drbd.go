package drbd

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"text/template"
	"time"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// Phase implements the DRBD setup phase
type Phase struct {
	cfg  *config.Config
	pool *ssh.Pool
}

// NewPhase creates a new DRBD Phase
func NewPhase(cfg *config.Config, pool *ssh.Pool) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

// Description returns the phase description
func (p *Phase) Description() string {
	return "Sets up DRBD replication for MariaDB data"
}

// Name returns the phase name
func (p *Phase) Name() string {
	return "DRBD Setup"
}

// Run runs the DRBD setup
func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()
	
	if len(p.cfg.Nodes.Masters) < 2 {
		return fmt.Errorf("DRBD requires at least 2 master nodes")
	}

	master1 := p.cfg.Nodes.Masters[0]
	master2 := p.cfg.Nodes.Masters[1]
	
	logging.Info("Installing DRBD utils on master nodes...")
	installCmd := "apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y drbd-utils"
	
	res := p.pool.RunParallel(ctx, []string{master1.IP, master2.IP}, installCmd)
	for _, r := range res {
		if r.Err != nil {
			return fmt.Errorf("failed to install DRBD on %s: %w", r.Host, r.Err)
		}
	}
	
	logging.Info("Deploying DRBD configuration...")
	tmplContent, err := cms.TemplatesFS.ReadFile("templates/cms-data.res.tmpl")
	if err != nil {
		return fmt.Errorf("failed to read template: %w", err)
	}
	
	tmpl, err := template.New("drbd").Parse(string(tmplContent))
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}
	
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, p.cfg); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}
	
	for _, ip := range []string{master1.IP, master2.IP} {
		if err := p.pool.CopyContent(ctx, ip, []byte(buf.String()), "/etc/drbd.d/cms-data.res", 0644); err != nil {
			return fmt.Errorf("failed to copy DRBD config to %s: %w", ip, err)
		}
	}
	
	logging.Info("Initializing DRBD metadata...")
	initCmds := []string{
		"modprobe drbd",
		"drbdadm create-md cms-data --force",
		"drbdadm up cms-data",
	}
	
	for _, ip := range []string{master1.IP, master2.IP} {
		for _, cmd := range initCmds {
			_, _, _, err := p.pool.RunCommand(ctx, ip, cmd)
			if err != nil {
				return fmt.Errorf("failed to run '%s' on %s: %w", cmd, ip, err)
			}
		}
	}
	
	logging.Info("Promoting Master 1 to Primary...")
	_, _, _, err = p.pool.RunCommand(ctx, master1.IP, "drbdadm primary cms-data --force")
	if err != nil {
		return fmt.Errorf("failed to promote primary on master1: %w", err)
	}
	
	logging.Info("Waiting for DRBD replication to start/sync...")
	err = retry.Do(ctx, retry.Config{MaxAttempts: 30, Interval: 5 * time.Second, Timeout: 5 * time.Minute}, func() error {
		out, err := p.pool.RunScript(ctx, master1.IP, "drbdadm status cms-data")
		if err != nil {
			return err
		}
		if !strings.Contains(out, "peer-disk:UpToDate") && !strings.Contains(out, "SyncTarget") {
			return fmt.Errorf("replication not ready: %s", out)
		}
		return nil
	})
	
	if err != nil {
		logging.Warn("DRBD replication sync might still be ongoing or failed: %v", err)
	}
	
	logging.Info("Formatting and mounting DRBD volume on Master 1...")
	_, _, _, err = p.pool.RunCommand(ctx, master1.IP, "mkfs.ext4 /dev/drbd0 && mkdir -p /mnt/data/mariadb && mount /dev/drbd0 /mnt/data/mariadb")
	if err != nil {
		logging.Warn("Failed to format/mount (might be already formatted): %v", err)
	}
	
	logging.Info("Updating fstab on Master 1...")
	fstabCmd := "grep -q '/dev/drbd0' /etc/fstab || echo '/dev/drbd0 /mnt/data/mariadb ext4 defaults,noauto 0 0' >> /etc/fstab"
	_, _, _, err = p.pool.RunCommand(ctx, master1.IP, fstabCmd)
	if err != nil {
		return fmt.Errorf("failed to update fstab: %w", err)
	}

	logging.Info("Deploying DRBD failover script...")
	scriptContent, err := cms.TemplatesFS.ReadFile("templates/drbd-failover.sh")
	if err != nil {
		return fmt.Errorf("failed to read drbd-failover.sh template: %w", err)
	}
	for _, ip := range []string{master1.IP, master2.IP} {
		if err := p.pool.CopyContent(ctx, ip, []byte(scriptContent), "/usr/local/bin/drbd-failover.sh", 0755); err != nil {
			return fmt.Errorf("failed to deploy failover script to %s: %w", ip, err)
		}
	}
	
	logging.Success("DRBD Setup completed successfully.")
	return nil
}
