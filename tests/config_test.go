package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
)

func TestConfig_LoadAndNodeSpecs(t *testing.T) {
	yamlContent := `
vm:
  storage_dir: "~/vm_storage"
  libvirt_uri: "qemu:///system"

ssh:
  private_key: "~/.ssh/id_ed25519"
  connect_timeout: 10s

network:
  internal:
    cidr: "192.168.10.0/24"
    gateway: "192.168.10.1"
    domain: "internal.local"
    bridge: "virbr-int"
  main:
    cidr: "192.168.20.0/24"
    gateway: "192.168.20.1"
    domain: "main.local"
    bridge: "virbr-main"
    vip: "192.168.20.100"

nodes:
  router:
    name: "ufw-router"
    ip: "192.168.10.1"
    mac: "52:54:00:10:01:02"
    ram_install_mb: 3072
    ram_final_mb: 768
    vcpus: 1
    disk_gb: 5
  jumpstart:
    name: "jumpstart"
    ip: "192.168.10.10"
    ip_main: "192.168.20.10"
    mac_internal: "52:54:00:10:00:01"
    mac_main: "52:54:00:10:02:0a"
    ram_install_mb: 3072
    ram_final_mb: 2048
    vcpus: 2
    disk_gb: 30
  masters:
    - name: "internal-master1"
      ip: "192.168.10.11"
      mac: "52:54:00:10:01:11"
      ram_install_mb: 4096
      ram_final_mb: 1024
      vcpus: 1
      disk_gb: 8
      drbd_disk_gb: 3
    - name: "internal-master2"
      ip: "192.168.10.12"
      mac: "52:54:00:10:01:12"
      ram_install_mb: 4096
      ram_final_mb: 1024
      vcpus: 1
      disk_gb: 8
      drbd_disk_gb: 3
    - name: "internal-master3"
      ip: "192.168.10.16"
      mac: "52:54:00:10:01:16"
      ram_install_mb: 4096
      ram_final_mb: 1024
      vcpus: 1
      disk_gb: 8
  workers:
    - name: "internal-worker1"
      ip: "192.168.10.13"
      mac: "52:54:00:10:01:13"
      ram_install_mb: 4096
      ram_final_mb: 768
      vcpus: 1
      disk_gb: 8
  storage:
    name: "internal-storage"
    ip: "192.168.10.15"
    mac: "52:54:00:10:01:15"
    ram_install_mb: 3072
    ram_final_mb: 1024
    vcpus: 1
    disk_gb: 8
  monitor:
    name: "internal-monitor"
    ip: "192.168.10.20"
    mac: "52:54:00:10:01:10"
    ram_install_mb: 3072
    ram_final_mb: 1024
    vcpus: 1
    disk_gb: 4
  lbs:
    - name: "main-lb1"
      ip: "192.168.20.103"
      mac: "52:54:00:10:02:67"
      ram_install_mb: 3072
      ram_final_mb: 768
      vcpus: 1
      disk_gb: 4
    - name: "main-lb2"
      ip: "192.168.20.104"
      mac: "52:54:00:10:02:68"
      ram_install_mb: 3072
      ram_final_mb: 768
      vcpus: 1
      disk_gb: 4
  cms_frontends:
    - name: "main-cms1"
      ip: "192.168.20.101"
      mac: "52:54:00:10:02:65"
      ram_install_mb: 3072
      ram_final_mb: 1024
      vcpus: 1
      disk_gb: 4
  hotdesks:
    count: 2
    max: 8
    ram_install_mb: 3072
    ram_final_mb: 768
    vcpus: 1
    disk_gb: 3
    base_ip_octet: 201

database:
  name: "wordpress"
  user: "wp_user"
  password: "WpS3cur3P4ss!"
  root_password: "mysqlrootpass"
  port: 30306

monitoring:
  exporters:
    nginx_version: "1.4.1"
    apache_version: "1.0.9"
    mysqld_version: "0.16.0"

pki:
  ca_port: 8443
  provisioner_password: "StepCA-Pr0v1s10ner!"
  step_version: "0.27.5"
  step_ca_version: "0.27.5"
  domain: "ca.internal.local"

deploy:
  ssh_timeout: 600s
  stagger_delay: 5s
`
	tmpFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmpFile, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := config.Load(tmpFile)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if cfg.Database.Name != "wordpress" {
		t.Errorf("expected DB name wordpress, got %s", cfg.Database.Name)
	}

	// Verify path expansion (no raw ~)
	if cfg.VM.StorageDir == "~/vm_storage" {
		t.Errorf("expected tilde expansion in StorageDir, got %s", cfg.VM.StorageDir)
	}

	internalNodes := cfg.InternalNodes()
	if len(internalNodes) != 8 {
		t.Errorf("expected 8 internal nodes, got %d", len(internalNodes))
	}

	mainNodes := cfg.MainNodes()
	if len(mainNodes) != 6 {
		t.Errorf("expected 6 main nodes, got %d", len(mainNodes))
	}

	allNodes := cfg.AllNodes()
	if len(allNodes) == 0 {
		t.Errorf("expected allNodes to be populated")
	}
}

