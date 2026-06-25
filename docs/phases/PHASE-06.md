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
- SSH (22) permitted only from the jumpstart node (192.168.10.10)
- node_exporter (9100) permitted only from the monitor node (192.168.10.20)

### Rules per Node

| Node | Open Ports | Justification |
|:-----|:-----------|:--------------|
| **internal-monitor** | 22, 9090, 3000, 9100 | SSH, Prometheus, Grafana, node_exporter |
| **internal-master1** | 22, 6443, 30306, 9100, 9104, 7788 | SSH, K3s API, MariaDB NodePort, exporters, DRBD |
| **internal-master2** | 22, 6443, 9100, 7788 | SSH, K3s API, node_exporter, DRBD |
| **internal-worker1/2** | 22, 9100, 10250 | SSH, node_exporter, kubelet |
| **internal-storage** | 22, 9100 | SSH, node_exporter |
| **main-lb** | 22, 80, 443, 9100, 9113 | SSH, HTTP, HTTPS, node_exporter, nginx_exporter |
| **main-cms1/2** | 22, 80, 9100, 9117 | SSH, Apache, node_exporter, apache_exporter |

### Associated Scripts

- `scripts/05_setup_ufw.sh` — Nodal firewalling section (second part of the script)

### Verification

```bash
# Verify rules on each node type
ssh root@192.168.10.20 "ufw status numbered"
ssh root@192.168.10.11 "ufw status numbered"
ssh root@192.168.20.100 "ufw status numbered"

# Port isolation test (from an external node):
# MariaDB direct port 3306 on master1 should NOT be accessible directly (only 30306 NodePort)
nc -zv 192.168.10.11 3306   # Should fail
nc -zv 192.168.10.11 30306  # Should work
```
