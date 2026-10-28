package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/drbd"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

func drbdConfig() *config.Config {
	return &config.Config{
		DRBD: config.DRBDConfig{Fencing: false},
		Network: config.NetworkConfig{
			Internal: config.NetworkDetail{CIDR: "192.168.10.0/24"},
		},
		Nodes: config.NodesConfig{
			Masters: []config.NodeDetail{
				{Name: "master1", IP: "192.168.10.11"},
				{Name: "master2", IP: "192.168.10.12"},
			},
		},
	}
}

func hasScript(scripts []testutil.Command, host, substr string) bool {
	for _, s := range scripts {
		if s.Host == host && strings.Contains(s.Cmd, substr) {
			return true
		}
	}
	return false
}

func TestDRBDPhaseRun(t *testing.T) {
	cfg := drbdConfig()
	master1 := cfg.Nodes.Masters[0].IP
	master2 := cfg.Nodes.Masters[1].IP
	promoted := false
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, host, cmd string) (string, string, int, error) {
			switch {
			case strings.Contains(cmd, "drbdadm primary cms_data --force"):
				promoted = true
				return "", "", 0, nil
			case strings.Contains(cmd, "drbdadm role cms_data"):
				if host == master1 && promoted {
					return "Primary/Unknown\n", "", 0, nil
				}
				return "Secondary/Unknown\n", "", 0, nil
			case strings.Contains(cmd, "drbdadm dstate cms_data"):
				return "Inconsistent/Inconsistent\n", "", 0, nil
			case strings.Contains(cmd, "drbdadm status cms_data"):
				return "cms_data role:Primary\n  disk:UpToDate\n  master2 role:Secondary\n    replication:Established\n    peer-disk:UpToDate\n", "", 0, nil
			}
			return "", "", 0, nil
		},
	}

	if err := drbd.NewPhase(cfg, fake).Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, ip := range []string{master1, master2} {
		if len(fake.Files[ip]["/etc/drbd.d/cms_data.res"]) == 0 {
			t.Errorf("%s did not receive the DRBD resource", ip)
		}
		if len(fake.Files[ip]["/etc/drbd-fence.conf"]) == 0 {
			t.Errorf("%s did not receive the fence configuration", ip)
		}
		if len(fake.Files[ip]["/usr/local/bin/drbd-failover.sh"]) == 0 {
			t.Errorf("%s did not receive the failover script", ip)
		}
		if len(fake.Files[ip]["/etc/systemd/system/drbd-failover-watchdog.service"]) == 0 {
			t.Errorf("%s did not receive the watchdog unit", ip)
		}
		if !hasScript(fake.Scripts, ip, "if ! drbdadm create-md cms_data; then") {
			t.Errorf("%s never initialized DRBD metadata", ip)
		}
		if hasScript(fake.Scripts, ip, "drbdadm create-md cms_data --force") {
			t.Errorf("%s must not overwrite existing DRBD metadata on a re-run", ip)
		}
		if !hasCommand(fake.Commands, ip, "modinfo drbd") {
			t.Errorf("%s did not check for the DRBD kernel module", ip)
		}
		if !hasCommand(fake.Commands, ip, `linux-modules-extra-$(uname -r)`) {
			t.Errorf("%s did not install the DRBD kernel module package", ip)
		}
		if !hasCommand(fake.Commands, ip, "/dev/drbd0 /mnt/data/mariadb") {
			t.Errorf("%s did not get the fstab entry", ip)
		}
		if len(fake.Files[ip]["/etc/tmpfiles.d/mariadb-datadir.conf"]) == 0 {
			t.Errorf("%s did not receive the tmpfiles entry", ip)
		}
	}

	if !hasCommand(fake.Commands, master1, "drbdadm primary cms_data --force") {
		t.Error("master1 was not promoted to Primary")
	}
	if hasCommand(fake.Commands, master2, "drbdadm primary cms_data --force") {
		t.Error("master2 must never promote itself")
	}
	if !hasCommand(fake.Commands, master1, "drbdadm status cms_data") {
		t.Error("phase did not verify replication")
	}
	if !hasScript(fake.Scripts, master1, "mkfs.ext4") || !hasScript(fake.Scripts, master1, "mount /dev/drbd0 /mnt/data/mariadb") {
		t.Error("master1 did not format and mount the DRBD volume")
	}

	boot1 := string(fake.Files[master1]["/etc/systemd/system/drbd-cms-boot.service"])
	boot2 := string(fake.Files[master2]["/etc/systemd/system/drbd-cms-boot.service"])
	if !strings.Contains(boot1, "Environment=PROMOTE=1") {
		t.Errorf("master1 boot unit = %q, want PROMOTE=1", boot1)
	}
	if !strings.Contains(boot2, "Environment=PROMOTE=0") {
		t.Errorf("master2 boot unit = %q, want PROMOTE=0", boot2)
	}
}