// TestConfig_LegacyBatchModeFieldIsIgnored makes sure that removing the
// batch_mode field does not break config.yaml files that still carry it.
func TestConfig_LegacyBatchModeFieldIsIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
vm:
  storage_dir: "~/vm_storage"
ssh:
  private_key: "~/.ssh/id_ed25519"
  batch_mode: true
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("loading a config with the removed batch_mode field failed: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected a non-nil config")
	}
}

func TestConfig_SecretsEncryption(t *testing.T) {
	pub, priv, err := config.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	if pub == "" || priv == "" {
		t.Fatalf("empty keypair generated")
	}

	secret := "SuperSecretPassword123"
	encrypted, err := config.EncryptValue(secret, pub)
	if err != nil {
		t.Fatalf("failed to encrypt value: %v", err)
	}

	if !config.IsEncrypted(encrypted) {
		t.Fatalf("expected value to be marked as encrypted: %s", encrypted)
	}

	keyFile := filepath.Join(t.TempDir(), "age.key")
	if err := os.WriteFile(keyFile, []byte(priv), 0600); err != nil {
		t.Fatalf("failed to write key file: %v", err)
	}

	decrypted, err := config.DecryptValue(encrypted, keyFile)
	if err != nil {
		t.Fatalf("failed to decrypt value: %v", err)
	}

	if decrypted != secret {
		t.Fatalf("expected decrypted value %q, got %q", secret, decrypted)
	}
}

func TestConfig_JumpstartImageMode(t *testing.T) {
	cases := []struct {
		set  string
		want string
	}{
		{"", "iso"},
		{"iso", "iso"},
		{" Cloud ", "cloud"},
	}
	for _, tc := range cases {
		cfg := &config.Config{VM: config.VMConfig{JumpstartImage: tc.set}}
		if got := cfg.JumpstartImageMode(); got != tc.want {
			t.Errorf("JumpstartImageMode(%q) = %q, want %q", tc.set, got, tc.want)
		}
	}
}

func TestConfig_ClientProvisioningMode(t *testing.T) {
	cases := []struct {
		set  string
		want string
	}{
		{"", "pxe"},
		{"pxe", "pxe"},
		{" Cloud ", "cloud"},
	}
	for _, tc := range cases {
		cfg := &config.Config{VM: config.VMConfig{Provisioning: tc.set}}
		if got := cfg.ClientProvisioningMode(); got != tc.want {
			t.Errorf("ClientProvisioningMode(%q) = %q, want %q", tc.set, got, tc.want)
		}
	}
}

