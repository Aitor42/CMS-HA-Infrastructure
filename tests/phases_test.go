package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/lint"
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
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/routerbootstrap"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/traffic"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/ufw"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

func TestPhases_MetadataAndInstantiation(t *testing.T) {
	cfg := &config.Config{
		Nodes: config.NodesConfig{
			Jumpstart: config.NodeDetail{Name: "jumpstart", IP: "192.168.10.10"},
			Masters: []config.NodeDetail{
				{Name: "internal-master1", IP: "192.168.10.11"},
				{Name: "internal-master2", IP: "192.168.10.12"},
			},
			Workers: []config.NodeDetail{
				{Name: "internal-worker1", IP: "192.168.10.13"},
			},
			Router:  config.NodeDetail{Name: "ufw-router", IP: "192.168.10.1"},
			Storage: config.NodeDetail{Name: "internal-storage", IP: "192.168.10.15"},
			Monitor: config.NodeDetail{Name: "internal-monitor", IP: "192.168.10.20"},
			LBs: []config.NodeDetail{
				{Name: "main-lb1", IP: "192.168.20.103"},
				{Name: "main-lb2", IP: "192.168.20.104"},
			},
			CMSFrontends: []config.NodeDetail{
				{Name: "main-cms1", IP: "192.168.20.101"},
			},
		},
	}
	pool := &ssh.Pool{}
	lv := libvirt.NewClient("test:///default")

	testCases := []struct {
		phaseName string
		phase     phases.Phase
	}{
		{"00-init-vms", initvms.NewPhase(cfg, pool, lv)},
		{"01-setup-cobbler", cobbler.NewPhase(cfg, pool)},
		{"02-register-nodes", registernodes.NewPhase(cfg, pool)},
		{"03-repair-ssh", repairssh.NewPhase(cfg, pool)},
		{"Router Bootstrap", routerbootstrap.NewPhase(cfg, pool)},
		{"04-setup-puppet", puppet.NewPhase(cfg, pool)},
		{"DRBD Setup", drbd.NewPhase(cfg, pool)},
		{"Kubernetes Setup", kubernetes.NewPhase(cfg, pool)},
		{"NGINX & WordPress Setup", nginx.NewPhase(cfg, pool)},
		{"Monitoring Setup", monitoring.NewPhase(cfg, pool)},
		{"UFW Setup", ufw.NewPhase(cfg, pool)},
		{"Internal CA Setup", pki.NewPhase(cfg, pool)},
	}

	for _, tc := range testCases {
		t.Run(tc.phaseName, func(t *testing.T) {
			if tc.phase == nil {
				t.Fatalf("expected non-nil Phase for %s", tc.phaseName)
			}
			if tc.phase.Name() != tc.phaseName {
				t.Errorf("expected phase name %q, got %q", tc.phaseName, tc.phase.Name())
			}
			if tc.phase.Description() == "" {
				t.Errorf("expected non-empty description for phase %s", tc.phaseName)
			}
		})
	}
}

func TestPhases_InitVMsOptions(t *testing.T) {
	cfg := &config.Config{}
	pool := &ssh.Pool{}
	lv := libvirt.NewClient("test:///default")

	opts := initvms.Options{
		JumpstartOnly: true,
		NodesOnly:     false,
		Cleanup:       false,
		Recreate:      false,
	}
	p := initvms.NewPhaseWithOpts(cfg, pool, lv, opts)
	if p == nil {
		t.Fatal("expected non-nil phase with options")
	}
}

func TestPhases_TrafficGenerator(t *testing.T) {
	cfg := &config.Config{}
	opts := traffic.Options{
		Mode:        "internal",
		TargetIP:    "192.168.20.100",
		Duration:    1,
		Concurrency: 1,
		WithDB:      false,
		Verbose:     false,
	}

	tr := traffic.New(cfg, opts)
	if tr == nil {
		t.Fatal("expected non-nil traffic runner")
	}

	// The k6 profile is embedded in the binary and shipped to the container.
	script, err := cms.TemplatesFS.ReadFile("templates/traffic/cms-smoke.js")
	if err != nil {
		t.Fatalf("embedded k6 script missing: %v", err)
	}
	for _, want := range []string{"export const options", "thresholds", "http_req_duration", "wp-login.php"} {
		if !strings.Contains(string(script), want) {
			t.Errorf("k6 script missing %q", want)
		}
	}
}

