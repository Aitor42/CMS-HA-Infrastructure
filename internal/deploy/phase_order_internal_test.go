package deploy

import (
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/catalog"
)

func phaseNames(list []phases.Phase) []string {
	names := make([]string, len(list))
	for i, p := range list {
		names[i] = p.Name()
	}
	return names
}

// TestBuildPhaseListMatchesCatalog keeps the explicit constructor order in
// BuildPhaseList and the metadata order in catalog.All from drifting apart.
func TestBuildPhaseListMatchesCatalog(t *testing.T) {
	o := &Orchestrator{Cfg: &config.Config{}}

	got := phaseNames(o.BuildPhaseList(DeployOpts{}))
	want := []string{
		catalog.InitVMs.Name, // jumpstart
		catalog.Cobbler.Name,
		catalog.RegisterNodes.Name,
		catalog.InitVMs.Name, // client nodes
		catalog.RepairSSH.Name,
		catalog.RouterBootstrap.Name,
		catalog.Puppet.Name,
		catalog.DRBD.Name,
		catalog.Backup.Name,
		catalog.Kubernetes.Name,
		catalog.Nginx.Name,
		catalog.Monitoring.Name,
		catalog.UFW.Name,
		catalog.PKI.Name,
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("phase order = %v, want %v", got, want)
	}

	wantSkip := append([]string{}, want[1:3]...)
	wantSkip = append(wantSkip, want[4:]...)
	skip := phaseNames(o.BuildPhaseList(DeployOpts{SkipVMCreate: true}))
	if strings.Join(skip, ",") != strings.Join(wantSkip, ",") {
		t.Fatalf("skip-VM phase order = %v, want %v", skip, wantSkip)
	}
}