func TestConfig_RuntimeRAMMB(t *testing.T) {
	cases := []struct {
		name string
		node config.NodeDetail
		want int
	}{
		{"final wins", config.NodeDetail{RAMInstallMB: 3072, RAMFinalMB: 1024}, 1024},
		{"install fallback", config.NodeDetail{RAMInstallMB: 3072}, 3072},
		{"zero fallback", config.NodeDetail{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.node.RuntimeRAMMB(); got != tc.want {
				t.Errorf("RuntimeRAMMB() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestConfig_Validate(t *testing.T) {
	validCfg := func() *config.Config {
		return &config.Config{
			VM: config.VMConfig{
				StorageDir: "/var/lib/libvirt/images",
			},
			Network: config.NetworkConfig{
				Internal: config.NetworkDetail{CIDR: "192.168.10.0/24"},
				Main:     config.NetworkDetail{CIDR: "192.168.20.0/24", VIP: "192.168.20.100"},
			},
			Nodes: config.NodesConfig{
				Router:    config.NodeDetail{Name: "ufw-router", IP: "192.168.10.1"},
				Jumpstart: config.NodeDetail{Name: "jumpstart", IP: "192.168.10.10"},
				Masters: []config.NodeDetail{
					{Name: "master1", IP: "192.168.10.11"},
					{Name: "master2", IP: "192.168.10.12"},
					{Name: "master3", IP: "192.168.10.16"},
				},
				LBs: []config.NodeDetail{
					{Name: "lb1", IP: "192.168.20.103"},
					{Name: "lb2", IP: "192.168.20.104"},
				},
				CMSFrontends: []config.NodeDetail{
					{Name: "cms1", IP: "192.168.20.101"},
				},
			},
		}
	}

	t.Run("valid configuration passes", func(t *testing.T) {
		cfg := validCfg()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("expected valid config to pass, got: %v", err)
		}
	})

	t.Run("missing storage_dir", func(t *testing.T) {
		cfg := validCfg()
		cfg.VM.StorageDir = ""
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for empty storage_dir")
		}
	})

	t.Run("invalid jumpstart image", func(t *testing.T) {
		cfg := validCfg()
		cfg.VM.JumpstartImage = "pxe"
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for an unknown jumpstart image")
		}
	})

	t.Run("invalid client provisioning", func(t *testing.T) {
		cfg := validCfg()
		cfg.VM.Provisioning = "fast"
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for an unknown client provisioning mode")
		}
	})

	// Validate is split into helpers; the split must not reorder the checks,
	// because callers rely on the first broken field being the one reported.
	t.Run("deploy error precedes network error", func(t *testing.T) {
		cfg := validCfg()
		cfg.VM.StorageDir = ""
		cfg.Network.Internal.CIDR = "not-a-cidr"
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "vm.storage_dir") {
			t.Fatalf("Validate() = %v, want the vm.storage_dir error first", err)
		}
	})

	t.Run("network error precedes topology error", func(t *testing.T) {
		cfg := validCfg()
		cfg.Network.Internal.CIDR = "not-a-cidr"
		cfg.Nodes.Router.IP = ""
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "network.internal.cidr") {
			t.Fatalf("Validate() = %v, want the network.internal.cidr error first", err)
		}
	})

	t.Run("missing internal CIDR", func(t *testing.T) {
		cfg := validCfg()
		cfg.Network.Internal.CIDR = ""
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for missing internal CIDR")
		}
	})

	t.Run("missing main CIDR", func(t *testing.T) {
		cfg := validCfg()
		cfg.Network.Main.CIDR = ""
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for missing main CIDR")
		}
	})

	t.Run("missing router IP", func(t *testing.T) {
		cfg := validCfg()
		cfg.Nodes.Router.IP = ""
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for missing router IP")
		}
	})

	t.Run("missing jumpstart IP", func(t *testing.T) {
		cfg := validCfg()
		cfg.Nodes.Jumpstart.IP = ""
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for missing jumpstart IP")
		}
	})

	t.Run("insufficient masters for HA", func(t *testing.T) {
		cfg := validCfg()
		cfg.Nodes.Masters = []config.NodeDetail{
			{Name: "master1", IP: "192.168.10.11"},
			{Name: "master2", IP: "192.168.10.12"},
		}
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for <3 masters (even count has no failure tolerance)")
		}
	})

	t.Run("even number of masters is rejected", func(t *testing.T) {
		cfg := validCfg()
		cfg.Nodes.Masters = []config.NodeDetail{
			{Name: "master1", IP: "192.168.10.11"},
			{Name: "master2", IP: "192.168.10.12"},
			{Name: "master3", IP: "192.168.10.16"},
			{Name: "master4", IP: "192.168.10.17"},
		}
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for an even number of masters (no failure tolerance)")
		}
	})

	t.Run("half-configured fence is rejected", func(t *testing.T) {
		cfg := validCfg()
		cfg.DRBD.FenceCommand = "ssh host sudo virsh destroy {domain}"
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error when only fence_command is set")
		}
	})

	t.Run("missing LB VIP", func(t *testing.T) {
		cfg := validCfg()
		cfg.Network.Main.VIP = ""
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for missing VIP")
		}
	})

	t.Run("single load balancer is rejected", func(t *testing.T) {
		cfg := validCfg()
		cfg.Nodes.LBs = cfg.Nodes.LBs[:1]
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for a single LB (no HA)")
		}
	})

	t.Run("VIP colliding with a node is rejected", func(t *testing.T) {
		cfg := validCfg()
		cfg.Network.Main.VIP = "192.168.20.103"
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error when the VIP collides with an LB IP")
		}
	})

	t.Run("missing CMS frontends", func(t *testing.T) {
		cfg := validCfg()
		cfg.Nodes.CMSFrontends = nil
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for missing CMS frontends")
		}
	})

	t.Run("invalid node name", func(t *testing.T) {
		cfg := validCfg()
		cfg.Nodes.Router.Name = "router; rm -rf /"
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for a node name with unsafe characters")
		}
	})

	t.Run("invalid database identifier", func(t *testing.T) {
		cfg := validCfg()
		cfg.Database.Name = "wordpress; DROP"
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for an unsafe database.name")
		}
	})

	t.Run("hotdesk range overflow reports the dedicated message", func(t *testing.T) {
		cfg := validCfg()
		cfg.Nodes.Hotdesks = config.HotdesksConf{Count: 60}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error for an overflowing hotdesk range")
		}
		// The overflow check must run before the per-node IP sweep, otherwise
		// this surfaces as the far less clear "invalid IPv4 .../192.168.20.260".
		if !strings.Contains(err.Error(), "overflows") {
			t.Errorf("expected the dedicated overflow message, got: %v", err)
		}
	})

	t.Run("floating k6 tag is rejected", func(t *testing.T) {
		cfg := validCfg()
		cfg.Traffic.K6Version = "latest"
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for a floating traffic.k6_version")
		}
	})

	t.Run("invalid prometheus remote-write URL is rejected", func(t *testing.T) {
		cfg := validCfg()
		cfg.Traffic.PrometheusRWURL = "192.168.10.20:9090/api/v1/write"
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for a scheme-less traffic.prometheus_rw_url")
		}
	})

	t.Run("unsafe Grafana admin password is rejected", func(t *testing.T) {
		cfg := validCfg()
		cfg.Monitoring.GrafanaAdminPassword = "bad password\nGF_SECURITY_ADMIN_USER=attacker"
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for a Grafana password that breaks the EnvironmentFile")
		}
	})
}

