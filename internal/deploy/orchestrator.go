package deploy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/backup"
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
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/routerbootstrap"
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
	// FromPhase runs from the named phase onwards, skipping the ones before it.
	FromPhase string
	// Resume skips every phase recorded as completed in the checkpoint file.
	Resume bool
	// StateFile is the checkpoint path (defaults to DefaultStateFile).
	StateFile string
	// TimingsFile, when set, is where the per-phase timing report is written.
	TimingsFile string
	// ConfigFile is the configuration path recorded in the timing report.
	ConfigFile string
	// FastProvisioning uses the cloud image for the jumpstart and every client
	// node. It is the opt-in fast path; PXE/Cobbler remains the default.
	FastProvisioning bool
}

// Deploy runs the entire deployment pipeline.
func (o *Orchestrator) Deploy(ctx context.Context, opts DeployOpts) (retErr error) {
	timer := logging.PhaseStart(topLevelTimer)
	defer timer.End()

	var timings *timingsRecorder
	if opts.TimingsFile != "" {
		timings = newTimingsRecorder(opts.TimingsFile, opts.ConfigFile, o.SSH)
		defer func() {
			status := "completed"
			if retErr != nil {
				status = "failed"
			}
			if err := timings.write(status); err != nil {
				logging.Warn("could not write timings file %s: %v", opts.TimingsFile, err)
			} else {
				logging.Info("Timings written to %s", opts.TimingsFile)
			}
		}()
	}

	statePath := opts.StateFile
	if statePath == "" {
		statePath = DefaultStateFile
	}

	var state *State
	if !opts.DryRun {
		var err error
		if state, err = LoadState(statePath); err != nil {
			return err
		}
	}

	if !opts.DryRun {
		if err := o.preflightChecks(ctx); err != nil {
			return fmt.Errorf("preflight checks failed: %w", err)
		}
	}

	phaseList := o.BuildPhaseList(opts)
	total := len(phaseList)

	skip, reset, err := planRun(phaseList, opts, state)
	if err != nil {
		return err
	}
	if !opts.DryRun && state != nil {
		if reset {
			state.CompletedPhases = nil
			state.Status = "running"
			state.LastError = ""
			state.StartedAt = time.Now().UTC()
		}
		if err := state.Save(statePath); err != nil {
			return err
		}
	}

	for i, phase := range phaseList {
		logging.Info("[%d/%d] Phase: %s — %s", i+1, total, phase.Name(), phase.Description())

		if opts.DryRun {
			timings.phase(phase.Name(), "dry-run", time.Time{}, 0, nil)
			logging.Info("[DRY-RUN] Skipping execution of: %s", phase.Name())
			continue
		}

		key := phaseKey(phase.Name())
		if skip[key] {
			timings.phase(phase.Name(), "skipped", time.Time{}, 0, nil)
			logging.Info("Phase %s already completed (checkpoint); skipping.", phase.Name())
			continue
		}

		phaseStart := time.Now()
		timings.setCurrent(phase.Name())
		runErr := phase.Run(ctx)
		timings.clearCurrent()

		if runErr != nil {
			timings.phase(phase.Name(), "failed", phaseStart, time.Since(phaseStart), runErr)
			if state != nil {
				state.Status = "failed"
				state.LastError = runErr.Error()
				if serr := state.Save(statePath); serr != nil {
					logging.Warn("could not persist checkpoint %s: %v", statePath, serr)
				} else {
					logging.Warn("Checkpoint saved to %s; resume with `cms-ha deploy --resume`.", statePath)
				}
			}
			return fmt.Errorf("phase %q failed: %w", phase.Name(), runErr)
		}
		timings.phase(phase.Name(), "ok", phaseStart, time.Since(phaseStart), nil)

		if state != nil {
			if err := state.MarkCompleted(statePath, key); err != nil {
				logging.Warn("could not update checkpoint %s: %v", statePath, err)
			}
		}

		if o.Cfg.Deploy.StaggerDelay > 0 && i < total-1 {
			staggerStart := time.Now()
			logging.Info("Waiting %v before next phase...", o.Cfg.Deploy.StaggerDelay)
			select {
			case <-ctx.Done():
				timings.step("stagger", staggerStart, time.Since(staggerStart))
				return ctx.Err()
			case <-time.After(o.Cfg.Deploy.StaggerDelay):
			}
			timings.step("stagger", staggerStart, time.Since(staggerStart))
		}
	}

	if state != nil {
		state.Status = "completed"
		state.LastError = ""
		if err := state.Save(statePath); err != nil {
			logging.Warn("could not persist checkpoint %s: %v", statePath, err)
		}
	}

	logging.Success("Deployment completed successfully")
	return nil
}

