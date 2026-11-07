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
| **internal-master1/2/3** | 22; 6443, 2379:2380, 10250, 8472/udp from Internal; 30306 from Main/Internal; 9100, 9104, 2381 from monitor; 7788 from Internal | SSH, K3s API/etcd/kubelet/Flannel, MariaDB NodePort, exporters, DRBD |
| **internal-worker1/2** | 22, 9100, 10250 | SSH, node_exporter, kubelet |
| **internal-storage** | 22, 2049/tcp from configured CMS frontend IPs only, 9100 from monitor | SSH, NFSv4, node_exporter; no Main-wide NFS/RPC grant |
| **main-lb1/2** | 22, 80, 443, 9100, 9113; IP protocol 112 (not a TCP port) | SSH, HTTP, HTTPS, node_exporter, nginx_exporter, VRRP (keepalived) |
| **main-cms1/2** | 22, 80, 9100, 9117 | SSH, Apache, node_exporter, apache_exporter |
| **jumpstart** | 22, 80, 443, 25151, 8140, 53, 123/udp, 69/udp, 67/udp (enp1s0/enp2s0), 2049, 111, 8443 | SSH, Cobbler/Puppet/DNS/NTP/TFTP, DHCP on the two provisioning NICs, NFS/RPC, step-ca; provisioning services are scoped to the Internal/Main subnets, not the WAN |

K3s API `6443/tcp` is scoped to Internal; MariaDB `30306/tcp` is scoped to
both lab CIDRs, not just the frontends. Puppet removes legacy public grants for
those ports after adding the replacements. Storage derives literal frontend IPs
from generated Hiera, adds their `2049/tcp` grants and removes the legacy
Main-subnet `2049`, `2049/tcp` and `111` grants. Additional hand-written rules
still need operator review. NodePort INPUT rules are not complete isolation:
kube-proxy DNAT/FORWARD can bypass UFW INPUT; pre-DNAT/CNI enforcement and live
packet-path tests would be needed for a stronger claim. This uploads allowlist is distinct from jumpstart's
subnet-scoped provisioning NFS export. Re-run `setup-puppet` for migration; see
[MANUAL section 6.2](../MANUAL.md#62-wordpress-media-and-file-backups).

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