// TestConfig_ValidatePartial guards the relaxed validation used by commands
// that act on whatever resources are present (cleanup/destroy, VM lifecycle,
// status and repair): they must accept an incomplete topology so the operator
// can still manage or remove whatever VMs, disks and networks exist after the
// config was broken.
func TestConfig_ValidatePartial(t *testing.T) {
	t.Run("partial topology is accepted", func(t *testing.T) {
		// No router, masters, LBs or VIP: none of it is needed to tear down.
		cfg := &config.Config{
			VM: config.VMConfig{StorageDir: "/var/lib/libvirt/images"},
		}
		if err := cfg.ValidatePartial(); err != nil {
			t.Fatalf("expected a partial config to be accepted for teardown, got: %v", err)
		}
	})

	t.Run("storage_dir is still required", func(t *testing.T) {
		if err := (&config.Config{}).ValidatePartial(); err == nil {
			t.Fatal("expected an error when vm.storage_dir is empty")
		}
	})

	t.Run("unsafe node name is still rejected", func(t *testing.T) {
		cfg := &config.Config{
			VM:    config.VMConfig{StorageDir: "/var/lib/libvirt/images"},
			Nodes: config.NodesConfig{Router: config.NodeDetail{Name: "router; rm -rf /"}},
		}
		if err := cfg.ValidatePartial(); err == nil {
			t.Fatal("expected partial validation to reject an unsafe node name")
		}
	})
}

func TestConfig_HypervisorIPAndFenceCommands(t *testing.T) {
	cfg := &config.Config{
		Network: config.NetworkConfig{
			Internal: config.NetworkDetail{CIDR: "192.168.10.0/24"},
		},
	}

	if got := cfg.HypervisorIP(); got != "192.168.10.254" {
		t.Errorf("HypervisorIP() = %q, want 192.168.10.254", got)
	}

	t.Run("derived fencing commands target the hypervisor", func(t *testing.T) {
		off, state := cfg.FenceCommands()
		for _, cmd := range []string{off, state} {
			if !strings.Contains(cmd, "192.168.10.254") {
				t.Errorf("derived fence command %q must target the hypervisor address", cmd)
			}
			if !strings.Contains(cmd, "{domain}") {
				t.Errorf("derived fence command %q must keep the {domain} placeholder", cmd)
			}
		}
		if !strings.Contains(off, "destroy") {
			t.Errorf("power-off command %q must use virsh destroy", off)
		}
		if !strings.Contains(state, "domstate") {
			t.Errorf("state command %q must use virsh domstate", state)
		}
	})

	t.Run("custom commands are returned verbatim", func(t *testing.T) {
		cfg.DRBD.FenceCommand = "fence-off {domain}"
		cfg.DRBD.FenceStateCommand = "fence-state {domain}"
		off, state := cfg.FenceCommands()
		if off != "fence-off {domain}" || state != "fence-state {domain}" {
			t.Errorf("custom commands not returned verbatim: %q / %q", off, state)
		}
	})
}

