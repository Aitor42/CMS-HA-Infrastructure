package tests

import (
	"fmt"
	"os"
	"strings"
	"testing"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"gopkg.in/yaml.v3"
)

func TestTemplates_EmbeddedNetworkXML(t *testing.T) {
	nets := []struct {
		file   string
		bridge string
		domain string
	}{
		{"templates/libvirt/internal-net.xml", "virbr-int", "internal.local"},
		{"templates/libvirt/main-net.xml", "virbr-main", "main.local"},
	}

	for _, n := range nets {
		t.Run(n.file, func(t *testing.T) {
			data, err := cms.TemplatesFS.ReadFile(n.file)
			if err != nil {
				t.Fatalf("failed to read embedded template %s: %v", n.file, err)
			}
			content := string(data)
			if !strings.Contains(content, "stp='on'") {
				t.Errorf("template %s must enable STP (stp='on') for loop prevention", n.file)
			}
			if !strings.Contains(content, fmt.Sprintf("bridge name='%s'", n.bridge)) {
				t.Errorf("template %s missing expected bridge %s", n.file, n.bridge)
			}
			if !strings.Contains(content, fmt.Sprintf("domain name='%s'", n.domain)) {
				t.Errorf("template %s missing expected domain %s", n.file, n.domain)
			}
		})
	}
}

func TestTemplates_EmbeddedCobblerAutoinstall(t *testing.T) {
	data, err := cms.TemplatesFS.ReadFile("templates/cobbler/ubuntu-24.04-autoinstall.yaml")
	if err != nil {
		t.Fatalf("failed to read embedded autoinstall template: %v", err)
	}

	content := string(data)
	if !strings.HasPrefix(content, "#cloud-config") {
		t.Errorf("autoinstall template must start with #cloud-config header")
	}
	expectedSections := []string{
		"autoinstall:",
		"identity:",
		"keyboard:",
		"late-commands:",
		"cblr/pub/authorized_keys",
		"nopxe",
	}
	for _, sec := range expectedSections {
		if !strings.Contains(content, sec) {
			t.Errorf("autoinstall template missing required directive: %s", sec)
		}
	}
}

func TestTemplates_EmbeddedMonitoringConfigs(t *testing.T) {
	t.Run("prometheus.yml", func(t *testing.T) {
		// Puppet (role::monitor) is the single source of truth for the scrape
		// configuration; the monitoring phase only deploys the alert rules.
		data, err := os.ReadFile(repoFile(t, "puppet/modules/role/files/prometheus/prometheus.yml"))
		if err != nil {
			t.Fatalf("failed to read prometheus.yml: %v", err)
		}
		var parsed map[string]interface{}
		if err := yaml.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("prometheus.yml is not valid YAML: %v", err)
		}
		if _, ok := parsed["scrape_configs"]; !ok {
			t.Error("prometheus.yml missing scrape_configs")
		}
		if _, ok := parsed["rule_files"]; !ok {
			t.Error("prometheus.yml must load /etc/prometheus/alert-rules.yml via rule_files")
		}
		if !strings.Contains(string(data), "job_name: 'etcd'") {
			t.Error("prometheus.yml missing the etcd job (required by the EtcdNoQuorum alert)")
		}
		if _, ok := parsed["alerting"]; !ok {
			t.Error("prometheus.yml must configure alerting (Alertmanager)")
		}
		if !strings.Contains(string(data), "localhost:9093") {
			t.Error("prometheus.yml must forward alerts to Alertmanager on localhost:9093")
		}
	})

	t.Run("alertmanager.yml", func(t *testing.T) {
		// Deployed by Puppet (role::monitor) to /etc/prometheus/alertmanager.yml.
		data, err := os.ReadFile(repoFile(t, "puppet/modules/role/files/prometheus/alertmanager.yml"))
		if err != nil {
			t.Fatalf("failed to read alertmanager.yml: %v", err)
		}
		var parsed map[string]interface{}
		if err := yaml.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("alertmanager.yml is not valid YAML: %v", err)
		}
		if _, ok := parsed["route"]; !ok {
			t.Error("alertmanager.yml missing a route")
		}
		receivers, ok := parsed["receivers"].([]interface{})
		if !ok || len(receivers) == 0 {
			t.Error("alertmanager.yml must define at least one receiver")
		}
	})

	t.Run("monitor.pp deploys alertmanager", func(t *testing.T) {
		data, err := os.ReadFile(repoFile(t, "puppet/modules/role/manifests/monitor.pp"))
		if err != nil {
			t.Fatalf("failed to read monitor.pp: %v", err)
		}
		manifest := string(data)
		for _, want := range []string{
			"package { 'prometheus-alertmanager'",
			"/etc/prometheus/alertmanager.yml",
			"service { 'prometheus-alertmanager'",
		} {
			if !strings.Contains(manifest, want) {
				t.Errorf("monitor.pp missing Alertmanager wiring: %q", want)
			}
		}
	})

	t.Run("alert-rules.yml", func(t *testing.T) {
		data, err := cms.TemplatesFS.ReadFile("templates/monitoring/alert-rules.yml")
		if err != nil {
			t.Fatalf("failed to read alert-rules.yml: %v", err)
		}
		var parsed map[string]interface{}
		if err := yaml.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("alert-rules.yml is not valid YAML: %v", err)
		}
		if _, ok := parsed["groups"]; !ok {
			t.Error("alert-rules.yml missing groups")
		}
		for _, alert := range []string{"EtcdNoQuorum", "DRBDPeerDown", "DRBDSplitBrain", "KeepalivedState", "ProbeLatencyHigh"} {
			if !strings.Contains(string(data), "alert: "+alert) {
				t.Errorf("alert-rules.yml missing the %s alert", alert)
			}
		}
	})

	t.Run("alert-rules.yml reads the metrics the exporters expose", func(t *testing.T) {
		data, err := cms.TemplatesFS.ReadFile("templates/monitoring/alert-rules.yml")
		if err != nil {
			t.Fatalf("failed to read alert-rules.yml: %v", err)
		}
		rules := string(data)

		// `up{job=...}` is the scrape health of the exporter, not of the
		// service it monitors: nginx_up/mysql_up/apache_up stay 1 while the
		// daemon is down. Assert the service metric is part of each alert.
		for _, want := range []string{
			"mysql_up == 0 or up{job=\"mysql\"} == 0",
			"nginx_up == 0 or up{job=\"nginx\"} == 0",
			"apache_up == 0 or up{job=\"apache\"} == 0",
		} {
			if !strings.Contains(rules, want) {
				t.Errorf("alert-rules.yml does not check the service health metric: %q", want)
			}
		}

		// blackbox exports probe_duration_seconds as a gauge (there is no
		// _bucket series), and covers the frontend probes with a second job.
		if strings.Contains(rules, "probe_duration_seconds_bucket") {
			t.Error("alert-rules.yml uses the nonexistent probe_duration_seconds_bucket series")
		}
		if !strings.Contains(rules, `probe_duration_seconds{job=~"blackbox.*"}`) {
			t.Error("ProbeLatencyHigh does not cover every blackbox job")
		}
		if !strings.Contains(rules, `probe_success{job=~"blackbox.*"}`) {
			t.Error("BlackboxProbeFailed does not cover every blackbox job")
		}
		if !strings.Contains(rules, "probe_http_status_code") {
			t.Error("the 5xx alert must use a status code the exporters actually expose")
		}
		if strings.Contains(rules, `nginx_http_requests_total{status`) {
			t.Error("nginx_http_requests_total has no status label; the 5xx rate cannot be derived from it")
		}
	})
}

