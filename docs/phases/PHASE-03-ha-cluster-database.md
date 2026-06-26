# PHASE 03 — HA Cluster and Database

## Objectives to Achieve

- [x] Install high-availability cluster: 2 master nodes, 2 agent/worker nodes.
- [x] Configure SQL database within the cluster.
- [x] Verify database and controller synchronization and failover.

---

## Technical Implementation

### K3s Cluster (Lightweight Kubernetes)

| Node | IP | K3s Role |
|:-----|:---|:---------|
| internal-master1 | 192.168.10.11 | Server (init, HA) |
| internal-master2 | 192.168.10.12 | Server (join, HA) |
| internal-worker1 | 192.168.10.13 | Agent |
| internal-worker2 | 192.168.10.14 | Agent |

**Installation:**
- Master 1: `curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="server --cluster-init --node-ip 192.168.10.11 --tls-san 192.168.10.11 --tls-san 192.168.10.12 --write-kubeconfig-mode 644" sh -`
- Master 2: Joins the existing cluster using the token.
- Workers: Join as agents using the token.

### MariaDB Database and Replication (DRBD)

- **Deployed as:** StatefulSet in namespace `cms` of the K3s cluster with 1 replica.
- **Image:** `mariadb:10.11`
- **HA Storage (DRBD):** 5 GiB PersistentVolume (type `hostPath` pointing to `/mnt/data/mariadb`).
  - The `/mnt/data/mariadb` directory is the mount point of the DRBD resource `cms_data` (synchronous block-level replication on `/dev/drbd0` over the `/dev/vdb` disk between the two master nodes).
  - Only the promoted primary master (`internal-master1` by default) mounts the DRBD volume.
- **Dynamic Node Pinning (Kubernetes + DRBD):** 
  - To prevent MariaDB from running on nodes without the mounted DRBD storage, `nodeSelector: drbd-status: primary` is configured in `mariadb-statefulset.yaml`.
  - The script `06_setup_kubernetes.sh` automatically labels `internal-master1` with `drbd-status=primary`.
  - The local failover script (`/usr/local/bin/drbd-failover.sh`) dynamically reassigns this label during promotion and demotion, causing Kubernetes to automatically move the MariaDB pod to the active master node.
- **Exposure:** ClusterIP (3306) + NodePort (30306) for access from the main network.
- **Database:** `wordpress`
- **User:** `wp_user` with GRANT ALL on `wordpress`.

### Kubernetes Manifests

The manifests are located in `kubernetes/`:
- `namespace.yaml` — Namespace `cms`
- `mariadb-secret.yaml` — Database credentials
- `mariadb-pv.yaml` / `mariadb-pvc.yaml` — Persistent storage (pointing to `/mnt/data/mariadb`)
- `mariadb-statefulset.yaml` — MariaDB StatefulSet (includes `nodeSelector` for DRBD)
- `mariadb-service.yaml` — ClusterIP and NodePort services
- `init-db-job.yaml` — Database initialization Job

### Associated Scripts

- `scripts/06_setup_kubernetes.sh` — K3s installation, node labeling, and MariaDB deployment.
- `scripts/05_setup_drbd.sh` — Configuration and installation of DRBD on the master nodes and generation of the `/usr/local/bin/drbd-failover.sh` script.

### Verification

```bash
# Verify nodes in K3s
ssh root@192.168.10.11 "kubectl get nodes"

# Verify that the MariaDB pod runs on internal-master1 (active label)
ssh root@192.168.10.11 "kubectl get pods -n cms -o wide"

# Verify DRBD status on the primary master
ssh root@192.168.10.11 "drbdadm status cms_data"

# Test connection to the database via NodePort
mysql -h 192.168.10.11 -P 30306 -u wp_user -p'WpS3cur3P4ss!' wordpress -e "SHOW TABLES;"
```