func TestConfig_LoadBalancerTLS(t *testing.T) {
	base := func() *config.Config {
		return &config.Config{
			Network: config.NetworkConfig{
				Main: config.NetworkDetail{CIDR: "192.168.20.0/24", VIP: "192.168.20.100"},
			},
			Nodes: config.NodesConfig{
				LBs: []config.NodeDetail{
					{Name: "lb1", IP: "192.168.20.103"},
					{Name: "lb2", IP: "192.168.20.104"},
				},
			},
		}
	}

	t.Run("defaults when unset", func(t *testing.T) {
		cfg := base()
		if got := cfg.PKICommonName(); got != "CMS Local CA" {
			t.Errorf("PKICommonName() = %q, want CMS Local CA", got)
		}
		if got := cfg.LBTLSDNSNames(); len(got) != 1 || got[0] != "cms.fake-enterprise.com" {
			t.Errorf("LBTLSDNSNames() = %v, want [cms.fake-enterprise.com]", got)
		}
		if got := cfg.LBTLSCertMinValidDays(); got != 30 {
			t.Errorf("LBTLSCertMinValidDays() = %d, want 30", got)
		}
		if got := cfg.LBTLSCertLifetimeHours(); got != 2160 {
			t.Errorf("LBTLSCertLifetimeHours() = %d, want 2160", got)
		}
	})

	t.Run("SANs include DNS names, VIP and both balancers", func(t *testing.T) {
		cfg := base()
		cfg.PKI.CertDNSNames = []string{"cms.example.com", "www.example.com"}
		sans := cfg.LBTLSSANs()
		for _, want := range []string{"cms.example.com", "www.example.com", "192.168.20.100", "192.168.20.103", "192.168.20.104"} {
			if !contains(sans, want) {
				t.Errorf("LBTLSSANs() = %v, missing %q", sans, want)
			}
		}
	})

	t.Run("overrides are honoured", func(t *testing.T) {
		cfg := base()
		cfg.PKI.CAName = "Custom CA"
		cfg.PKI.CertMinValidDays = 45
		cfg.PKI.CertLifetimeHours = 4380
		if got := cfg.PKICommonName(); got != "Custom CA" {
			t.Errorf("PKICommonName() = %q", got)
		}
		if got := cfg.LBTLSCertMinValidDays(); got != 45 {
			t.Errorf("LBTLSCertMinValidDays() = %d", got)
		}
		if got := cfg.LBTLSCertLifetimeHours(); got != 4380 {
			t.Errorf("LBTLSCertLifetimeHours() = %d", got)
		}
	})

	t.Run("validation rejects a lifetime shorter than the minimum validity", func(t *testing.T) {
		cfg := base()
		cfg.VM.StorageDir = "/var/lib/libvirt/images"
		cfg.Network.Internal.CIDR = "192.168.10.0/24"
		cfg.Nodes.Router.IP = "192.168.10.1"
		cfg.Nodes.Jumpstart.IP = "192.168.10.10"
		cfg.Nodes.Masters = []config.NodeDetail{
			{Name: "m1", IP: "192.168.10.11"},
			{Name: "m2", IP: "192.168.10.12"},
			{Name: "m3", IP: "192.168.10.16"},
		}
		cfg.Nodes.CMSFrontends = []config.NodeDetail{{Name: "cms1", IP: "192.168.20.101"}}
		cfg.PKI.CertMinValidDays = 30
		cfg.PKI.CertLifetimeHours = 720 // 30 days, not strictly greater
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for a lifetime not exceeding cert_min_valid_days")
		}
	})

	t.Run("validation rejects an invalid certificate DNS name", func(t *testing.T) {
		cfg := base()
		cfg.VM.StorageDir = "/var/lib/libvirt/images"
		cfg.Network.Internal.CIDR = "192.168.10.0/24"
		cfg.Nodes.Router.IP = "192.168.10.1"
		cfg.Nodes.Jumpstart.IP = "192.168.10.10"
		cfg.Nodes.Masters = []config.NodeDetail{
			{Name: "m1", IP: "192.168.10.11"},
			{Name: "m2", IP: "192.168.10.12"},
			{Name: "m3", IP: "192.168.10.16"},
		}
		cfg.Nodes.CMSFrontends = []config.NodeDetail{{Name: "cms1", IP: "192.168.20.101"}}
		cfg.PKI.CertDNSNames = []string{"not a valid name"}
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for an invalid certificate DNS name")
		}
	})
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// TestConfig_SSHAccessHardening: config.yaml must point at a non-root deploy
// account and a known-hosts store, defaults must be safe, and an invalid
// account name must be rejected.
func TestConfig_SSHAccessHardening(t *testing.T) {
	validCfg := func() *config.Config {
		cfg := &config.Config{
			VM: config.VMConfig{StorageDir: "/var/lib/libvirt/images"},
			Network: config.NetworkConfig{
				Internal: config.NetworkDetail{CIDR: "192.168.10.0/24"},
				Main:     config.NetworkDetail{CIDR: "192.168.20.0/24", VIP: "192.168.20.100"},
			},
			Nodes: config.NodesConfig{
				Router:    config.NodeDetail{Name: "ufw-router", IP: "192.168.10.1"},
				Jumpstart: config.NodeDetail{Name: "jumpstart", IP: "192.168.10.10"},
				Masters: []config.NodeDetail{
					{Name: "m1", IP: "192.168.10.11"},
					{Name: "m2", IP: "192.168.10.12"},
					{Name: "m3", IP: "192.168.10.16"},
				},
				LBs: []config.NodeDetail{
					{Name: "lb1", IP: "192.168.20.103"},
					{Name: "lb2", IP: "192.168.20.104"},
				},
				CMSFrontends: []config.NodeDetail{{Name: "cms1", IP: "192.168.20.101"}},
			},
		}
		return cfg
	}

	t.Run("config.yaml uses a non-root deploy account and a known_hosts store", func(t *testing.T) {
		cfg, err := config.Load(filepath.Join("..", "config.yaml"))
		if err != nil {
			t.Fatalf("load config.yaml: %v", err)
		}
		if got := cfg.SSHUser(); got != "ubuntu" {
			t.Errorf("SSHUser() = %q, want ubuntu", got)
		}
		if kh := cfg.SSHKnownHostsFile(); !strings.HasSuffix(kh, "cms_known_hosts") {
			t.Errorf("SSHKnownHostsFile() = %q, want a cms_known_hosts path", kh)
		}
	})

	t.Run("empty ssh.user falls back to root for backwards compatibility", func(t *testing.T) {
		cfg := &config.Config{}
		if got := cfg.SSHUser(); got != "root" {
			t.Errorf("SSHUser() = %q, want root", got)
		}
	})

	t.Run("validation rejects an invalid ssh.user", func(t *testing.T) {
		cfg := validCfg()
		cfg.SSH.User = "bad user; rm -rf /"
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for an invalid ssh.user")
		}
	})

	t.Run("validation accepts a valid ssh.user", func(t *testing.T) {
		cfg := validCfg()
		cfg.SSH.User = "cms-deploy"
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid ssh.user rejected: %v", err)
		}
	})
}

