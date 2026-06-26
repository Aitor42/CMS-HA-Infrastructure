# PHASE 00 — Network Design and Addressing

## Objectives to Achieve

- [x] Segment the network into `internal` and `main` subnets.
- [x] Design network layout defining L3 addresses and VLANs.
- [x] Place the 6 static nodes in `internal` and the 3 static nodes in `main`.

---

## Technical Implementation

### Subnet Design

| Network | CIDR | Gateway | Function |
|:--------|:-----|:--------|:---------|
| **internal** | 192.168.10.0/24 | 192.168.10.1 | K3s cluster, monitoring, centralized storage |
| **main** | 192.168.20.0/24 | 192.168.20.1 | Load balancer, CMS frontends, hot-desks |
| **WAN** | DHCP | — | Internet access (router external interface) |

### IP Address Inventory

**Internal Network (6 nodes):**

| Hostname | IP | MAC | Role | RAM |
|:---------|:---|:---|:-----|:----|
| internal-monitor | 192.168.10.20 | 52:54:00:10:01:10 | Prometheus + Grafana | 512 MB |
| internal-master1 | 192.168.10.11 | 52:54:00:10:01:11 | K3s master + DRBD | 1024 MB |
| internal-master2 | 192.168.10.12 | 52:54:00:10:01:12 | K3s master + DRBD | 1024 MB |
| internal-worker1 | 192.168.10.13 | 52:54:00:10:01:13 | K3s worker | 768 MB |
| internal-worker2 | 192.168.10.14 | 52:54:00:10:01:14 | K3s worker | 768 MB |
| internal-storage | 192.168.10.15 | 52:54:00:10:01:15 | Centralized storage | 1024 MB |

**Client Network / Main (3 nodes + 8 hot-desks):**

| Hostname | IP | MAC | Role | RAM |
|:---------|:---|:---|:-----|:----|
| main-lb | 192.168.20.100 | 52:54:00:10:02:64 | Nginx LB | 512 MB |
| main-cms1 | 192.168.20.101 | 52:54:00:10:02:65 | WordPress + Apache | 512 MB |
| main-cms2 | 192.168.20.102 | 52:54:00:10:02:66 | WordPress + Apache | 512 MB |
| main-hotdesk1..8 | 192.168.20.201..208 | dynamic | Employee workstation | 768 MB |

**Others:**

| Hostname | IP | Role |
|:---------|:---|:-----|
| jumpstart | 192.168.10.10 / 192.168.20.10 | Cobbler + Puppet (both networks) |
| ufw-router | 192.168.10.1 / 192.168.20.1 / DHCP | Perimeter firewall / router |

### Associated Scripts

- `scripts/00_init_vms.sh` — Creates the virtual networks (bridges) and all VMs
- `scripts/start_all_vms.sh` — Starts existing VMs (resume after `shrink_vm_ram.sh`)
- `scripts/utils/shrink_vm_ram.sh` — Powers off VMs and adjusts RAM to resource-constrained environment limits

### Verification

```bash
sudo virsh net-list --all      # Verify internal and main networks
sudo virsh list --all          # Verify all 20 VMs created
```
