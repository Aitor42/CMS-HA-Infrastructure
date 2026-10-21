# PHASE 00 — Network Design and Addressing

## Objectives to Achieve

- [x] Segment the network into `internal` and `main` subnets.
- [x] Design network layout defining L3 addresses and VLANs.
- [x] Place the 7 internal service nodes and the 4 main service nodes.

---

## Technical Implementation

### Subnet Design

| Network | CIDR | Gateway | Function |
|:--------|:-----|:--------|:---------|
| **internal** | 192.168.10.0/24 | 192.168.10.1 | K3s cluster, monitoring, centralized storage |
| **main** | 192.168.20.0/24 | 192.168.20.1 | Load balancer, CMS frontends, hot-desks |
| **WAN** | DHCP | — | Internet access (router external interface) |

### IP Address Inventory

**Internal Network — `internal` (7 nodes):**

| Hostname | IP | MAC | Role | RAM |
|:---------|:---|:---|:-----|:----|
| internal-monitor | 192.168.10.20 | 52:54:00:10:01:10 | Prometheus + Grafana | 1024 MB |
| internal-master1 | 192.168.10.11 | 52:54:00:10:01:11 | K3s master + DRBD | 1024 MB |
| internal-master2 | 192.168.10.12 | 52:54:00:10:01:12 | K3s master + DRBD | 1024 MB |
| internal-master3 | 192.168.10.16 | 52:54:00:10:01:16 | K3s master (etcd quorum) | 1024 MB |
| internal-worker1 | 192.168.10.13 | 52:54:00:10:01:13 | K3s worker | 768 MB |
| internal-worker2 | 192.168.10.14 | 52:54:00:10:01:14 | K3s worker | 768 MB |
| internal-storage | 192.168.10.15 | 52:54:00:10:01:15 | Centralized storage | 1024 MB |

**Main Network — `main` (4 nodes + 3 hot-desks):**

| Hostname | IP | MAC | Role | RAM |
|:---------|:---|:---|:-----|:----|
| main-lb1 | 192.168.20.103 | 52:54:00:10:02:67 | Nginx LB (keepalived MASTER) | 768 MB |
| main-lb2 | 192.168.20.104 | 52:54:00:10:02:68 | Nginx LB (keepalived BACKUP) | 768 MB |
| *(VIP)* | 192.168.20.100 | — | Floating keepalived address (clients + router DNAT) | — |
| main-cms1 | 192.168.20.101 | 52:54:00:10:02:65 | WordPress + Apache | 1024 MB |
| main-cms2 | 192.168.20.102 | 52:54:00:10:02:66 | WordPress + Apache | 1024 MB |
| main-hotdesk1..3 (up to 8) | 192.168.20.201..203 | dynamic | Employee workstation | 768 MB |

**Others:**

| Hostname | IP | Role |
|:---------|:---|:-----|
| jumpstart | 192.168.10.10 / 192.168.20.10 | Cobbler + Puppet (both networks) |
| ufw-router | 192.168.10.1 / 192.168.20.1 / DHCP | Perimeter firewall / router |

### Implementation

- `internal/phases/vms` (`cms-ha phase init-vms`) — creates the libvirt networks and VM domains from `config.yaml`.
- `cms-ha vm install-batches` — installs the clients in batches and shrinks their RAM afterwards.
- `cms-ha vm start` / `cms-ha vm shrink` — powers the guests on and applies the production memory profile.

### Verification

```bash
sudo virsh net-list --all      # Verify internal and main networks
sudo virsh list --all          # Verify all 16 VMs created
```