func TestConfig_HotdeskSpecsFollowMainCIDR(t *testing.T) {
	cfg := &config.Config{
		Network: config.NetworkConfig{Main: config.NetworkDetail{CIDR: "10.0.30.0/24"}},
		Nodes:   config.NodesConfig{Hotdesks: config.HotdesksConf{Count: 2, BaseIPOctet: 201}},
	}
	specs := cfg.HotdeskSpecs()
	if len(specs) != 2 {
		t.Fatalf("expected 2 hotdesk specs, got %d", len(specs))
	}
	if got, want := specs[0].IP, "10.0.30.201"; got != want {
		t.Errorf("hotdesk IP = %q, want %q (derived from network.main.cidr)", got, want)
	}
	if got, want := specs[1].IP, "10.0.30.202"; got != want {
		t.Errorf("hotdesk IP = %q, want %q (derived from network.main.cidr)", got, want)
	}
}

func TestConfig_PrefixFromCIDR(t *testing.T) {
	tests := []struct {
		cidr     string
		fallback string
		want     string
	}{
		{"192.168.30.0/24", "192.168.20.", "192.168.30."},
		{"10.0.30.0/24", "192.168.20.", "10.0.30."},
		{"", "192.168.20.", "192.168.20."},
		{"not-a-cidr", "192.168.20.", "192.168.20."},
	}
	for _, tt := range tests {
		if got := config.PrefixFromCIDR(tt.cidr, tt.fallback); got != tt.want {
			t.Errorf("PrefixFromCIDR(%q, %q) = %q, want %q", tt.cidr, tt.fallback, got, tt.want)
		}
	}
}