// BuildPhaseList constructs the ordered list of phases based on options.
func (o *Orchestrator) BuildPhaseList(opts DeployOpts) []phases.Phase {
	var list []phases.Phase

	if !opts.SkipVMCreate {
		jumpstartOpts := initvms.Options{JumpstartOnly: true}
		if opts.FastProvisioning {
			jumpstartOpts.JumpstartImage = "cloud"
		}
		list = append(list, initvms.NewPhaseWithOpts(o.Cfg, o.SSH, o.Libvirt, jumpstartOpts))
	}
	list = append(list,
		cobbler.NewPhase(o.Cfg, o.SSH),
		registernodes.NewPhase(o.Cfg, o.SSH),
	)
	if !opts.SkipVMCreate {
		list = append(list, initvms.NewPhaseWithOpts(o.Cfg, o.SSH, o.Libvirt, initvms.Options{
			NodesOnly:        true,
			FastProvisioning: opts.FastProvisioning,
		}))
	}
	list = append(list,
		repairssh.NewPhase(o.Cfg, o.SSH),
		// The router has to be routing and NATing before the Puppet phase
		// downloads anything on the client nodes; see the phase's package
		// documentation.
		routerbootstrap.NewPhase(o.Cfg, o.SSH),
		puppet.NewPhase(o.Cfg, o.SSH),
		drbd.NewPhase(o.Cfg, o.SSH),
		backup.NewPhase(o.Cfg),
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

	// Ensure the storage directory exists, then warn if it is nearly full.
	storageDir := filepath.Clean(o.Cfg.VM.StorageDir)
	if err := os.MkdirAll(storageDir, 0750); err != nil {
		// Fail now: if the images directory cannot be created, every VM
		// creation would fail much later with a far less clear error.
		return fmt.Errorf("storage directory %s cannot be created/accessed: %w", storageDir, err)
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(storageDir, &stat); err != nil {
		logging.Warn("cannot query free space of %s: %v", storageDir, err)
	} else if stat.Bsize <= 0 {
		logging.Warn("cannot query free space of %s: invalid block size %d", storageDir, stat.Bsize)
	} else {
		freeGB := stat.Bavail * uint64(stat.Bsize) / (1 << 30)
		logging.Info("Disk space: %d GB free in %s", freeGB, storageDir)
		if freeGB < 30 {
			logging.Warn("Low disk space: %d GB free (>= 30 GB recommended)", freeGB)
		}
	}

	// Warn when the host cannot back the full cluster comfortably.
	memData, err := os.ReadFile("/proc/meminfo")
	if err == nil {
		for _, line := range strings.Split(string(memData), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if kb, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
						totalGB := (kb + 512*1024) / (1024 * 1024)
						logging.Info("Host RAM: %d GB total", totalGB)
						if totalGB < 16 {
							logging.Warn("Low host RAM: %d GB total (>= 16 GB recommended for full cluster)", totalGB)
						}
					}
				}
				break
			}
		}
	}

	if _, err := o.Libvirt.ListAll(ctx); err != nil {
		return fmt.Errorf("libvirt not accessible at %s: %w", o.Cfg.VM.LibvirtURI, err)
	}

	logging.Success("Preflight checks passed")
	return nil
}
