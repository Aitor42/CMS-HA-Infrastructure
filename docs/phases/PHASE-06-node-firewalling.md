# PHASE 06 — Nodal Firewalling (Endpoint Security)

## Objectives to Achieve

- [x] Implement UFW to limit access to services installed on each node.
- [x] Ensure monitoring and SSH ports are accessible only to the correct source.

---

## Technical Implementation

### General Policy Per Node

Each node has UFW configured with:
- `ufw default deny incoming` — Denies all incoming traffic by default
- Only specific service ports executed by the node are opened
- SSH (22) is open so the orchestrator on the hypervisor can reach every
  node; the perimeter router is the WAN boundary. Restrict this rule to the
  lab CIDRs if the WAN is not trusted.
- node_exporter (9100) permitted only from the monitor node (192.168.10.20)

### Rules per Node

| Node | Open Ports | Justification |
|:-----|:-----------|:--------------|
| **internal-monitor** | 22, 9090, 3000, 9100 | SSH, Prometheus, Grafana, node_exporter |
| **internal-master1** | 22, 6443, 30306, 9100, 9104, 7788 | SSH, K3s API, MariaDB NodePort, exporters, DRBD |
| **internal-master2** | 22, 6443, 9100, 7788 | SSH, K3s API, node_exporter, DRBD |
| **internal-master3** | 22, 6443, 9100, 2379:2380 | SSH, K3s API, node_exporter, etcd |
| **internal-worker1/2** | 22, 9100, 10250 | SSH, node_exporter, kubelet |
| **internal-storage** | 22, 111, 2049, 9100 | SSH, RPCbind, NFS (from Main subnet), node_exporter |
| **main-lb1/2** | 22, 80, 443, 9100, 9113, 112 | SSH, HTTP, HTTPS, node_exporter, nginx_exporter, VRRP (keepalived) |
| **main-cms1/2** | 22, 80, 9100, 9117 | SSH, Apache, node_exporter, apache_exporter |
| **jumpstart** | 22, 80, 443, 25151, 8140, 53, 123/udp, 69/udp, 67/udp (enp1s0/enp2s0), 2049, 111, 8443 | SSH, Cobbler/Puppet/DNS/NTP/TFTP, DHCP on the two provisioning NICs, NFS/RPC, step-ca; provisioning services are scoped to the Internal/Main subnets, not the WAN |

### Implementation

- `puppet/modules/role` (`role::base` plus each role manifest) — applies and
  keeps the per-node rules converged during the Puppet phase.
- `internal/phases/ufw` (`cms-ha phase setup-ufw`) — re-injects the router's
  NAT rules, reloads UFW, verifies the perimeter policies/forwarding/live NAT
  table, and checks that UFW is active on every node (including jumpstart).

### Verification

```bash
# Verify rules on each node type
ssh ubuntu@192.168.10.20 "sudo ufw status numbered"
ssh ubuntu@192.168.10.11 "sudo ufw status numbered"
ssh ubuntu@192.168.20.103 "sudo ufw status numbered"

# Verify the router's live NAT table, not only before.rules
ssh ubuntu@192.168.10.1 "sudo iptables -t nat -S"

# Port isolation test (from an external node):
# MariaDB direct port 3306 on master1 should NOT be accessible directly (only 30306 NodePort)
nc -zv 192.168.10.11 3306   # Should fail
nc -zv 192.168.10.11 30306  # Should work
```