func TestTemplates_NoDuplicatedMonitoringConfig(t *testing.T) {
	// Monitoring configuration lives in exactly one place (the Puppet role
	// files). templates/monitoring/ must only carry the alert rules the
	// monitoring phase deploys; any other file would silently drift from the
	// Puppet copy that actually lands on the node.
	entries, err := os.ReadDir(repoFile(t, "templates/monitoring"))
	if err != nil {
		t.Fatalf("failed to read templates/monitoring: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "alert-rules.yml" {
		t.Fatalf("templates/monitoring must contain only alert-rules.yml, got %v", names)
	}
}

func TestTemplates_EmbeddedNginxConfig(t *testing.T) {
	data, err := cms.TemplatesFS.ReadFile("templates/nginx/cms-lb.conf")
	if err != nil {
		t.Fatalf("failed to read cms-lb.conf: %v", err)
	}
	content := string(data)
	expectedDirectives := []string{
		"upstream cms_backend",
		"proxy_pass",
		"listen 80",
		"listen 443 ssl",
	}
	for _, dir := range expectedDirectives {
		if !strings.Contains(content, dir) {
			t.Errorf("cms-lb.conf missing expected directive: %s", dir)
		}
	}
}

func TestTemplates_CasperHelperSingleSource(t *testing.T) {
	// The helper that forces casper's NFS-over-TCP mount must exist as a single
	// canonical copy (templates/cobbler/, embedded in the binary and uploaded by
	// the Go phase). The legacy shell script has to consume that same file; a
	// second copy under scripts/ had already drifted from it.
	data, err := cms.TemplatesFS.ReadFile("templates/cobbler/casper_initrd_tcp.sh")
	if err != nil {
		t.Fatalf("failed to read embedded casper helper: %v", err)
	}
	if !strings.HasPrefix(string(data), "#!/usr/bin/env bash") {
		t.Error("casper helper must be a bash script")
	}
	if !strings.Contains(string(data), "070701") {
		t.Error("casper helper is missing its cpio parser")
	}

	legacy, err := os.ReadFile("../scripts/01_setup_cobbler.sh")
	if err != nil {
		t.Fatalf("failed to read legacy cobbler script: %v", err)
	}
	if !strings.Contains(string(legacy), "templates/cobbler/casper_initrd_tcp.sh") {
		t.Error("legacy cobbler script must reference the canonical casper helper")
	}
	if strings.Contains(string(legacy), "utils/casper_initrd_tcp.sh") {
		t.Error("legacy cobbler script must not reference a duplicated casper helper")
	}
	if _, err := os.Stat("../scripts/utils/casper_initrd_tcp.sh"); err == nil {
		t.Error("scripts/utils/casper_initrd_tcp.sh must not exist: templates/cobbler/ is the single source")
	}
}
