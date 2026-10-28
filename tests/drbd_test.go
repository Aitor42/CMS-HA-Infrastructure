package tests

import (
	"bytes"
	"context"
	"os"
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

func TestDRBD_FailoverScriptRefusesForcedPromotionAndClearsAllMasters(t *testing.T) {
	content, err := cms.TemplatesFS.ReadFile("templates/drbd/drbd-failover.sh")
	if err != nil {
		t.Fatalf("failed to read drbd-failover.sh: %v", err)
	}
	script := string(content)
	if strings.Contains(script, "drbdadm primary --force") {
		t.Error("failover promotion must not force an Outdated/Inconsistent disk to Primary")
	}
	if !strings.Contains(script, `for node in $(kubectl get nodes`) || !strings.Contains(script, `grep -vx "$LOCAL_NODE"`) {
		t.Error("failover promotion must clear the primary label from every other master")
	}
}

func TestDRBD_FailoverDemotePausesWatchdogAndWaitsForThePod(t *testing.T) {
	content, err := cms.TemplatesFS.ReadFile("templates/drbd/drbd-failover.sh")
	if err != nil {
		t.Fatalf("failed to read drbd-failover.sh: %v", err)
	}
	script := string(content)
	if !strings.Contains(script, "systemctl stop drbd-failover-watchdog.service") {
		t.Error("demote must pause the watchdog so it cannot scale MariaDB back up mid-demote")
	}
	if !strings.Contains(script, "trap restart_watchdog EXIT") {
		t.Error("demote must restart the watchdog even when a step fails")
	}
	if !strings.Contains(script, "kubectl get pod mariadb-0 -n cms") {
		t.Error("demote must wait for mariadb-0 to release the volume before unmounting")
	}
}

func TestDRBD_WatchdogRestoresMariaDBReplicas(t *testing.T) {
	content, err := cms.TemplatesFS.ReadFile("templates/drbd/drbd-watchdog.sh")
	if err != nil {
		t.Fatalf("failed to read drbd-watchdog.sh: %v", err)
	}
	if !strings.Contains(string(content), "kubectl scale statefulset mariadb -n cms --replicas=1") {
		t.Error("watchdog must scale MariaDB back to one replica when the local node is Primary")
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
		"After=network-online.target drbd.service drbd-cms-boot.service",
		"Wants=network-online.target drbd.service drbd-cms-boot.service",
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

// TestDrbdMetricsScriptUsesTheResourceName keeps the textfile publisher in step
// with templates/drbd/cms-data.res. When the names drift apart "drbdadm status"
// fails, the script deletes its .prom file, and the DRBD panels go blank while
// DRBDPeerDown/DRBDSplitBrain stay silent: nothing else in the lab notices.
func TestDrbdMetricsScriptUsesTheResourceName(t *testing.T) {
	script, err := os.ReadFile("../puppet/modules/role/files/cms-ha/cms-drbd-metrics.sh")
	if err != nil {
		t.Fatalf("read the DRBD metrics script: %v", err)
	}
	const want = `RESOURCE="${CMS_DRBD_RESOURCE:-cms_data}"`
	if !strings.Contains(string(script), want) {
		t.Fatalf("cms-drbd-metrics.sh must query the resource created by templates/drbd/cms-data.res, want %s", want)
	}
}