// repoFile resolves rel against the module root, independently of the test
// working directory (go test always runs with the package dir as cwd, but
// editors/tools running single tests may differ).
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	root, err := lint.FindModuleRoot()
	if err != nil {
		t.Fatalf("cannot locate module root: %v", err)
	}
	return filepath.Join(root, rel)
}

func TestPhases_CMSFrontendPHPPatch(t *testing.T) {
	// The "php2shell" remote code executions (CVE-2026-109618 and
	// CVE-2026-109619, published 2026-10-21) are fixed by a PHP package
	// upgrade that nothing else in the stack would apply on its own, and the
	// mitigations the PHP team asks for are a web server configuration. Both
	// have to live in the manifest, so a frontend that missed them is
	// exposed even though the site keeps answering.
	cmsManifest, err := os.ReadFile(repoFile(t, "puppet/modules/role/manifests/cms_frontend.pp"))
	if err != nil {
		t.Fatalf("failed to read cms_frontend.pp: %v", err)
	}
	cmsContent := string(cmsManifest)
	for _, expected := range []string{
		// The patched Ubuntu revision of the PHP 8.3 branch. Ubuntu backports
		// the upstream fix, so the upstream version stays 8.3.6.
		"8.3.6-0ubuntu0.24.04.13",
		"php8.3-cli",
		"php8.3-common",
		"libapache2-mod-php8.3",
		`">= ${php_patched_version}"`,
		// The mitigations: the session path CVE-2026-109619 was confirmed
		// with, and the out-of-memory bailout CVE-2026-109618 abuses.
		"session.upload_progress.enabled",
		"php_admin_value memory_limit -1",
		"post_max_size",
		"LimitRequestBody",
		"/etc/php/8.3/apache2/conf.d/99-cms-ha-php-hardening.ini",
		"/etc/apache2/conf-available/php-hardening.conf",
	} {
		if !strings.Contains(cmsContent, expected) {
			t.Errorf("expected cms_frontend.pp to contain %q", expected)
		}
	}

	// The request size is bounded at the proxy as well: the frontends answer
	// the internet directly too, so the limit cannot live in one layer only.
	for _, path := range []string{
		"puppet/modules/role/files/nginx/cms-lb.conf",
		"templates/nginx/cms-lb.conf",
	} {
		conf, err := os.ReadFile(repoFile(t, path))
		if err != nil {
			t.Fatalf("failed to read %s: %v", path, err)
		}
		if !strings.Contains(string(conf), "client_max_body_size") {
			t.Errorf("expected %s to bound the request size", path)
		}
	}
}

func TestPhases_NFSStorageManifests(t *testing.T) {
	// Verify role::storage manifest exists and contains critical configurations
	storageManifest, err := os.ReadFile(repoFile(t, "puppet/modules/role/manifests/storage.pp"))
	if err != nil {
		t.Fatalf("failed to read storage.pp: %v", err)
	}
	storageContent := string(storageManifest)
	for _, expected := range []string{
		"class role::storage",
		"nfs-kernel-server",
		"/srv/nfs/wp-uploads",
		"/etc/exports",
		"exportfs -ra",
		"2049",
		"111",
	} {
		if !strings.Contains(storageContent, expected) {
			t.Errorf("expected storage.pp to contain %q", expected)
		}
	}

	// Verify site.pp classifies internal-storage with role::storage
	siteManifest, err := os.ReadFile(repoFile(t, "puppet/manifests/site.pp"))
	if err != nil {
		t.Fatalf("failed to read site.pp: %v", err)
	}
	siteContent := string(siteManifest)
	if !strings.Contains(siteContent, "include role::storage") {
		t.Errorf("expected site.pp to include role::storage for internal-storage")
	}

	// Verify cms_frontend.pp configures nfs-common and wp-content/uploads mount
	cmsManifest, err := os.ReadFile(repoFile(t, "puppet/modules/role/manifests/cms_frontend.pp"))
	if err != nil {
		t.Fatalf("failed to read cms_frontend.pp: %v", err)
	}
	cmsContent := string(cmsManifest)
	for _, expected := range []string{
		"'nfs-common'",
		"/var/www/html/wp-content/uploads",
		"192.168.10.15:/srv/nfs/wp-uploads",
		"fstype  => 'nfs'",
		"nfsvers=4",
		"Mount['/var/www/html/wp-content/uploads']",
		"Exec['mount-wp-uploads']",
	} {
		if !strings.Contains(cmsContent, expected) {
			t.Errorf("expected cms_frontend.pp to contain %q", expected)
		}
	}
}
