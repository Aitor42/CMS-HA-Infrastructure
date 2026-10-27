package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExpandPath(t *testing.T) {
	t.Setenv("CMS_HA_TEST_DIR", "/tmp/cms-ha")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home: %v", err)
	}

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "environment", in: "$CMS_HA_TEST_DIR/vms", want: filepath.Clean("/tmp/cms-ha/vms")},
		{name: "tilde", in: "~/cms-ha", want: filepath.Join(home, "cms-ha")},
		{name: "plain", in: "/var/lib/cms-ha", want: "/var/lib/cms-ha"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExpandPath(tt.in); got != tt.want {
				t.Fatalf("ExpandPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestModeDefaultsAndNormalisation(t *testing.T) {
	c := &Config{}
	if got := c.JumpstartImageMode(); got != "iso" {
		t.Fatalf("default jumpstart mode = %q, want iso", got)
	}
	if got := c.ClientProvisioningMode(); got != "pxe" {
		t.Fatalf("default provisioning mode = %q, want pxe", got)
	}

	c.VM.JumpstartImage = " CLOUD "
	c.VM.Provisioning = " PXE "
	if got := c.JumpstartImageMode(); got != "cloud" {
		t.Fatalf("jumpstart mode = %q, want cloud", got)
	}
	if got := c.ClientProvisioningMode(); got != "pxe" {
		t.Fatalf("provisioning mode = %q, want pxe", got)
	}
}

func TestSSHAccessors(t *testing.T) {
	c := &Config{}
	if got := c.SSHUser(); got != "root" {
		t.Fatalf("default SSH user = %q, want root", got)
	}
	if got := c.SSHUser(); got != "root" {
		t.Fatalf("default SSH user = %q, want root", got)
	}

	c.SSH.User = "ubuntu"
	if got := c.SSHUser(); got != "ubuntu" {
		t.Fatalf("SSH user = %q, want ubuntu", got)
	}
	if got := c.SSHKnownHostsFile(); got == "" {
		t.Fatal("default known-hosts path is empty")
	}
	c.SSH.KnownHostsFile = "~/known_hosts"
	home, _ := os.UserHomeDir()
	if got := c.SSHKnownHostsFile(); got != filepath.Join(home, "known_hosts") {
		t.Fatalf("known-hosts path = %q", got)
	}
}

func TestRuntimeRAMMB(t *testing.T) {
	if got := (NodeDetail{RAMInstallMB: 4096}).RuntimeRAMMB(); got != 4096 {
		t.Fatalf("runtime RAM fallback = %d, want 4096", got)
	}
	if got := (NodeDetail{RAMInstallMB: 4096, RAMFinalMB: 2048}).RuntimeRAMMB(); got != 2048 {
		t.Fatalf("runtime RAM = %d, want 2048", got)
	}
}

func TestPKIAccessors(t *testing.T) {
	c := &Config{}
	if got := c.PKICommonName(); got != "CMS Local CA" {
		t.Fatalf("default CA name = %q", got)
	}
	if got := c.LBTLSDNSNames(); !reflect.DeepEqual(got, []string{"cms.fake-enterprise.com"}) {
		t.Fatalf("default DNS names = %#v", got)
	}
	if got := c.LBTLSCertMinValidDays(); got != 30 {
		t.Fatalf("default minimum validity = %d, want 30", got)
	}
	if got := c.LBTLSCertLifetimeHours(); got != 2160 {
		t.Fatalf("default lifetime = %d, want 2160", got)
	}

	c.PKI.CAName = "Lab CA"
	c.PKI.CertDNSNames = []string{"cms.example.test", "www.example.test"}
	c.PKI.CertMinValidDays = 10
	c.PKI.CertLifetimeHours = 240
	c.Network.Main.VIP = "192.168.20.100"
	c.Nodes.LBs = []NodeDetail{{IP: "192.168.20.11"}, {IP: ""}, {IP: "192.168.20.12"}}
	if got := c.PKICommonName(); got != "Lab CA" {
		t.Fatalf("CA name = %q", got)
	}
	wantSANs := []string{"cms.example.test", "www.example.test", "192.168.20.100", "192.168.20.11", "192.168.20.12"}
	if got := c.LBTLSSANs(); !reflect.DeepEqual(got, wantSANs) {
		t.Fatalf("SANs = %#v, want %#v", got, wantSANs)
	}
	if got := c.LBTLSCertMinValidDays(); got != 10 {
		t.Fatalf("minimum validity = %d, want 10", got)
	}
	if got := c.LBTLSCertLifetimeHours(); got != 240 {
		t.Fatalf("lifetime = %d, want 240", got)
	}
}

func TestTrafficAndBackupAccessors(t *testing.T) {
	c := &Config{}
	if got := c.K6Image(); got != "grafana/k6:0.55.0" {
		t.Fatalf("default k6 image = %q", got)
	}
	if got := c.TrafficVHost(); got != "cms.fake-enterprise.com" {
		t.Fatalf("default traffic vhost = %q", got)
	}
	if c.BackupEnabled() {
		t.Fatal("backup should be disabled with an empty engine")
	}
	if got := c.BackupEngine(); got != "garage" {
		t.Fatalf("default backup engine = %q", got)
	}
	if got := c.GarageImage(); got != "dxflrs/garage:v2.4.1" {
		t.Fatalf("default garage image = %q", got)
	}
	if got := c.BackupDataDir(); got != "/var/lib/cms-garage" {
		t.Fatalf("default backup dir = %q", got)
	}
	if got := c.BackupS3Port(); got != 3900 {
		t.Fatalf("default S3 port = %d", got)
	}
	if got := c.BackupS3Region(); got != "garage" {
		t.Fatalf("default S3 region = %q", got)
	}
	if got := c.BackupBucket(); got != "cms-backups" {
		t.Fatalf("default bucket = %q", got)
	}
	if got := c.BackupAccessKey(); got != "GKcmshabackup0000000000" {
		t.Fatalf("default access key = %q", got)
	}
	if got := c.BackupRetentionDays(); got != 7 {
		t.Fatalf("default retention = %d", got)
	}
	if got := c.BackupSchedule(); got != "0 2 * * *" {
		t.Fatalf("default schedule = %q", got)
	}
	if got := c.BackupRestoreTestSchedule(); got != "30 3 * * *" {
		t.Fatalf("default restore schedule = %q", got)
	}

	c.Traffic.K6Version = "1.2.3"
	c.Traffic.VHost = "cms.test"
	c.Backup.Engine = "garage"
	c.Backup.GarageVersion = "v9.9.9"
	c.Backup.DataDir = "~/garage"
	c.Backup.S3Port = 9000
	c.Backup.S3Region = "test-region"
	c.Backup.Bucket = "test-bucket"
	c.Backup.AccessKey = "test-key"
	c.Backup.RetentionDays = 14
	c.Backup.Schedule = "0 1 * * *"
	c.Backup.RestoreTestSchedule = "0 2 * * *"
	c.Network.Internal.CIDR = "192.168.10.0/24"
	home, _ := os.UserHomeDir()
	if got := c.K6Image(); got != "grafana/k6:1.2.3" {
		t.Fatalf("k6 image = %q", got)
	}
	if got := c.TrafficVHost(); got != "cms.test" {
		t.Fatalf("traffic vhost = %q", got)
	}
	if !c.BackupEnabled() {
		t.Fatal("backup should be enabled with an engine")
	}
	if got := c.GarageImage(); got != "dxflrs/garage:v9.9.9" {
		t.Fatalf("garage image = %q", got)
	}
	if got := c.BackupDataDir(); got != filepath.Join(home, "garage") {
		t.Fatalf("backup dir = %q", got)
	}
	if got := c.BackupS3Endpoint(); got != "http://192.168.10.254:9000" {
		t.Fatalf("S3 endpoint = %q", got)
	}
	if got := c.BackupS3Port(); got != 9000 {
		t.Fatalf("S3 port = %d", got)
	}
	if got := c.BackupS3Region(); got != "test-region" {
		t.Fatalf("S3 region = %q", got)
	}
	if got := c.BackupBucket(); got != "test-bucket" {
		t.Fatalf("bucket = %q", got)
	}
	if got := c.BackupAccessKey(); got != "test-key" {
		t.Fatalf("access key = %q", got)
	}
	if got := c.BackupRetentionDays(); got != 14 {
		t.Fatalf("retention = %d", got)
	}
	if got := c.BackupSchedule(); got != "0 1 * * *" {
		t.Fatalf("schedule = %q", got)
	}
	if got := c.BackupRestoreTestSchedule(); got != "0 2 * * *" {
		t.Fatalf("restore schedule = %q", got)
	}
}

func TestNodeSpecsAndNetworks(t *testing.T) {
	c := &Config{
		Network: NetworkConfig{
			Internal: NetworkDetail{CIDR: "192.168.10.0/24"},
			Main:     NetworkDetail{CIDR: "192.168.20.0/24", VIP: "192.168.20.100"},
		},
		Nodes: NodesConfig{
			Router:    NodeDetail{Name: "router", IP: "192.168.10.1"},
			Jumpstart: NodeDetail{Name: "jumpstart", IP: "192.168.10.10", IPMain: "192.168.20.10", MACMain: "52:54:00:10:02:0a"},
			Masters:   []NodeDetail{{Name: "master1", IP: "192.168.10.21"}},
			Workers:   []NodeDetail{{Name: "worker1", IP: "192.168.10.31"}},
			Storage:   NodeDetail{Name: "storage", IP: "192.168.10.40"},
			Monitor:   NodeDetail{Name: "monitor", IP: "192.168.10.50"},
			LBs:       []NodeDetail{{Name: "main-lb1", IP: "192.168.20.11"}},
			CMSFrontends: []NodeDetail{
				{Name: "main-cms1", IP: "192.168.20.21"},
			},
			Hotdesks: HotdesksConf{Count: 2, BaseIPOctet: 210, RAMInstallMB: 4096, RAMFinalMB: 2048, VCPUs: 2, DiskGB: 20},
		},
	}

	if got := c.HypervisorIP(); got != "192.168.10.254" {
		t.Fatalf("hypervisor IP = %q", got)
	}
	if got := c.LBVIP(); got != "192.168.20.100" {
		t.Fatalf("LB VIP = %q", got)
	}
	if got := c.PrimaryLB().Name; got != "main-lb1" {
		t.Fatalf("primary LB = %q", got)
	}
	if got, ok := c.FirstMaster(); !ok || got.Name != "master1" {
		t.Fatalf("first master = %#v, %v", got, ok)
	}

	internal := c.InternalNodes()
	if len(internal) != 6 {
		t.Fatalf("internal nodes = %d, want 6", len(internal))
	}
	main := c.MainNodes()
	if len(main) != 5 {
		t.Fatalf("main nodes = %d, want 5", len(main))
	}
	if main[0].MAC != "52:54:00:10:02:0a" {
		t.Fatalf("jumpstart main MAC = %q", main[0].MAC)
	}
	all := c.AllNodes()
	if len(all) != 10 {
		t.Fatalf("all nodes = %d, want 10", len(all))
	}
	ips := c.AllNodeIPs()
	if len(ips) != 11 {
		t.Fatalf("all node IPs = %d, want 11", len(ips))
	}

	hotdesks := c.HotdeskSpecs()
	if len(hotdesks) != 2 {
		t.Fatalf("hotdesks = %d, want 2", len(hotdesks))
	}
	if hotdesks[0].IP != "192.168.20.210" || hotdesks[1].IP != "192.168.20.211" {
		t.Fatalf("hotdesk IPs = %q, %q", hotdesks[0].IP, hotdesks[1].IP)
	}
	if hotdesks[0].MAC != "52:54:00:10:02:d2" || hotdesks[1].MAC != "52:54:00:10:02:d3" {
		t.Fatalf("hotdesk MACs = %q, %q", hotdesks[0].MAC, hotdesks[1].MAC)
	}
}

func TestPrefixFromCIDRAndMACValidation(t *testing.T) {
	if got := PrefixFromCIDR("192.168.20.0/24", "fallback."); got != "192.168.20." {
		t.Fatalf("prefix = %q", got)
	}
	if got := PrefixFromCIDR("bad", "fallback."); got != "fallback." {
		t.Fatalf("fallback prefix = %q", got)
	}
	for _, mac := range []string{"52:54:00:aa:bb:cc", "AA:BB:CC:DD:EE:FF"} {
		if !ValidMAC(mac) {
			t.Fatalf("ValidMAC(%q) = false", mac)
		}
	}
	for _, mac := range []string{"", "52-54-00-aa-bb-cc", "not-a-mac"} {
		if ValidMAC(mac) {
			t.Fatalf("ValidMAC(%q) = true", mac)
		}
	}
}

func TestFenceCommands(t *testing.T) {
	t.Setenv("USER", "tester")
	c := &Config{Network: NetworkConfig{Internal: NetworkDetail{CIDR: "192.168.10.0/24"}}}
	off, state := c.FenceCommands()
	if off != "ssh -i /etc/drbd-fence/fence_key -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=5 tester@192.168.10.254 sudo virsh destroy {domain}" {
		t.Fatalf("default fence command = %q", off)
	}
	if state != "ssh -i /etc/drbd-fence/fence_key -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=5 tester@192.168.10.254 sudo virsh domstate {domain}" {
		t.Fatalf("default fence state command = %q", state)
	}

	c.DRBD.FenceCommand = "poweroff {domain}"
	c.DRBD.FenceStateCommand = "status {domain}"
	if gotOff, gotState := c.FenceCommands(); gotOff != "poweroff {domain}" || gotState != "status {domain}" {
		t.Fatalf("configured fence commands = %q, %q", gotOff, gotState)
	}
}

func TestLoadExpandsPaths(t *testing.T) {
	t.Setenv("CMS_HA_TEST_DIR", "/tmp/cms-ha-load")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := []byte("vm:\n  storage_dir: $CMS_HA_TEST_DIR/vms\nssh:\n  private_key: ~/.ssh/id_ed25519\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.VM.StorageDir != filepath.Clean("/tmp/cms-ha-load/vms") {
		t.Fatalf("storage dir = %q", cfg.VM.StorageDir)
	}
	home, _ := os.UserHomeDir()
	if cfg.SSH.PrivateKey != filepath.Join(home, ".ssh/id_ed25519") {
		t.Fatalf("private key = %q", cfg.SSH.PrivateKey)
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("Load(missing) succeeded")
	}
}

func TestSecretsRoundTrip(t *testing.T) {
	publicKey, privateKey, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	encrypted, err := EncryptValue("correct horse battery staple", publicKey)
	if err != nil {
		t.Fatalf("EncryptValue: %v", err)
	}
	if !IsEncrypted(encrypted) {
		t.Fatal("encrypted value is not recognised as ENC[age,...]")
	}
	if IsEncrypted("plain text") {
		t.Fatal("plain text recognised as encrypted")
	}
	keyPath := filepath.Join(t.TempDir(), "age.key")
	if err := os.WriteFile(keyPath, []byte(privateKey), 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	got, err := DecryptValue(encrypted, keyPath)
	if err != nil {
		t.Fatalf("DecryptValue: %v", err)
	}
	if got != "correct horse battery staple" {
		t.Fatalf("decrypted value = %q", got)
	}
	if got, err := DecryptValue("plain text", keyPath); err != nil || got != "plain text" {
		t.Fatalf("plaintext DecryptValue = %q, %v", got, err)
	}

	insecureKey := filepath.Join(t.TempDir(), "insecure.key")
	if err := os.WriteFile(insecureKey, []byte(privateKey), 0600); err != nil {
		t.Fatalf("write insecure key: %v", err)
	}
	if err := os.Chmod(insecureKey, 0644); err != nil {
		t.Fatalf("chmod insecure key: %v", err)
	}
	if _, err := DecryptValue(encrypted, insecureKey); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("world-readable key was not rejected: %v", err)
	}
}

func TestRekeyConfig(t *testing.T) {
	oldPublic, oldPrivate, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey(old): %v", err)
	}
	newPublic, newPrivate, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey(new): %v", err)
	}
	if got, err := PublicKeyFromPrivate(newPrivate); err != nil || got != newPublic {
		t.Fatalf("PublicKeyFromPrivate = %q, %v; want %q", got, err, newPublic)
	}

	dir := t.TempDir()
	oldKeyPath := filepath.Join(dir, "old.key")
	newKeyPath := filepath.Join(dir, "new.key")
	if err := os.WriteFile(oldKeyPath, []byte(oldPrivate), 0600); err != nil {
		t.Fatalf("write old key: %v", err)
	}
	if err := os.WriteFile(newKeyPath, []byte(newPrivate), 0600); err != nil {
		t.Fatalf("write new key: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("database:\n  user: cms\n  password: db-secret\n"), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := RekeyConfig(configPath, oldKeyPath, newPublic); err != nil {
		t.Fatalf("RekeyConfig: %v", err)
	}
	rekeyed, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read rekeyed config: %v", err)
	}
	if strings.Contains(string(rekeyed), "db-secret") {
		t.Fatal("rekeyed config still contains the plaintext secret")
	}
	if !strings.Contains(string(rekeyed), "ENC[age,") {
		t.Fatal("rekeyed config does not contain an encrypted secret")
	}
	cfg, err := LoadWithKey(configPath, newKeyPath)
	if err != nil {
		t.Fatalf("load with new key: %v", err)
	}
	if cfg.Database.Password != "db-secret" {
		t.Fatalf("decrypted password = %q", cfg.Database.Password)
	}
	if cfg.Database.User != "cms" {
		t.Fatalf("non-secret field changed: %q", cfg.Database.User)
	}
	if _, err := LoadWithKey(configPath, oldKeyPath); err == nil {
		t.Fatal("old key still decrypts the rekeyed config")
	}

	// A failed rekey must not touch the file.
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read before failed rekey: %v", err)
	}
	wrongKey := filepath.Join(dir, "wrong.key")
	wrongPublic, wrongPrivate, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey(wrong): %v", err)
	}
	_ = wrongPublic
	if err := os.WriteFile(wrongKey, []byte(wrongPrivate), 0600); err != nil {
		t.Fatalf("write wrong key: %v", err)
	}
	if err := RekeyConfig(configPath, wrongKey, oldPublic); err == nil {
		t.Fatal("RekeyConfig succeeded with the wrong old key")
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read after failed rekey: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("failed rekey modified the config file")
	}
}
