# Puppet Configuration Management — CMS High-Availability Infrastructure

This directory contains the full Puppet codebase that declares the **desired state**
of every node in the infrastructure. Once a Puppet agent is registered, it
re-applies its catalogue every 30 minutes automatically.

## Directory Structure

```
puppet/
├── manifests/
│   └── site.pp                  # Node classifier — maps hostnames to roles
└── modules/
    └── role/
        ├── manifests/            # Role classes (desired state per node type)
        │   ├── base.pp           # Applied to ALL nodes (node_exporter, chrony, UFW, swap, /etc/hosts)
        │   ├── router.pp         # ufw-router: IP forwarding, UFW perimeter + forwarding rules
        │   ├── loadbalancer.pp   # main-lb: Nginx, SSL cert, UFW rules
        │   ├── cms_frontend.pp   # main-cms*: Apache, PHP, WordPress, WP-CLI, UFW rules
        │   ├── monitor.pp        # internal-monitor: Prometheus, Grafana + provisioning, UFW rules
        │   ├── k3s_master.pp     # internal-master*: K3s deps, UFW rules (API, etcd, DRBD, Flannel)
        │   ├── k3s_worker.pp     # internal-worker*: K3s deps, UFW rules (kubelet, Flannel)
        │   └── hotdesk.pp        # main-hotdesk*: XFCE desktop, LightDM, NetworkManager
        └── files/                # Static config files served by Puppet fileserver
            ├── nginx/
            │   └── cms-lb.conf           # Nginx upstream pool + SSL reverse proxy
            ├── apache/
            │   ├── wordpress.conf        # Apache VirtualHost for WordPress
            │   └── wordpress.htaccess    # WordPress URL rewriting rules
            ├── prometheus/
            │   └── prometheus.yml        # Prometheus scrape targets (all exporters)
            └── grafana/
                ├── datasource.yaml       # Grafana datasource (Prometheus)
                └── dashboard-provider.yaml  # Grafana dashboard provider config
```

## Role Assignment (site.pp)

| Hostname pattern | Role class | Key services managed |
|---|---|---|
| `ufw-router` | `role::router` | ip_forward, UFW perimeter, inter-zone routing |
| `internal-monitor` | `role::monitor` | Prometheus, Grafana, UFW |
| `internal-master*` | `role::k3s_master` | K3s prereqs, UFW (API/etcd/DRBD/Flannel) |
| `internal-worker*` | `role::k3s_worker` | K3s prereqs, UFW (kubelet/Flannel) |
| `main-lb` | `role::loadbalancer` | Nginx, SSL cert, UFW |
| `main-cms*` | `role::cms_frontend` | Apache, PHP, WordPress, WP-CLI, UFW |
| `main-hotdesk*` | `role::hotdesk` | XFCE desktop, LightDM |
| `*` (default) | `role::base` | node_exporter, chrony, /etc/hosts, swap, UFW base |

Every role **includes `role::base`** as its first statement, so base config is
always applied regardless of the node's specific role.

## What Puppet Manages vs. What Bash Scripts Handle

### Managed entirely by Puppet (declarative, idempotent, auto-applied every 30 min)
- Package installation and service state for all roles
- All config files (Nginx, Apache, Prometheus, Grafana provisioning, chrony)
- WordPress download, `wp-config.php` credentials, WP-CLI, `wp core install`
- Self-signed SSL certificate generation (on first run only)
- UFW default policies and all per-role firewall rules
- `/etc/hosts` cluster-wide name resolution
- Swap file creation and activation

### Handled by bash scripts (bootstrap steps Puppet cannot do)
| Script | Reason |
|---|---|
| `00_init_vms.sh` | Creates VMs on the KVM hypervisor — outside the VMs themselves |
| `01_setup_cobbler.sh` | Bootstraps Cobbler before any agents exist |
| `04_setup_puppet.sh` | Installs Puppet Server itself (chicken-and-egg) |
| `06_setup_kubernetes.sh` | K3s requires ordered token exchange between masters |
| `05_setup_drbd.sh` | DRBD needs cross-node promote/demote coordination |
| NAT rules in `09_setup_ufw.sh` | WAN interface name is detected at runtime from MAC |

## Deployment Flow

```
00_init_vms.sh          # Create VMs + networks
    ↓
01_setup_cobbler.sh     # PXE provision OS on all nodes
    ↓
04_setup_puppet.sh      # Install Puppet Server + agents
                        # Sync puppet/ code to jumpstart codedir  ← THIS DIRECTORY
                        # Initial `puppet agent -t` on all nodes  ← FULL DESIRED STATE APPLIED
    ↓
06_setup_kubernetes.sh  # K3s cluster bootstrap (token exchange)
    ↓
05_setup_drbd.sh        # DRBD block replication setup
    ↓
07_setup_nginx_wordpress.sh  # puppet agent -t (Nginx + WordPress convergence)
08_setup_monitoring.sh  # puppet agent -t (Prometheus + Grafana convergence)
09_setup_ufw.sh         # puppet agent -t + NAT rules injection
```

## Manual Operations

```bash
# Force immediate Puppet run on a node
ssh root@<NODE_IP> '/opt/puppetlabs/bin/puppet agent -t'

# Check Puppet agent status
ssh root@<NODE_IP> '/opt/puppetlabs/bin/puppet agent --configprint runinterval'

# Dry run (show what would change without applying)
ssh root@<NODE_IP> '/opt/puppetlabs/bin/puppet agent -t --noop'

# View last Puppet run report
ssh root@<NODE_IP> '/opt/puppetlabs/bin/puppet report last'

# List all signed certificates (from jumpstart)
ssh root@192.168.10.10 'puppetserver ca list --all'
```
