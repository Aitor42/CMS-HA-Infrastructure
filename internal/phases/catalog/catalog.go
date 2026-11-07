// Package catalog is the single source of truth for phase metadata: the name
// and description used by the orchestrator, and the CLI command, short help and
// aliases. Constructors stay with the orchestrator/CLI because each phase needs
// different dependencies, but no display string is duplicated there.
package catalog

// Spec describes one phase.
type Spec struct {
	// Name is the display name and the value accepted by --from-phase after
	// normalisation (lowercase, non-alphanumeric runs collapsed to a dash).
	Name string
	// Description is the long description shown by the orchestrator.
	Description string
	// CLI is the `cms-ha phase` subcommand.
	CLI string
	// Short is the one-line help for that subcommand.
	Short string
	// Aliases are extra subcommand names.
	Aliases []string
}

// Deploy phases, in the order the CLI presents them. The orchestrator order is
// explicit because it depends on options, but it must agree with this list.
var (
	InitVMs = Spec{
		Name:        "00-init-vms",
		Description: "Initialize libvirt networks and VMs",
		CLI:         "init-vms",
		Short:       "Initialize libvirt networks and VMs",
	}
	Cobbler = Spec{
		Name:        "01-setup-cobbler",
		Description: "Install and configure Cobbler PXE provisioning server on Jumpstart",
		CLI:         "setup-cobbler",
		Short:       "Install and configure the Cobbler provisioning server",
		Aliases:     []string{"cobbler"},
	}
	RegisterNodes = Spec{
		Name:        "02-register-nodes",
		Description: "Register all nodes in Cobbler for PXE boot",
		CLI:         "register-nodes",
		Short:       "Register client nodes with Cobbler",
		Aliases:     []string{"register"},
	}
	RepairSSH = Spec{
		Name:        "03-repair-ssh",
		Description: "Repair SSH connectivity and distribute keys across all nodes",
		CLI:         "repair-ssh",
		Short:       "Repair SSH host keys across all nodes",
		Aliases:     []string{"ssh"},
	}
	RouterBootstrap = Spec{
		Name:        "Router Bootstrap",
		Description: "Give the router its gateway role (addresses, forwarding, NAT) before any node needs the internet",
		CLI:         "bootstrap-router",
		Short:       "Give the router its gateway role (addresses, forwarding, NAT)",
		Aliases:     []string{"router-bootstrap"},
	}
	Puppet = Spec{
		Name:        "04-setup-puppet",
		Description: "Install Puppet Server on Jumpstart and Puppet Agents on all nodes",
		CLI:         "setup-puppet",
		Short:       "Install and configure the Puppet server",
		Aliases:     []string{"puppet"},
	}
	DRBD = Spec{
		Name:        "DRBD Setup",
		Description: "Sets up DRBD replication for MariaDB data",
		CLI:         "setup-drbd",
		Short:       "Configure DRBD replication on the master nodes",
		Aliases:     []string{"drbd"},
	}
	Backup = Spec{
		Name:        "Backup Store Setup",
		Description: "Deploys the off-DRBD Garage S3 backup store on the hypervisor",
		CLI:         "setup-backup",
		Short:       "Configure the S3 (Garage) backup stack",
		Aliases:     []string{"backup"},
	}
	Kubernetes = Spec{
		Name:        "Kubernetes Setup",
		Description: "Sets up Kubernetes cluster (K3s) and deploys MariaDB",
		CLI:         "setup-kubernetes",
		Short:       "Install and configure the K3s cluster",
		Aliases:     []string{"kubernetes", "k8s"},
	}
	Nginx = Spec{
		Name:        "NGINX & WordPress Setup",
		Description: "Converges the CMS frontends and verifies nginx, Apache, NFS and HTTPS",
		CLI:         "setup-nginx-wordpress",
		Short:       "Configure Nginx, WordPress and the CMS frontends",
		Aliases:     []string{"setup-nginx", "nginx", "wordpress"},
	}
	Monitoring = Spec{
		Name:        "Monitoring Setup",
		Description: "Verifies the monitoring stack and deploys its alert rules",
		CLI:         "setup-monitoring",
		Short:       "Install and configure Prometheus and Grafana",
		Aliases:     []string{"monitoring"},
	}
	UFW = Spec{
		Name:        "UFW Setup",
		Description: "Sets up UFW rules and NAT",
		CLI:         "setup-ufw",
		Short:       "Configure UFW firewall rules",
		Aliases:     []string{"ufw"},
	}
	PKI = Spec{
		Name:        "Internal CA Setup",
		Description: "Sets up Internal CA with step-ca",
		CLI:         "setup-ca",
		Short:       "Set up the internal certificate authority (PKI)",
		Aliases:     []string{"ca", "pki"},
	}
)

// All returns every phase spec in deploy order. InitVMs appears once: the
// orchestrator creates two instances of it (jumpstart and client nodes) from
// the same metadata.
func All() []Spec {
	return []Spec{
		InitVMs, Cobbler, RegisterNodes, RepairSSH, RouterBootstrap, Puppet,
		DRBD, Backup, Kubernetes, Nginx, Monitoring, UFW, PKI,
	}
}
