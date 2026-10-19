# PHASE 03 — HA Cluster and Database

## Objectives to Achieve

- [x] Install high-availability cluster: 3 control-plane (server) nodes, 2 agent/worker nodes.
- [x] Configure SQL database within the cluster.
- [x] Verify database and controller synchronization and failover.

---

## Technical Implementation

### K3s Cluster (Lightweight Kubernetes)

| Node | IP | K3s Role |
|:-----|:---|:---------|
| internal-master1 | 192.168.10.11 | Server (init, HA) + DRBD Primary |
| internal-master2 | 192.168.10.12 | Server (join, HA) + DRBD Secondary |
| internal-master3 | 192.168.10.16 | Server (join, etcd quorum; no DRBD) |
| internal-worker1 | 192.168.10.13 | Agent |
| internal-worker2 | 192.168.10.14 | Agent |

**Installation:**
- Master 1: `curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="server --cluster-init --secrets-encryption --node-external-ip 192.168.10.11 --flannel-iface enp1s0" sh -` (Secret encryption at rest is enabled from cluster init).
- Master 2: joins the existing cluster with `--server https://192.168.10.11:6443 --token-file /root/.k3s-join-token` — the join token is uploaded to each joining node as a file with `0600` permissions and never placed on the command line (an argv value would be readable by every local user via `/proc/*/cmdline`).
- Master 3: joins as an additional control-plane **server** with the same command, giving the embedded etcd cluster an odd quorum (3 members tolerate 1 failure). It carries no DRBD disk. After all servers join, the phase verifies that the number of nodes labelled `node-role.kubernetes.io/etcd` equals the configured master count — K3s runs etcd inside the server process, so its members are control-plane nodes, not kube-system pods.
- Workers: join as agents with `K3S_TOKEN` expanded from the same protected file into an exported environment variable (`export K3S_TOKEN="$(cat /root/.k3s-join-token)"`), visible only in the owner-readable `/proc/*/environ`. Agent join results are validated per node (SSH error **and** exit code); a failed join aborts the phase.

### MariaDB Database and Replication (DRBD)

- **Deployed as:** StatefulSet in namespace `cms` of the K3s cluster with 1 replica.
- **Image:** `mariadb:10.11@sha256:0d0945c57f75b3a3b9ebe71aedaaad005fa5305054e8fca3d2e42a8c92b43b5e` (pinned by digest so a re-tagged `10.11` cannot change the deployed bits).
- **HA Storage (DRBD):** 3 GiB PersistentVolume (type `hostPath` pointing to `/mnt/data/mariadb`).
  - The `/mnt/data/mariadb` directory is the mount point of the DRBD resource `cms_data` (synchronous block-level replication on `/dev/drbd0` over the `/dev/vdb` disk between the two DRBD master nodes, `internal-master1` and `internal-master2`; the third control-plane server carries no DRBD disk).
  - Only the promoted primary master (`internal-master1` by default) mounts the DRBD volume.
- **Dynamic Node Pinning (Kubernetes + DRBD):** 
  - To prevent MariaDB from running on nodes without the mounted DRBD storage, `nodeSelector: drbd-status: primary` is configured in `mariadb-statefulset.yaml`.
  - The script `06_setup_kubernetes.sh` automatically labels `internal-master1` with `drbd-status=primary`.
  - The local failover script (`/usr/local/bin/drbd-failover.sh`) dynamically reassigns this label during promotion and demotion, causing Kubernetes to automatically move the MariaDB pod to the active master node.
  - The pod tolerates node loss for only 60 s (`node.kubernetes.io/not-ready` / `unreachable` `NoExecute` tolerations with `tolerationSeconds: 60`). Kubernetes' default is 300 s, which would leave WordPress without a database for five minutes after a master failure even though the watchdog has already fenced the peer and promoted the survivor. The window is deliberately longer than the 40 s `node-monitor-grace-period`, so a transient blip does not evict the database.
- **Automated Failover Watchdog (`drbd-failover-watchdog.service`):**
  - Continuous background daemon (`/usr/local/bin/drbd-watchdog.sh`) running on both control plane masters.
  - **Quorum & Anti-Split-Brain Safeguards:**
    1. *Gateway Ping Check:* Verifies reachability of the default gateway (`192.168.10.1`). If the gateway is unreachable, failover is suppressed to prevent split-brain under network isolation.
    2. *Peer Heartbeat Check:* Pings the peer master before taking action, avoiding false positives during transient replication interruptions.
    3. *Debounce Strike Threshold:* Requires 3 consecutive failed checks (15s timeout) before triggering promotion.
    4. *STONITH Fencing (`/usr/local/bin/fence-peer.sh`):* The ping checks are only a precondition. Before promoting, the failed peer domain is powered off at the hypervisor and the power-off is **confirmed**; without a confirmed fence the watchdog refuses to promote (unless `drbd.fencing` is disabled). This makes split-brain impossible rather than merely unlikely.
    5. *Autonomous Orchestration:* Automatically promotes DRBD, mounts the partition, updates Kubernetes node affinity labels, and reschedules the MariaDB StatefulSet.
    6. *Automatic Split-Brain Auto-healing:* Upon node recovery, DRBD kernel handlers (`after-sb-1pri discard-secondary`) automatically resynchronize outdated blocks without human intervention.
- **Exposure:** ClusterIP (3306) + NodePort (30306) for access from the main network.
- **Database:** `wordpress`
- **User:** `wp_user` with GRANT ALL on `wordpress`.

### MariaDB Backups (off the DRBD device)

