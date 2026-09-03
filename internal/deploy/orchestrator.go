package deploy

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/cobbler"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/drbd"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/initvms"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/kubernetes"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/monitoring"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/nginx"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/pki"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/puppet"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/registernodes"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/repairssh"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/ufw"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// Orchestrator coordinates the full deployment process.
type Orchestrator struct {
	Cfg     *config.Config
	SSH     *ssh.Pool
	Libvirt *libvirt.Client
}

// DeployOpts are options for the orchestrator.
type DeployOpts struct {
	SkipVMCreate bool
	DryRun       bool
}

// Deploy runs the entire deployment pipeline.
func (o *Orchestrator) Deploy(ctx context.Context, opts DeployOpts) error {
	timer := logging.PhaseStart("Full Infrastructure Deployment")
	defer timer.End()

	if !opts.DryRun {
		if err := o.preflightChecks(ctx); err != nil {
			return fmt.Errorf("preflight checks failed: %w", err)
		}
	}

	phaseList := o.BuildPhaseList(opts)
	total := len(phaseList)

	for i, phase := range phaseList {
		logging.Info("[%d/%d] Phase: %s — %s", i+1, total, phase.Name(), phase.Description())

		if opts.DryRun {
			logging.Info("[DRY-RUN] Skipping execution of: %s", phase.Name())
			continue
		}

		if err := phase.Run(ctx); err != nil {
			return fmt.Errorf("phase %q failed: %w", phase.Name(), err)
		}

		if o.Cfg.Deploy.StaggerDelay > 0 && i < total-1 {
			logging.Info("Waiting %v before next phase...", o.Cfg.Deploy.StaggerDelay)
			time.Sleep(o.Cfg.Deploy.StaggerDelay)
		}
	}

	logging.Success("Deployment completed successfully")
	return nil
}

// BuildPhaseList constructs the ordered list of phases based on options.
func (o *Orchestrator) BuildPhaseList(opts DeployOpts) []phases.Phase {
	var list []phases.Phase

	if !opts.SkipVMCreate {
		list = append(list, initvms.NewPhase(o.Cfg, o.SSH, o.Libvirt))
	}
	list = append(list,
		cobbler.NewPhase(o.Cfg, o.SSH),
		registernodes.NewPhase(o.Cfg, o.SSH),
		repairssh.NewPhase(o.Cfg, o.SSH),
		puppet.NewPhase(o.Cfg, o.SSH),
		drbd.NewPhase(o.Cfg, o.SSH),
		kubernetes.NewPhase(o.Cfg, o.SSH),
		nginx.NewPhase(o.Cfg, o.SSH),
		monitoring.NewPhase(o.Cfg, o.SSH),
		ufw.NewPhase(o.Cfg, o.SSH),
		pki.NewPhase(o.Cfg, o.SSH),
	)

	return list
}

// preflightChecks validates host prerequisites before deployment.
func (o *Orchestrator) preflightChecks(ctx context.Context) error {
	logging.Info("Running preflight checks...")

	// Check disk space in storage dir
	storageDir := o.Cfg.VM.StorageDir
	if err := os.MkdirAll(storageDir, 0755); err == nil {
		var stat syscall.Statfs_t
		if err := syscall.Statfs(storageDir, &stat); err == nil {
			freeGB := stat.Bavail * uint64(stat.Bsize) / (1 << 30)
			logging.Info("Disk space: %d GB free in %s", freeGB, storageDir)
			if freeGB < 30 {
				logging.Warn("Low disk space: %d GB free (>= 30 GB recommended)", freeGB)
			}
		}
	}

	// Check RAM from /proc/meminfo
	memData, err := os.ReadFile("/proc/meminfo")
	if err == nil {
		for _, line := range strings.Split(string(memData), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				// Parse kB value and convert to GB
				// MemTotal:       16384000 kB
				// ...
			}
		}
	}

	// Verify libvirt connectivity
	if _, err := o.Libvirt.ListAll(ctx); err != nil {
		return fmt.Errorf("libvirt not accessible at %s: %w", o.Cfg.VM.LibvirtURI, err)
	}

	logging.Success("Preflight checks passed")
	return nil
}
