package tests

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"text/template"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/drbd"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

func TestDRBD_PhaseRequiresTwoMasters(t *testing.T) {
	pool := &ssh.Pool{}

	t.Run("fails with 0 masters", func(t *testing.T) {
		cfg := &config.Config{
			Nodes: config.NodesConfig{
				Masters: []config.NodeDetail{},
			},
		}
		p := drbd.NewPhase(cfg, pool)
		err := p.Run(context.Background())
		if err == nil {
			t.Fatal("expected error when masters < 2")
		}
		if !strings.Contains(err.Error(), "requires at least 2 master nodes") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("fails with 1 master", func(t *testing.T) {
		cfg := &config.Config{
			Nodes: config.NodesConfig{
				Masters: []config.NodeDetail{
					{Name: "master1", IP: "192.168.10.11"},
				},
			},
		}
		p := drbd.NewPhase(cfg, pool)
		err := p.Run(context.Background())
		if err == nil {
			t.Fatal("expected error when masters < 2")
		}
		if !strings.Contains(err.Error(), "requires at least 2 master nodes") {
			t.Errorf("unexpected error message: %v", err)
		}
	})
}

func TestDRBD_ResourceTemplateRendering(t *testing.T) {
	tmplContent, err := cms.TemplatesFS.ReadFile("templates/drbd/cms-data.res")
	if err != nil {
		t.Fatalf("failed to read cms-data.res: %v", err)
	}

	tmpl, err := template.New("drbd").Parse(string(tmplContent))
	if err != nil {
		t.Fatalf("failed to parse drbd template: %v", err)
	}

	cfg := &config.Config{
		Nodes: config.NodesConfig{
			Masters: []config.NodeDetail{
				{
					Name: "internal-master1",
					IP:   "192.168.10.11",
					FQDN: "internal-master1.internal.local",
				},
				{
					Name: "internal-master2",
					IP:   "192.168.10.12",
					FQDN: "internal-master2.internal.local",
				},
			},
		},
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, cfg); err != nil {
		t.Fatalf("failed to render drbd template: %v", err)
	}

	rendered := buf.String()

	expectedSubstrings := []string{
		"resource cms_data",
		"protocol C;",
		"after-sb-0pri discard-zero-changes;",
		"after-sb-1pri discard-secondary;",
		"after-sb-2pri disconnect;",
		"on internal-master1.internal.local internal-master1",
		"address 192.168.10.11:7788;",
		"on internal-master2.internal.local internal-master2",
		"address 192.168.10.12:7788;",
		"device /dev/drbd0;",
		"disk /dev/vdb;",
		"meta-disk internal;",
	}

	for _, sub := range expectedSubstrings {
		if !strings.Contains(rendered, sub) {
			t.Errorf("rendered DRBD template missing expected line: %s", sub)
		}
	}
}

func TestDRBD_WatchdogScriptIntegrity(t *testing.T) {
	content, err := cms.TemplatesFS.ReadFile("templates/drbd/drbd-watchdog.sh")
	if err != nil {
		t.Fatalf("failed to read drbd-watchdog.sh: %v", err)
	}

	script := string(content)

	checks := []struct {
		name   string
		needle string
	}{
		{"Gateway quorum check", "GATEWAY="},
		{"Ping count parameter", "ping -c"},
		{"Failure strike threshold", "FAIL_THRESHOLD="},
		{"DRBD device", "DRBD_DEVICE=\"/dev/drbd0\""},
		{"Mount point", "MOUNT_POINT=\"/mnt/data/mariadb\""},
		{"Kubeconfig path", "KUBECONFIG="},
		{"Failover trigger script", "/usr/local/bin/drbd-failover.sh"},
		{"Resource name", "RESOURCE=\"cms_data\""},
	}

	for _, c := range checks {
		if !strings.Contains(script, c.needle) {
			t.Errorf("watchdog script missing %s (%q)", c.name, c.needle)
		}
	}
}

func TestDRBD_WatchdogServiceDefinition(t *testing.T) {
	content, err := cms.TemplatesFS.ReadFile("templates/drbd/drbd-failover-watchdog.service")
	if err != nil {
		t.Fatalf("failed to read watchdog service template: %v", err)
	}

	svc := string(content)

	expectedDirectives := []string{
		"[Unit]",
		"Description=DRBD Automatic Failover Watchdog Daemon",
		"After=network-online.target drbd.service",
		"Before=k3s.service",
		"[Service]",
		"Type=simple",
		"ExecStart=/usr/local/bin/drbd-watchdog.sh",
		"Restart=always",
		"[Install]",
		"WantedBy=multi-user.target",
	}

	for _, d := range expectedDirectives {
		if !strings.Contains(svc, d) {
			t.Errorf("watchdog service missing expected directive: %s", d)
		}
	}
}