Backups must not share fate with the database: a split-brain on `cms_data` must
never take the copies with it. The copies therefore live **outside the DRBD
device**, in an S3-compatible **Garage** store running as a container on the
hypervisor (`backup` phase), on a data volume under `backup.data_dir`
(default `/var/lib/cms-garage`).

- **Store:** Garage v2.4.1 (`dxflrs/garage`), single-node/`--default-bucket`,
  S3 API published only on the hypervisor's internal address
  (`192.168.10.254:3900`), never on the WAN. Deployed by
  `internal/phases/backup/backup.go`; its `garage.toml` (with freshly generated
  RPC/admin secrets) is written once and reused on later deploys.
- **Dump + upload** (`mariadb-backup-cronjob.yaml`, daily 02:00):
  `mysqldump` writes a gzip dump to the local staging PVC, then an `rclone`
  container uploads it to the bucket and drops objects older than
  `backup.retention_days` (default 7).
- **Restore drill** (`mariadb-restore-test-cronjob.yaml`, daily 03:30): fetches
  the newest object, restores it into a throwaway MariaDB instance, and
  validates the WordPress schema (`wp_posts`, `wp_options`, `wp_users`). It also
  measures the restore duration, so RTO is proven rather than assumed.
- **Alerts:** the jobs publish `cms_backup_last_success_timestamp_seconds` and
  `cms_backup_restore_*` through the node-exporter textfile collector;
  `BackupMissing` (26 h) and `BackupRestoreTestFailing` (48 h) guard the RPO.
- **Configuration** (single source of truth): the `backup:` block in
  `config.yaml`. The `kubernetes` phase derives a `backup-config` ConfigMap
  (endpoint/bucket/region/retention) and a `backup-secret` Secret
  (access/secret key) from it, and pins the CronJob schedules.
- **Engine:** `backup.engine: "garage"` (`""` disables the whole feature). The
  S3 secret must be encrypted with `cms-ha secrets encrypt`.

### Kubernetes Manifests (Kustomize)

The manifests are a **kustomize** tree applied with `kubectl apply -k`; the Go
phase copies the whole tree to the master and runs the same commands an operator
would:

```
kubernetes/
├── base/                 # environment-agnostic platform (applied via the overlay)
│   ├── kustomization.yaml
│   ├── namespace.yaml            # Namespace `cms`
│   ├── mariadb-pv.yaml / mariadb-pvc.yaml   # Persistent storage (/mnt/data/mariadb)
│   ├── mariadb-statefulset.yaml  # MariaDB StatefulSet (nodeSelector: drbd-status=primary)
│   └── mariadb-service.yaml      # ClusterIP + NodePort services
├── backups/              # off-DRBD backup stack (applied only when backup.engine is set)
│   ├── kustomization.yaml
│   ├── mariadb-backup-pv.yaml             # local staging volume
│   ├── mariadb-backup-cronjob.yaml        # daily dump → staging → S3 upload (rclone)
│   └── mariadb-restore-test-cronjob.yaml  # daily restore drill
├── jobs/                 # one-shot Jobs applied after the DB is reachable
│   ├── kustomization.yaml
│   └── init-db-job.yaml  # idempotent DB/user init (ALTER USER, escaped SQL)
└── overlays/lab/         # lab overlay: composes ../../base + lab labels
    ├── kustomization.yaml
    └── mariadb-labels.yaml
```

- **Apply:** `kubectl apply -k kubernetes/overlays/lab` (core),
  `kubectl apply -k kubernetes/backups` (backups) and
  `kubectl apply -k kubernetes/jobs` (init Job). **Drift check:**
  `kubectl diff -k kubernetes/overlays/lab`.
- **Generated at deploy time** (never committed): the MariaDB Secret
  (`makeMariaDBSecret`), plus `backup-config` ConfigMap and `backup-secret`
  Secret derived from the `backup:` block.
- **Schedules** are patched from `config.yaml` after apply, so the manifest
  values are only fallbacks.

### Prometheus Cluster Access (Least Privilege)

The monitoring phase binds Prometheus to a dedicated read-only `ClusterRole` (`prometheus-metrics-reader`: nodes, pods, services, endpoints, namespaces, `/metrics`), not `cluster-admin`. The bearer token issued for scraping is valid for **1 year** and is rotatable by re-running the monitoring phase; failures during token rotation are warnings, not silent failures.

### Associated Scripts & Daemons

- `scripts/06_setup_kubernetes.sh` / `internal/phases/kubernetes/kubernetes.go` — K3s installation, node labeling, and MariaDB deployment.
- `scripts/05_setup_drbd.sh` / `internal/phases/drbd/drbd.go` — DRBD configuration, initial Primary bootstrap, failover utility (`/usr/local/bin/drbd-failover.sh`), and watchdog service (`/etc/systemd/system/drbd-failover-watchdog.service`).
- `scripts/utils/test_failover.sh` — Automated chaos engineering test suite validating unattended master, frontend, and worker node failover.

### Verification

```bash
# Verify nodes in K3s
ssh ubuntu@192.168.10.11 "sudo -H kubectl get nodes"

# Verify that the MariaDB pod runs on internal-master1 (active label)
ssh ubuntu@192.168.10.11 "sudo -H kubectl get pods -n cms -o wide"

# Verify DRBD status on the primary master
ssh ubuntu@192.168.10.11 "sudo drbdadm status cms_data"

# Test connection to the database via NodePort
mysql -h 192.168.10.11 -P 30306 -u wp_user -p wordpress -e "SHOW TABLES;"
```
