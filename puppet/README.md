# Puppet — Node Configuration

`puppet/` declares the desired state of every node: packages, services,
configuration files, exporters and firewall rules. Once an agent is registered,
it re-applies its catalogue every 30 minutes.

## Layout

```
puppet/
├── manifests/site.pp              # maps hostnames to role classes
└── modules/role/
    ├── manifests/                 # one class per node type
    │   ├── base.pp                # all nodes: node_exporter, chrony, UFW, swap, /etc/hosts
    │   ├── router.pp              # ufw-router
    │   ├── jumpstart.pp           # jumpstart: Puppet Server, Cobbler, DNS, NFS
    │   ├── loadbalancer.pp        # main-lb*: Nginx, keepalived, TLS
    │   ├── cms_frontend.pp        # main-cms*: Apache, PHP, WordPress, WP-CLI
    │   ├── monitor.pp             # internal-monitor: Prometheus, Grafana
    │   ├── k3s_master.pp          # internal-master*: K3s deps, UFW
    │   ├── k3s_worker.pp          # internal-worker*: K3s deps, UFW
    │   ├── storage.pp             # internal-storage: NFS uploads
    │   └── hotdesk.pp             # main-hotdesk*: XFCE, LightDM
    └── files/                     # static files served by the fileserver
```

## Role Assignment

| Hostname | Role class | Key services |
|:---------|:-----------|:-------------|
| `jumpstart` | `role::jumpstart` | Puppet Server, Cobbler, DNS, NTP, NFS |
| `ufw-router` | `role::router` | IP forwarding, UFW perimeter, inter-zone routing |
| `internal-monitor` | `role::monitor` | Prometheus, Grafana, Alertmanager |
| `internal-master*` | `role::k3s_master` | K3s prerequisites, UFW (API/etcd/DRBD/Flannel) |
| `internal-worker*` | `role::k3s_worker` | K3s prerequisites, UFW (kubelet/Flannel) |
| `internal-storage` | `role::storage` | NFS export for WordPress uploads |
| `main-lb*` | `role::loadbalancer` | Nginx, step-ca certificate, keepalived VIP |
| `main-cms*` | `role::cms_frontend` | Apache, PHP, WordPress, WP-CLI |
| `main-hotdesk*` | `role::hotdesk` | XFCE desktop, LightDM |
| `*` (default) | `role::base` | node_exporter, chrony, `/etc/hosts`, swap, UFW base |

Every role includes `role::base` first, so the base configuration is applied
regardless of the node type.

## What Puppet manages

- Package installation and service state for every role.
- Configuration files (Nginx, Apache, Prometheus, Alertmanager, Grafana provisioning, chrony).
- WordPress, `wp-config.php`, WP-CLI and `wp core install`.
- Certificates issued by the internal CA and their renewal timers.
- UFW policies and per-role rules.
- `/etc/hosts`, swap and the DRBD/keepalived textfile metrics.

## What the orchestrator does

Steps Puppet cannot own: VM and network creation, Cobbler bootstrap, the Puppet
Server install (chicken-and-egg), K3s token exchange, DRBD promote/demote, and
the NAT rules whose WAN interface is detected at runtime.

## Deployment Flow

```text
cms-ha phase init-vms          # VMs + networks
cms-ha phase setup-cobbler     # PXE installs the OS
cms-ha phase setup-puppet      # Puppet Server + agents, first full catalogue
cms-ha phase setup-drbd        # DRBD replication
cms-ha phase setup-kubernetes  # K3s + MariaDB
cms-ha phase setup-nginx       # Nginx, keepalived, WordPress
cms-ha phase setup-monitoring  # Prometheus + Grafana
cms-ha phase setup-ufw         # firewall + NAT
```

## Manual Operations

```bash
# Force a Puppet run on a node (non-root deploy account + sudo)
ssh ubuntu@<NODE_IP> 'sudo /opt/puppetlabs/bin/puppet agent -t'

# Dry run (show what would change)
ssh ubuntu@<NODE_IP> 'sudo /opt/puppetlabs/bin/puppet agent -t --noop'

# Check the run interval
ssh ubuntu@<NODE_IP> 'sudo /opt/puppetlabs/bin/puppet agent --configprint runinterval'

# List signed certificates (from jumpstart)
ssh ubuntu@192.168.10.10 'sudo puppetserver ca list --all'
```