func TestDRBDPhaseLeavesExistingPrimaryInPlace(t *testing.T) {
	cfg := drbdConfig()
	master1 := cfg.Nodes.Masters[0].IP
	master2 := cfg.Nodes.Masters[1].IP
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, host, cmd string) (string, string, int, error) {
			switch {
			case strings.Contains(cmd, "drbdadm role cms_data"):
				if host == master2 {
					return "Primary/Unknown\n", "", 0, nil
				}
				return "Secondary/Unknown\n", "", 0, nil
			case strings.Contains(cmd, "drbdadm dstate cms_data"):
				return "UpToDate/UpToDate\n", "", 0, nil
			case strings.Contains(cmd, "drbdadm status cms_data"):
				return "cms_data role:Primary\n  disk:UpToDate\n  master1 role:Secondary\n    replication:Established\n    peer-disk:UpToDate\n", "", 0, nil
			}
			return "", "", 0, nil
		},
	}

	if err := drbd.NewPhase(cfg, fake).Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if hasCommand(fake.Commands, master1, "drbdadm primary cms_data") {
		t.Error("master1 must not be promoted while master2 is already Primary")
	}
	if !hasCommand(fake.Commands, master2, "drbdadm status cms_data") {
		t.Error("replication was not checked on the existing Primary")
	}
	if !hasScript(fake.Scripts, master2, "mount /dev/drbd0 /mnt/data/mariadb") {
		t.Error("the existing Primary was not mounted")
	}
	if hasScript(fake.Scripts, master1, "mount /dev/drbd0 /mnt/data/mariadb") {
		t.Error("the Secondary must not be mounted")
	}
}

func TestDRBDPhaseRejectsAnExistingPrimaryWithAStaleDisk(t *testing.T) {
	cfg := drbdConfig()
	master1 := cfg.Nodes.Masters[0].IP
	master2 := cfg.Nodes.Masters[1].IP
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, host, cmd string) (string, string, int, error) {
			switch {
			case strings.Contains(cmd, "drbdadm role cms_data"):
				if host == master2 {
					return "Primary/Unknown\n", "", 0, nil
				}
				return "Secondary/Unknown\n", "", 0, nil
			case strings.Contains(cmd, "drbdadm dstate cms_data"):
				if host == master2 {
					return "Inconsistent/Inconsistent\n", "", 0, nil
				}
				return "UpToDate/UpToDate\n", "", 0, nil
			}
			return "", "", 0, nil
		},
	}

	err := drbd.NewPhase(cfg, fake).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "local disk state is Inconsistent") {
		t.Fatalf("Run() = %v, want the stale-primary refusal", err)
	}
	if hasScript(fake.Scripts, master2, "mount /dev/drbd0 /mnt/data/mariadb") {
		t.Error("a Primary with a stale disk must not be mounted")
	}
	if hasScript(fake.Scripts, master1, "mount /dev/drbd0 /mnt/data/mariadb") {
		t.Error("the Secondary must not be mounted")
	}
}

func TestDRBDPhasePromotesAnUpToDatePeerWithoutForce(t *testing.T) {
	cfg := drbdConfig()
	master1 := cfg.Nodes.Masters[0].IP
	master2 := cfg.Nodes.Masters[1].IP
	promoted := false
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, host, cmd string) (string, string, int, error) {
			switch {
			case strings.Contains(cmd, "drbdadm primary cms_data"):
				if strings.Contains(cmd, "--force") {
					t.Errorf("unexpected forced promotion: %s", cmd)
				}
				if host != master2 {
					t.Errorf("promotion ran on %s, want master2", host)
				}
				promoted = true
				return "", "", 0, nil
			case strings.Contains(cmd, "drbdadm role cms_data"):
				if host == master2 && promoted {
					return "Primary/Unknown\n", "", 0, nil
				}
				return "Secondary/Unknown\n", "", 0, nil
			case strings.Contains(cmd, "drbdadm dstate cms_data"):
				if host == master2 {
					return "UpToDate/UpToDate\n", "", 0, nil
				}
				return "Inconsistent/Inconsistent\n", "", 0, nil
			case strings.Contains(cmd, "drbdadm status cms_data"):
				return "cms_data role:Primary\n  disk:UpToDate\n  master1 role:Secondary\n    replication:Established\n    peer-disk:UpToDate\n", "", 0, nil
			}
			return "", "", 0, nil
		},
	}

	if err := drbd.NewPhase(cfg, fake).Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !promoted {
		t.Error("master2 was not promoted")
	}
	if !hasScript(fake.Scripts, master2, "mount /dev/drbd0 /mnt/data/mariadb") {
		t.Error("the UpToDate peer was not mounted")
	}
	if hasScript(fake.Scripts, master1, "mount /dev/drbd0 /mnt/data/mariadb") {
		t.Error("the Inconsistent peer must not be mounted")
	}
}

func TestDRBDPhaseRejectsSplitBrain(t *testing.T) {
	cfg := drbdConfig()
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			switch {
			case strings.Contains(cmd, "drbdadm role cms_data"):
				return "Primary/Unknown\n", "", 0, nil
			case strings.Contains(cmd, "drbdadm dstate cms_data"):
				return "UpToDate/UpToDate\n", "", 0, nil
			}
			return "", "", 0, nil
		},
	}

	err := drbd.NewPhase(cfg, fake).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "both DRBD peers report Primary") {
		t.Fatalf("Run() = %v, want a split-brain refusal", err)
	}
}

func TestDRBDPhaseRequiresTwoMasters(t *testing.T) {
	cfg := drbdConfig()
	cfg.Nodes.Masters = cfg.Nodes.Masters[:1]

	err := drbd.NewPhase(cfg, &testutil.FakeExecutor{}).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "at least 2 master nodes") {
		t.Fatalf("Run() = %v, want the master-count error", err)
	}
}

func TestDRBDPhaseReportsPromotionFailure(t *testing.T) {
	cfg := drbdConfig()
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			if strings.Contains(cmd, "drbdadm primary cms_data --force") {
				return "", "no valid meta data", 1, nil
			}
			if strings.Contains(cmd, "drbdadm status cms_data") {
				return "replication:Established\npeer-disk:UpToDate\n", "", 0, nil
			}
			return "", "", 0, nil
		},
	}

	err := drbd.NewPhase(cfg, fake).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "failed to promote") {
		t.Fatalf("Run() = %v, want the promotion failure", err)
	}
}
