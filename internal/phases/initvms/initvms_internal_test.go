package initvms

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"gopkg.in/yaml.v3"
)

func TestJumpstartImageMode(t *testing.T) {
	cases := []struct {
		name    string
		flag    string
		config  string
		want    string
		wantErr bool
	}{
		{name: "default is iso", want: "iso"},
		{name: "config cloud", config: "cloud", want: "cloud"},
		{name: "flag wins", flag: "iso", config: "cloud", want: "iso"},
		{name: "normalises case and spaces", flag: " Cloud ", want: "cloud"},
		{name: "rejects unknown values", flag: "pxe", config: "iso", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Phase{
				cfg:  &config.Config{VM: config.VMConfig{JumpstartImage: tc.config}},
				opts: Options{JumpstartImage: tc.flag},
			}
			got, err := p.jumpstartImageMode()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("jumpstartImageMode() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("jumpstartImageMode(): %v", err)
			}
			if got != tc.want {
				t.Errorf("jumpstartImageMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

// sparseImage creates a file of the given size without allocating it, so the
// >100 MB cloud-image check can be exercised cheaply.
func sparseImage(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		t.Fatalf("truncate %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

func TestFindCloudImage(t *testing.T) {
	t.Run("missing storage image explains how to get it", func(t *testing.T) {
		_, err := findCloudImage(t.TempDir(), "")
		if err == nil {
			t.Fatal("expected an error when no cloud image exists")
		}
		if !strings.Contains(err.Error(), "cloud-images.ubuntu.com") {
			t.Errorf("error %q should point at the download URL", err)
		}
	})

	t.Run("finds the storage candidate", func(t *testing.T) {
		dir := t.TempDir()
		want := filepath.Join(dir, "ubuntu-24.04-server-cloudimg-amd64.img")
		sparseImage(t, want, 101<<20)
		got, err := findCloudImage(dir, "")
		if err != nil {
			t.Fatalf("findCloudImage: %v", err)
		}
		if got != want {
			t.Errorf("findCloudImage() = %q, want %q", got, want)
		}
	})

	t.Run("configured path wins", func(t *testing.T) {
		dir := t.TempDir()
		configured := filepath.Join(t.TempDir(), "custom-cloud.img")
		sparseImage(t, configured, 101<<20)
		// A differently named file in storage_dir must not shadow the config.
		sparseImage(t, filepath.Join(dir, "ubuntu-24.04-server-cloudimg-amd64.img"), 101<<20)
		got, err := findCloudImage(dir, configured)
		if err != nil {
			t.Fatalf("findCloudImage: %v", err)
		}
		if got != configured {
			t.Errorf("findCloudImage() = %q, want %q", got, configured)
		}
	})

	t.Run("configured path must exist", func(t *testing.T) {
		_, err := findCloudImage(t.TempDir(), filepath.Join(t.TempDir(), "nope.img"))
		if err == nil {
			t.Fatal("expected an error for a missing configured image")
		}
	})
}

func TestRenderISOUserDataUsesTheConfiguredDeployUser(t *testing.T) {
	data := autoinstallData{
		Hostname:        "jumpstart",
		DeployUser:      "deploy",
		PasswordHash:    "$6$rounds=4096$salt$hash",
		PubKey:          "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA test@host",
		MACInternal:     "52:54:00:10:00:01",
		IPInternal:      "192.168.10.10",
		InternalPrefix:  24,
		GatewayInternal: "192.168.10.1",
		MACMain:         "52:54:00:10:02:0a",
		IPMain:          "192.168.20.10",
		MainPrefix:      24,
	}

	out, err := renderTemplate("templates/autoinstall/user-data.tmpl", data)
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}
	for _, want := range []string{
		"username: deploy",
		"/target/etc/sudoers.d/deploy",
		"Defaults:deploy !requiretty",
		"/target/home/deploy/.ssh/authorized_keys",
		"chown -R deploy:deploy /home/deploy/.ssh",
		"passwd -l deploy",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("rendered ISO user-data is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "ubuntu") {
		t.Errorf("rendered ISO user-data still hardcodes the ubuntu account:\n%s", out)
	}
}

func TestRenderCloudUserData(t *testing.T) {
	data := autoinstallData{
		Hostname:            "jumpstart",
		DeployUser:          "deploy",
		PasswordHash:        "$6$rounds=4096$salt$hash",
		PubKey:              "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA test@host",
		MACInternal:         "52:54:00:10:00:01",
		IPInternal:          "192.168.10.10",
		InternalPrefix:      24,
		GatewayInternal:     "192.168.10.1",
		MACMain:             "52:54:00:10:02:0a",
		IPMain:              "192.168.20.10",
		MainPrefix:          24,
		WANIfaceConfigCloud: "\n  net2:\n    match:\n      macaddress: \"52:54:00:10:00:09\"\n    dhcp4: true",
	}

	out, err := renderTemplate("templates/autoinstall/cloud-user-data.tmpl", data)
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rendered cloud user-data is not valid YAML: %v\n%s", err, out)
	}

	for _, want := range []string{
		"#cloud-config",
		"hostname: jumpstart",
		"name: deploy",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA test@host",
		"ALL=(ALL) NOPASSWD:ALL",
		"Defaults:deploy !requiretty",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("rendered user-data is missing %q", want)
		}
	}
	if strings.Contains(string(out), "name: ubuntu") {
		t.Errorf("cloud user-data ignores ssh.user and still creates ubuntu:\n%s", out)
	}
	if strings.Contains(string(out), "autoinstall:") {
		t.Error("cloud image user-data must not contain the Subiquity autoinstall block")
	}
	// cloud-init does not read network configuration from user-data. Keeping it
	// here would silently fall back to DHCP and break the static topology.
	if _, ok := doc["network"]; ok {
		t.Errorf("cloud user-data must not contain a network block:\n%s", out)
	}
	if doc["hostname"] != "jumpstart" {
		t.Errorf("parsed hostname = %v, want jumpstart", doc["hostname"])
	}

	netOut, err := renderTemplate("templates/autoinstall/cloud-network-config.tmpl", data)
	if err != nil {
		t.Fatalf("render network-config: %v", err)
	}
	var netDoc struct {
		Version   int `yaml:"version"`
		Ethernets map[string]struct {
			Match struct {
				MAC string `yaml:"macaddress"`
			} `yaml:"match"`
			Addresses []string `yaml:"addresses"`
		} `yaml:"ethernets"`
	}
	if err := yaml.Unmarshal(netOut, &netDoc); err != nil {
		t.Fatalf("rendered cloud network-config is not valid YAML: %v\n%s", err, netOut)
	}
	if netDoc.Version != 2 {
		t.Errorf("network-config version = %d, want 2", netDoc.Version)
	}
	for name, mac := range map[string]string{
		"net0": "52:54:00:10:00:01",
		"net1": "52:54:00:10:02:0a",
		"net2": "52:54:00:10:00:09",
	} {
		iface, ok := netDoc.Ethernets[name]
		if !ok {
			t.Errorf("network-config is missing %s", name)
			continue
		}
		if iface.Match.MAC != mac {
			t.Errorf("%s MAC = %q, want %q", name, iface.Match.MAC, mac)
		}
	}
	if got := netDoc.Ethernets["net0"].Addresses; len(got) != 1 || got[0] != "192.168.10.10/24" {
		t.Errorf("net0 addresses = %v, want [192.168.10.10/24]", got)
	}
	if got := netDoc.Ethernets["net1"].Addresses; len(got) != 1 || got[0] != "192.168.20.10/24" {
		t.Errorf("net1 addresses = %v, want [192.168.20.10/24]", got)
	}
}

func TestClientProvisioningMode(t *testing.T) {
	cases := []struct {
		name    string
		flag    bool
		config  string
		want    string
		wantErr bool
	}{
		{name: "default is pxe", want: "pxe"},
		{name: "config cloud", config: "cloud", want: "cloud"},
		{name: "flag wins", flag: true, config: "pxe", want: "cloud"},
		{name: "rejects unknown values", config: "fast", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Phase{
				cfg:  &config.Config{VM: config.VMConfig{Provisioning: tc.config}},
				opts: Options{FastProvisioning: tc.flag},
			}
			got, err := p.clientProvisioningMode()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("clientProvisioningMode: %v", err)
			}
			if got != tc.want {
				t.Errorf("mode = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCloudInterfaces(t *testing.T) {
	cfg := &config.Config{
		Network: config.NetworkConfig{
			Internal: config.NetworkDetail{CIDR: "192.168.10.0/24", Gateway: "192.168.10.1"},
			Main:     config.NetworkDetail{CIDR: "192.168.20.0/24", Gateway: "192.168.20.1"},
		},
		Nodes: config.NodesConfig{
			Router:    config.NodeDetail{Name: "router", IP: "192.168.10.1"},
			Jumpstart: config.NodeDetail{IP: "192.168.10.10", IPMain: "192.168.20.10"},
		},
	}

	t.Run("internal node", func(t *testing.T) {
		got, err := cloudInterfaces(cfg,
			config.NodeDetail{Name: "master1", IP: "192.168.10.11"},
			[]libvirt.NetworkOpt{{Network: "internal", Mac: "52:54:00:10:01:11"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("interfaces = %d, want 1", len(got))
		}
		want := cloudInterface{Name: "net0", MAC: "52:54:00:10:01:11", IP: "192.168.10.11", Prefix: 24, Gateway: "192.168.10.1", DNS: "192.168.10.10", Metric: 200}
		if got[0] != want {
			t.Errorf("interface = %+v, want %+v", got[0], want)
		}
	})

	t.Run("main node", func(t *testing.T) {
		got, err := cloudInterfaces(cfg,
			config.NodeDetail{Name: "lb1", IP: "192.168.20.103"},
			[]libvirt.NetworkOpt{{Network: "main", Mac: "52:54:00:10:02:67"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("interfaces = %d, want 1", len(got))
		}
		want := cloudInterface{Name: "net0", MAC: "52:54:00:10:02:67", IP: "192.168.20.103", Prefix: 24, Gateway: "192.168.20.1", DNS: "192.168.20.10", Metric: 200}
		if got[0] != want {
			t.Errorf("interface = %+v, want %+v", got[0], want)
		}
	})

	t.Run("router with WAN", func(t *testing.T) {
		got, err := cloudInterfaces(cfg,
			config.NodeDetail{Name: "router", IP: "192.168.10.1"},
			[]libvirt.NetworkOpt{
				{Network: "internal", Mac: "52:54:00:10:01:02"},
				{Network: "main", Mac: "52:54:00:10:02:02"},
				{Network: "default", Mac: "52:54:00:10:00:02"},
			})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("interfaces = %d, want 3", len(got))
		}
		if got[0].IP != "192.168.10.1" || got[1].IP != "192.168.20.1" {
			t.Errorf("router addresses = %q/%q, want the two gateways", got[0].IP, got[1].IP)
		}
		if got[0].Gateway != "" || got[1].Gateway != "" {
			t.Errorf("router gateways = %q/%q, want no default route via itself", got[0].Gateway, got[1].Gateway)
		}
		if !got[2].DHCP || got[2].Metric != 100 {
			t.Errorf("WAN interface = %+v, want DHCP with metric 100", got[2])
		}
	})

	t.Run("non-/24 networks keep their prefix", func(t *testing.T) {
		cfg := *cfg
		cfg.Network.Internal = config.NetworkDetail{CIDR: "192.168.10.0/25", Gateway: "192.168.10.1"}
		cfg.Network.Main = config.NetworkDetail{CIDR: "192.168.20.0/26", Gateway: "192.168.20.1"}
		got, err := cloudInterfaces(&cfg,
			config.NodeDetail{Name: "router", IP: "192.168.10.1"},
			[]libvirt.NetworkOpt{
				{Network: "internal", Mac: "52:54:00:10:01:02"},
				{Network: "main", Mac: "52:54:00:10:02:02"},
			})
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Prefix != 25 || got[1].Prefix != 26 {
			t.Fatalf("prefixes = %d/%d, want 25/26", got[0].Prefix, got[1].Prefix)
		}
	})
}

func TestRenderCloudNodeUserData(t *testing.T) {
	data := cloudNodeData{
		Hostname:     "internal-master1",
		DeployUser:   "deploy",
		PasswordHash: "$6$rounds=4096$salt$hash",
		PubKey:       "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA test@host",
		Interfaces: []cloudInterface{
			{Name: "net0", MAC: "52:54:00:10:01:11", IP: "192.168.10.11", Prefix: 24, Gateway: "192.168.10.1", DNS: "192.168.10.10", Metric: 200},
		},
	}

	out, err := renderTemplate("templates/autoinstall/cloud-node-user-data.tmpl", data)
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rendered cloud-node user-data is not valid YAML: %v\n%s", err, out)
	}
	for _, want := range []string{
		"#cloud-config",
		"hostname: internal-master1",
		"name: deploy",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA test@host",
		"Defaults:deploy !requiretty",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("rendered user-data does not contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "name: ubuntu") {
		t.Errorf("cloud-node user-data ignores ssh.user and still creates ubuntu:\n%s", out)
	}
	if _, ok := doc["network"]; ok {
		t.Errorf("cloud-node user-data must not contain a network block:\n%s", out)
	}

	netOut, err := renderTemplate("templates/autoinstall/cloud-node-network-config.tmpl", data)
	if err != nil {
		t.Fatalf("render network-config: %v", err)
	}
	var netDoc struct {
		Version   int `yaml:"version"`
		Ethernets map[string]struct {
			Match struct {
				MAC string `yaml:"macaddress"`
			} `yaml:"match"`
			Addresses []string `yaml:"addresses"`
			Routes    []struct {
				To     string `yaml:"to"`
				Via    string `yaml:"via"`
				Metric int    `yaml:"metric"`
			} `yaml:"routes"`
		} `yaml:"ethernets"`
	}
	if err := yaml.Unmarshal(netOut, &netDoc); err != nil {
		t.Fatalf("rendered cloud-node network-config is not valid YAML: %v\n%s", err, netOut)
	}
	iface, ok := netDoc.Ethernets["net0"]
	if !ok {
		t.Fatalf("network-config is missing net0:\n%s", netOut)
	}
	if iface.Match.MAC != "52:54:00:10:01:11" {
		t.Errorf("net0 MAC = %q, want 52:54:00:10:01:11", iface.Match.MAC)
	}
	if len(iface.Addresses) != 1 || iface.Addresses[0] != "192.168.10.11/24" {
		t.Errorf("net0 addresses = %v, want [192.168.10.11/24]", iface.Addresses)
	}
	if len(iface.Routes) != 1 || iface.Routes[0].To != "default" || iface.Routes[0].Via != "192.168.10.1" || iface.Routes[0].Metric != 200 {
		t.Errorf("net0 routes = %+v, want one default route via 192.168.10.1 metric 200", iface.Routes)
	}
}

func TestRenderCloudNetworkConfigKeepsNon24Prefixes(t *testing.T) {
	t.Run("client node", func(t *testing.T) {
		data := cloudNodeData{
			Hostname: "internal-master1",
			Interfaces: []cloudInterface{
				{Name: "net0", MAC: "52:54:00:10:01:11", IP: "192.168.10.11", Prefix: 25, Gateway: "192.168.10.1", DNS: "192.168.10.10", Metric: 200},
			},
		}
		out, err := renderTemplate("templates/autoinstall/cloud-node-network-config.tmpl", data)
		if err != nil {
			t.Fatalf("renderTemplate: %v", err)
		}
		var doc struct {
			Ethernets map[string]struct {
				Addresses []string `yaml:"addresses"`
			} `yaml:"ethernets"`
		}
		if err := yaml.Unmarshal(out, &doc); err != nil {
			t.Fatalf("rendered network-config is not valid YAML: %v\n%s", err, out)
		}
		if got := doc.Ethernets["net0"].Addresses; len(got) != 1 || got[0] != "192.168.10.11/25" {
			t.Errorf("net0 addresses = %v, want [192.168.10.11/25]", got)
		}
	})

	t.Run("jumpstart", func(t *testing.T) {
		data := autoinstallData{
			MACInternal:    "52:54:00:10:00:01",
			IPInternal:     "192.168.10.10",
			InternalPrefix: 25,
			MACMain:        "52:54:00:10:02:0a",
			IPMain:         "192.168.20.10",
			MainPrefix:     26,
		}
		out, err := renderTemplate("templates/autoinstall/cloud-network-config.tmpl", data)
		if err != nil {
			t.Fatalf("renderTemplate: %v", err)
		}
		var doc struct {
			Ethernets map[string]struct {
				Addresses []string `yaml:"addresses"`
			} `yaml:"ethernets"`
		}
		if err := yaml.Unmarshal(out, &doc); err != nil {
			t.Fatalf("rendered network-config is not valid YAML: %v\n%s", err, out)
		}
		if got := doc.Ethernets["net0"].Addresses; len(got) != 1 || got[0] != "192.168.10.10/25" {
			t.Errorf("net0 addresses = %v, want [192.168.10.10/25]", got)
		}
		if got := doc.Ethernets["net1"].Addresses; len(got) != 1 || got[0] != "192.168.20.10/26" {
			t.Errorf("net1 addresses = %v, want [192.168.20.10/26]", got)
		}
	})
}

func TestRenderCloudNodeUserDataShapes(t *testing.T) {
	cases := map[string][]cloudInterface{
		"internal": {
			{Name: "net0", MAC: "52:54:00:10:01:11", IP: "192.168.10.11", Gateway: "192.168.10.1", DNS: "192.168.10.10", Metric: 200},
		},
		"main": {
			{Name: "net0", MAC: "52:54:00:10:02:67", IP: "192.168.20.103", Gateway: "192.168.20.1", DNS: "192.168.20.10", Metric: 200},
		},
		"router": {
			{Name: "net0", MAC: "52:54:00:10:01:02", IP: "192.168.10.1", DNS: "192.168.10.10", Metric: 200},
			{Name: "net1", MAC: "52:54:00:10:02:02", IP: "192.168.20.1", DNS: "192.168.20.10", Metric: 200},
			{Name: "net2", MAC: "52:54:00:10:00:02", DHCP: true, Metric: 100},
		},
	}
	for name, ifaces := range cases {
		t.Run(name, func(t *testing.T) {
			data := cloudNodeData{
				Hostname:     name,
				DeployUser:   "deploy",
				PasswordHash: "$6$rounds=4096$salt$hash",
				PubKey:       "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA test@host",
				Interfaces:   ifaces,
			}
			out, err := renderTemplate("templates/autoinstall/cloud-node-user-data.tmpl", data)
			if err != nil {
				t.Fatalf("renderTemplate: %v", err)
			}
			var doc map[string]any
			if err := yaml.Unmarshal(out, &doc); err != nil {
				t.Fatalf("rendered user-data is not valid YAML: %v\n%s", err, out)
			}
			if _, ok := doc["network"]; ok {
				t.Errorf("cloud-node user-data must not contain a network block:\n%s", out)
			}

			netOut, err := renderTemplate("templates/autoinstall/cloud-node-network-config.tmpl", data)
			if err != nil {
				t.Fatalf("render network-config: %v", err)
			}
			var netDoc struct {
				Version   int `yaml:"version"`
				Ethernets map[string]struct {
					Match struct {
						MAC string `yaml:"macaddress"`
					} `yaml:"match"`
					Addresses []string `yaml:"addresses"`
					DHCP4     bool     `yaml:"dhcp4"`
					Routes    []struct {
						To  string `yaml:"to"`
						Via string `yaml:"via"`
					} `yaml:"routes"`
				} `yaml:"ethernets"`
			}
			if err := yaml.Unmarshal(netOut, &netDoc); err != nil {
				t.Fatalf("rendered network-config is not valid YAML: %v\n%s", err, netOut)
			}
			if netDoc.Version != 2 {
				t.Errorf("network-config version = %d, want 2", netDoc.Version)
			}
			if len(netDoc.Ethernets) != len(ifaces) {
				t.Errorf("network-config has %d interfaces, want %d:\n%s", len(netDoc.Ethernets), len(ifaces), netOut)
			}
			routes := 0
			for _, iface := range ifaces {
				got, ok := netDoc.Ethernets[iface.Name]
				if !ok {
					t.Errorf("network-config is missing %s:\n%s", iface.Name, netOut)
					continue
				}
				if got.Match.MAC != iface.MAC {
					t.Errorf("%s MAC = %q, want %q", iface.Name, got.Match.MAC, iface.MAC)
				}
				if iface.DHCP != got.DHCP4 {
					t.Errorf("%s dhcp4 = %v, want %v", iface.Name, got.DHCP4, iface.DHCP)
				}
				routes += len(got.Routes)
			}
			if routes > 1 {
				t.Errorf("network-config has %d default routes, want at most one:\n%s", routes, netOut)
			}
		})
	}
}

func TestRenderNetworkXMLUsesConfiguredBridgeAndDomain(t *testing.T) {
	tmpl, err := cms.TemplatesFS.ReadFile("templates/libvirt/internal-net.xml")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	out, err := renderNetworkXML(tmpl, "br-lab0", "lab.internal")
	if err != nil {
		t.Fatalf("renderNetworkXML: %v", err)
	}
	for _, want := range []string{
		"<bridge name='br-lab0' stp='on' delay='0'/>",
		"<domain name='lab.internal'/>",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("rendered XML is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "{{") {
		t.Errorf("rendered XML still has placeholders:\n%s", out)
	}
}

func TestVerifyNetworkDefinitionRejectsBridgeAndDomainDrift(t *testing.T) {
	const matching = `<network>
  <name>internal</name>
  <bridge name='virbr-int' stp='on' delay='0'/>
  <domain name='internal.local'/>
</network>`
	cases := []struct {
		name       string
		definition string
		bridge     string
		domain     string
		wantErr    string
	}{
		{name: "matching", definition: matching, bridge: "virbr-int", domain: "internal.local"},
		{name: "bridge drift", definition: matching, bridge: "virbr-other", domain: "internal.local", wantErr: "uses bridge"},
		{name: "domain drift", definition: matching, bridge: "virbr-int", domain: "lab.internal", wantErr: "uses DNS domain"},
		{name: "missing bridge", definition: `<network><domain name='internal.local'/></network>`, bridge: "virbr-int", domain: "internal.local", wantErr: "has no bridge"},
		{name: "missing domain", definition: `<network><bridge name='virbr-int'/></network>`, bridge: "virbr-int", domain: "internal.local", wantErr: "has no DNS domain"},
		{name: "malformed XML", definition: `<network>`, bridge: "virbr-int", domain: "internal.local", wantErr: "parse existing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyNetworkDefinition("internal", []byte(tc.definition), tc.bridge, tc.domain)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("verifyNetworkDefinition() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("verifyNetworkDefinition() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}
