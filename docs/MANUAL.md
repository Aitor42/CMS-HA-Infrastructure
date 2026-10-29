# Operations Manual — CMS High-Availability Infrastructure

> **Version:** 2.1  
> **Date:** October 2026

This manual covers deployment, verification, monitoring, maintenance and failover for the WordPress CMS high-availability lab. The lab runs on a single KVM hypervisor.

---

## Table of Contents

1. [Prerequisites](#1-prerequisites)
2. [Deployment Procedure (Batch Mode)](#2-deployment-procedure-batch-mode)
3. [Deployment Verification](#3-deployment-verification)
4. [Monitoring and Metrics](#4-monitoring-and-metrics)
5. [Scaling and Node Expansion](#5-scaling-and-node-expansion)
6. [Maintenance and Backups](#6-maintenance-and-backups)
7. [Failover and Disaster Recovery](#7-failover-and-disaster-recovery)
8. [Lab Resume Procedure](#8-lab-resume-procedure)

---

## 1. Prerequisites

### 1.1 Hypervisor Host Hardware
*   **CPU**: Minimum 4 physical cores (recommended: 8+ vCPUs).
*   **RAM**: 16 GB minimum for the reduced topology (`config.ci.yaml`; the hosted CI runner uses 15 GiB plus zram); 27 GB for the full 16-VM lab.
*   **Disk**: Minimum 60 GB free in `/home` (VM disks are stored at `/home/$USER/vm_storage` to avoid filling the root partition).
*   **Network**: At least 1 interface with internet access for NAT.

### 1.2 Required Software on the Hypervisor
The physical host must have the KVM hypervisor and libvirt library installed:
```bash
sudo apt update
sudo apt install -y qemu-kvm libvirt-daemon-system virtinst bridge-utils
```
The user must belong to the `libvirt` group to manage VMs without direct root privileges:
```bash
sudo usermod -aG libvirt $USER
newgrp libvirt
```

Disk images are created mode `0600` and handed to the qemu account by libvirt
**dynamic ownership**. Keep `dynamic_ownership = 1` in
`/etc/libvirt/qemu.conf` (the default); `init-vms` checks it and fails before
creating VMs when it is explicitly disabled. It also applies a default ACL to
`vm.storage_dir` so PXE disks created by `virt-install` inherit qemu access and
no world permissions. The deploy account still needs passwordless `sudo` as a
fallback for a disk that libvirt chowns before the orchestrator can restrict
it; without sudo, the default ACL is what keeps the PXE path working.

### 1.3 CLI Behaviour and Configuration Validation (v2.1)

*   **Error handling**: command errors are printed once to **stderr** and the process exits with code `1`. Nothing is retried silently; failures surface where they occur.
*   **`--dry-run` works for `cms-ha deploy` and `cms-ha phase`.** Phase dry-runs load and validate the configuration read-only, then report the phase without opening SSH or libvirt connections; they never auto-encrypt `config.yaml`. Other commands reject the flag up front.
*   **Configuration validation** runs on every `cms-ha` invocation before any network or VM operation. The load aborts with a precise message (naming the offending node) when:
    - `network.internal.cidr` / `network.main.cidr` are not valid CIDRs;
    - any node IP is not a valid IPv4 address, FQDN contains characters outside letters/digits/dots/hyphens, or a MAC address is malformed — these fields are interpolated into SSH commands, Cobbler registrations and `awk` filters;
    - the hotdesk IP range would overflow (base octet + count must stay ≤ 254, e.g. 40 hotdesks from octet 250 is rejected before generating `192.168.20.256`);
    - `vm.storage_dir` is missing, fewer than 3 master nodes (or an even number) are configured (etcd quorum), the LB IP is unset, or there is no CMS frontend.
*   **`NUM_HOTDESKS`** overrides the hotdesk count dynamically; an invalid value is ignored with a `WARN` instead of silently altering the topology.

### 1.4 Security Hardening (Non-Root Access, Host Keys, Scanning)

*   **Non-root deploy account.** `cms-ha` never logs in as `root`. Every VM connection uses `ssh.user` (default `ubuntu`, provisioned by the installers with an authorised key) and privileged commands are elevated transparently with `sudo -n -H`. Direct root SSH is disabled on every provisioned node (`PermitRootLogin no`).
*   **Host-key pinning (TOFU).** VM host keys are pinned on first contact and stored in `ssh.known_hosts_file` (default `~/.ssh/cms_known_hosts`). A later mismatch **fails closed** instead of silently trusting a changed key (MITM protection).
*   **Secrets at rest.** Service passwords and `backup.secret_key` are age-encrypted on first load; the identity and recipient live outside the repository (`${HOME}/.config/cms-ha`).
*   See `docs/SECURITY.md` for the full model, the accepted risks, and the CI scanner configuration.

---

## 2. Deployment Procedure (Batch Mode)

The Ubuntu installer needs 3–4 GB per node, so the 16 VMs cannot all install at once on a 27 GB host. The initial installation therefore runs in **batches**.

### Before the First Deployment: Set Service Passwords

Edit `config.yaml` and enter the desired service passwords in plain text:

```yaml
database:
  password: "<wp_user-password>"
  root_password: "<root-password>"
  exporter_password: "<exporter-password>"

wordpress:
  admin_password: "<admin-password>"

pki:
  provisioner_password: "<ca-password>"
```

No separate encryption command is needed. The first `cms-ha` operation that loads the config creates a local `age` key outside the repository, encrypts the non-empty secret fields in `config.yaml` in place, and then loads the decrypted values for deployment. Keep `~/.config/cms-ha/age.key` safe and backed up: without it the encrypted configuration cannot be used. Do not commit `config.yaml` until the first `cms-ha` operation that loads it has completed.

The private key must be mode `0600`; `cms-ha` rejects a key that is readable by group or other. To rotate the age identity and re-encrypt the config, run `cms-ha secrets rotate` (or `make secrets-rotate`). It writes `config.yaml.bak-<timestamp>` and keeps the previous key as `age.key.bak-<timestamp>`. This does **not** rotate the service passwords; use the per-service procedure below and re-run the affected phases. If a new key is supplied with `--new-key`, install it at `~/.config/cms-ha/age.key` yourself before the next load.

K3s is installed with Secret encryption at rest. The option applies to new clusters; it does not migrate an existing one.

Changing `database.password` or `database.exporter_password` later requires rerunning the database initialization Job and Puppet setup so MariaDB and the clients receive the same credentials. `database.root_password` initializes the MariaDB data directory; changing it in the config does not rotate the root account in an existing persistent volume. Rotate that account in MariaDB first, then update the config, or the initialization Job will no longer be able to authenticate. `wordpress.admin_password` is used only during the first WordPress installation; changing it later does not change the existing administrator account. Keep `pki.provisioner_password` stable after CA initialization because it unlocks the existing CA key.

### Step 1: Provision the Jumpstart Node and Configure Cobbler
On the hypervisor terminal, create the virtual networks and install the central provisioning node (`jumpstart`):
```bash
# 1. Create networks and launch the jumpstart VM
./cms-ha phase init-vms --jumpstart-only

# 2. Configure Cobbler server, DHCP, and NFS
./cms-ha phase setup-cobbler

# 3. Register client node templates and profiles in Cobbler
./cms-ha phase register-nodes
```

**Cloud-image jumpstart (hosted workflow default).** The ISO/autoinstall path above is the CLI/config default and the deliverable. The hosted `Integration (KVM lab, hosted)` workflow defaults to `jumpstart_image=cloud` because it is the fastest way to exercise the rest of the pipeline. For local iteration, `--jumpstart-image cloud` (or `vm.jumpstart_image: cloud` for a persistent choice) boots a qcow2 overlay of the official Ubuntu cloud image instead: the base system is already installed, cloud-init applies the same hostname, MACs, IPs and SSH key, and the node is reachable in well under a minute instead of ~4 minutes. The run pays for the base image once and shares it read-only across overlays:

```bash
cd "${HOME}/vm_storage"
curl -LO https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img

./cms-ha phase init-vms --jumpstart-only --jumpstart-image cloud
```

Set `vm.jumpstart_base_image` if the image lives elsewhere. Cobbler, PXE, the client install and every later phase are unchanged. The ISO/PXE run remains the gate: use the self-hosted `Integration (KVM lab)` workflow, or dispatch the hosted one with `jumpstart_image=iso` and `fast_provisioning=false`. Measure both with `--timings-file` before changing the CLI/config default.

**Cloud-image client nodes (hosted workflow default).** `--fast-provisioning` applies the same base image to every client node, so the lab skips the PXE installs entirely. Each VM gets a qcow2 overlay and its own cloud-init seed ISO (hostname, MACs, static addresses, SSH key); everything from `03-repair-ssh` onwards is unchanged. The hosted workflow defaults to `fast_provisioning=true`; the CLI/config default remains PXE. Use it on `deploy`, or persist the choice for clients with `vm.provisioning: cloud`:

```bash
# Jumpstart + all clients from the cloud image; PXE is not used
./cms-ha deploy --fast-provisioning

# Client nodes only, reusing an existing jumpstart
./cms-ha phase init-vms --nodes-only --fast-provisioning
```

Fast provisioning needs the cloud image from the previous step. The Cobbler and `vm install-batches` steps can be skipped in this mode because no node boots the installer; `deploy` still runs the Cobbler phases so later phases see the same topology. The PXE run with `vm.provisioning: pxe` remains the deliverable gate; the hosted workflow reproduces it with `jumpstart_image=iso` and `fast_provisioning=false`. Measure both with `--timings-file`.

### Step 2: Sequential Installation and RAM Adjustment (Batches)

Run the command that automates the creation and installation of client nodes in sequential batches:
```bash
./cms-ha vm install-batches
```
**Operational notes:**
*   **Installation RAM:** never install below 3 GB. `config.yaml` uses 4 GB on masters and workers; `config.ci.yaml` uses 3 GB everywhere so five installers fit a 15 GiB CI runner.
*   **Batch width:** `deploy.install_batch_size` (default `0`) keeps the documented batches (management, Kubernetes, frontends). Set it to the hypervisor's vCPU count to balance them; the hosted lab uses `4`.
*   **Memory shrinking:** after a batch finishes installing, its RAM is reduced to the production profile (768 MB – 2048 MB) and the batch stays shut down until the last one finishes. Re-apply later with `./cms-ha vm shrink`.
*   **Boot order:** `./cms-ha vm fix-boot-order` makes rebooted VMs boot from disk instead of PXE.
*   **Install timeout:** `deploy.node_install_timeout` (default `15m`) is a stall guard per batch; the hosted 4-vCPU lab uses `25m`.
*   **Puppet catalog width:** `deploy.puppet_concurrency` caps how many nodes run their first catalog at once. `0` (default) derives it from the host CPU count (4–8); `PUPPET_CONCURRENCY` overrides both. Most of a catalog is guest-side work, so a wider window trims the tail without saturating the puppetserver.
*   **Stagger:** `deploy.stagger_delay` pauses between phases. It defaults to `5s` for slow interactive hosts; CI sets `0s` because the phases already run against separate guests.
*   **Installed system vs installer:** the live installer answers on port 22 but refuses the deployment key. A node that goes silent after its installer answered has a wedged shutdown; the batch restarts it and, because boot order is already disk-first, it returns to the installed system.

The following screenshot shows the automated PXE network installation process via Cobbler:

![PXE Installation](pxe-installation.png)

### Step 3: Service and Application Deployment
Once all VMs are running and adjusted to production profiles, run the deployment orchestrator:
```bash
# Go CLI (VMs already installed; skips VM creation):
./cms-ha deploy --skip-vm-create

# Full run (creates the networks and VMs first):
./cms-ha deploy

# The orchestrator writes a checkpoint (deploy.state) after every phase, so a
# failed run can resume instead of starting over. A second concurrent deploy
# fails fast because deploy.state.lock is held:
./cms-ha deploy --resume                    # skip phases already completed
./cms-ha deploy --from-phase drbd-setup     # start at a named phase
./cms-ha deploy --state-file /var/tmp/deploy.state

# Record per-phase and per-sub-step durations (JSON) for before/after
# comparisons. It is written even when a phase fails. The report carries the
# same run_id as deploy.state, and every phase/step has a status and duration:
./cms-ha deploy --timings-file integration-artifacts/timings.json

# Pin the identifier when correlating with an external CI job or ticket:
./cms-ha deploy --run-id ci-1234 --timings-file integration-artifacts/timings.json

# (Or Makefile: make deploy-resume)
```

Phase names for `--from-phase` are the normalised phase names (e.g.
`00-init-vms`, `drbd-setup`, `kubernetes-setup`, `monitoring-setup`); an
unknown value is rejected with the list of valid phases.

**Orchestration in short:**
1. The jumpstart node converges first, so Puppet Server, DNS, Cobbler, DHCP and TFTP are up before any client runs.
2. Clients converge in parallel with automatic recovery from stale Puppet SSL state (private-key mismatch or signature/verify failure); the window is `deploy.puppet_concurrency` (auto: 4–8 by CPU count, or `PUPPET_CONCURRENCY`). Each node converges once, in phase 04.
3. `bootstrap-router` gives the router static addresses, IP forwarding and NAT before clients download packages.
4. DRBD is promoted and mounted on `internal-master1`; K3s then resolves the current DRBD primary and labels that node `drbd-status=primary` so `mariadb-0` can mount the volume.
5. In phase 07 (Nginx/WordPress), the CMS frontends converge sequentially (the first one creates the WordPress schema); monitoring, UFW and the CA fan out in parallel. This sequential CMS convergence is not part of the parallel Puppet phase.
6. Every remote exit code is checked: a broken DRBD/K3s state aborts the deploy instead of reporting success.

---

## 3. Deployment Verification

### 3.1 VM Status on the Hypervisor
```bash
virsh -c qemu:///system list --all
# You should see 16 active VMs (jumpstart, ufw-router, monitor, storage, masters, workers, lb1, lb2, cms, hotdesks)
```

### 3.2 Network Connectivity and Isolation (DTE-to-DTE Tests)
1.  **Internal Connectivity**: `ping` from `internal-master1` (`192.168.10.11`) to `jumpstart` (`192.168.10.10`) → **Allowed** (0% loss).
2.  **Allowed Inter-VLAN Connectivity**: `ping` from `internal-master1` (`192.168.10.11`) to the LB VIP (`192.168.20.100`) → **Allowed** (routed).
3.  **Security Isolation (Main → Internal)**: `ping` from a hotdesk (`192.168.20.201`) to `internal-master1` (`192.168.10.11`) → **Denied** by the node firewall (UFW has no ICMP rule for `main`).
4.  **Internet Access**: `ping -c 3 8.8.8.8` from any node → **Allowed** (NAT masqueraded by the router).

### 3.3 WordPress CMS Service Verification
Send an HTTPS request to the load balancer to verify the CMS is responding:
```bash
curl -sk https://192.168.20.100/ | grep -i "wordpress"
```

### 3.4 Full Infrastructure Health Check

The health check validates all infrastructure phases. The Go CLI executes these checks concurrently using the SSH connection pool, and finishes in a few seconds (the end-to-end HTTP check dominates: it loads the CMS through the load-balancer VIP a hundred times to prove the request path, not just that its units started):

```bash
# Go CLI (parallel execution across all phases):
./cms-ha verify

# (Or Makefile: make verify)
```

**Go CLI Concurrent Verification Output:**
```text
ℹ Starting phase: Infrastructure Verification
✓ Phase 00 - Libvirt VMs & Networks: [PASS] (16 running VMs)
✓ Phase 01 - Cobbler Services & Systems: [PASS] (Systems: 15)
✓ Phase 02 - Puppet Server & Certs: [PASS] (Certs: 15)
✓ Phase 03 - Nginx & Apache & SSL: [PASS]
✓ Phase 04 - K3s & MariaDB Pod: [PASS]
✓ Phase 05 - Prometheus & Grafana: [PASS]
✓ Phase 06 - Router UFW & IP Forward: [PASS]
✓ Phase 07 - DRBD Status: [PASS] (Verified on 192.168.10.11)
✓ Phase 08 - End-to-end HTTP (CMS, Grafana, Prometheus): [PASS] (CMS: 100/100 requests OK (100%), Prometheus: healthy, 9 target(s) up, Grafana: database ok (v13.2.4))
✓ Phase 10 - Stack functional checks (CMS pages, REST API, frontends, load balancers, MariaDB, backup store, probes, Grafana): [PASS]
✓ Completed phase: Infrastructure Verification in 21.37s
```

### 3.5 Configuration Validation

Validate `config.yaml` without creating or changing any VM:

```bash
./cms-ha config validate
./cms-ha config validate --json
```

The JSON form is intended for CI and scripts; it returns `valid: false` and a
non-zero exit code for an invalid configuration.

`schema/config.schema.json` is generated from the Go types in `internal/config`
and is checked into the repository. Regenerate it after changing a config
field (the `tools/configschema` test fails on drift):

```bash
make schema
```

For VS Code with the Red Hat YAML extension, add this to your settings so
`config.yaml` and `config.ci.yaml` get completion and inline validation:

```json
{
  "yaml.schemas": {
    "./schema/config.schema.json": ["config.yaml", "config.ci.yaml"]
  }
}
```

### 3.6 Real-Time SSH Connectivity Matrix

To instantly inspect the state and SSH reachability of all provisioned virtual machines:

```bash
./cms-ha status ssh
./cms-ha status ssh --json
```

```text
VM NAME              IP              STATE      SSH       
------------------------------------------------------------
ufw-router           192.168.10.1    running    OK        
jumpstart            192.168.10.10   running    OK        
internal-master1     192.168.10.11   running    OK        
internal-master2     192.168.10.12   running    OK        
internal-master3     192.168.10.16   running    OK        
internal-worker1     192.168.10.13   running    OK        
internal-worker2     192.168.10.14   running    OK        
internal-storage     192.168.10.15   running    OK        
internal-monitor     192.168.10.20   running    OK        
main-lb1             192.168.20.103  running    OK        
main-lb2             192.168.20.104  running    OK        
main-cms1            192.168.20.101  running    OK        
main-cms2            192.168.20.102  running    OK        
main-hotdesk1        192.168.20.201  running    OK        
main-hotdesk2        192.168.20.202  running    OK        
main-hotdesk3        192.168.20.203  running    OK        
```

### 3.6 End-to-End Integration Test

`tests/integration/run.sh` (also `make integration`) drives the whole pipeline
on a real KVM host: **build → deploy → verify → traffic → drills → failover**.
It is the entry point of the `Integration (KVM lab)` workflow, which targets a
self-hosted runner labelled `kvm` and is manual only (`workflow_dispatch`).

```bash
make integration                 # deploy + verify + failover
make integration-traffic         # same, plus 45s of k6 load against the VIP
make integration-drills          # traffic and the operational drills
make integration-resume          # resume from the deploy.state checkpoint
tests/integration/run.sh --skip-deploy --skip-chaos   # verify only
```

It requires `/dev/kvm`, a working libvirt connection and a container runtime
(docker/podman). The checkpoint and the phase logs are uploaded as CI artifacts.

The same driver runs on a GitHub-hosted runner through
`.github/workflows/integration-hosted.yml` (nightly, release and manual dispatch). Hosted runners
expose nested KVM but are small (4 vCPU, 15 GiB), so that workflow deploys
`config.ci.yaml` (no hotdesks) and installs clients in batches of four. Its
dispatch defaults to the fast cloud path (`jumpstart_image=cloud`,
`fast_provisioning=true`); the PXE/Cobbler deliverable gate is the self-hosted
workflow, or a hosted dispatch with `jumpstart_image=iso` and
`fast_provisioning=false`.

---

## 4. Monitoring and Metrics

The `internal-monitor` node centralises metrics collection and visualisation:
*   **Prometheus**: `http://192.168.10.20:9090` (target status at `/targets`, firing alerts at `/alerts`).
*   **Grafana**: `http://192.168.10.20:3000` (User: `admin`). The admin password is generated per deployment by the monitoring phase and stored on the monitor node at `/etc/grafana/grafana-admin.env` (root:grafana, mode 0640); read it with `sudo cat /etc/grafana/grafana-admin.env`. To choose a fixed password instead, set `monitoring.grafana_admin_password` in `config.yaml` (it is encrypted like every other password). Grafana does **not** use the default `admin/admin`. Two dashboards are provisioned: **Node Exporter Overview** and **HA Overview** (DRBD replication/role, etcd quorum, keepalived VIP ownership, blackbox probes and k6 load metrics). Grafana opens straight on the **HA Overview** (`[dashboards] default_home_dashboard_path`, set through a systemd drop-in by `role::monitor`), so the first page after login shows live graphs instead of the welcome screen. The verification walks Grafana's API with that credential: both dashboards must be loaded, every panel must point at the provisioned `prometheus-cms-infra` datasource, and the HA Overview must be the home dashboard; after the k6 run, the traffic phase evaluates every panel query against Prometheus and fails if one would render "No data".
*   **Alertmanager**: `http://192.168.10.20:9093`. Receives firing alerts from Prometheus, groups and deduplicates them, and exposes them through its UI and `amtool`. It ships with a no-op receiver, so alerts are visible but not delivered out of the box: add a webhook/email/Slack receiver in `/etc/prometheus/alertmanager.yml` (managed by Puppet, `role::monitor`) and let Puppet re-apply.

**Where the monitoring configuration lives.** The scrape configuration, the Alertmanager and blackbox modules, and the Grafana provisioning/dashboards are managed by **Puppet** (`role::monitor`; files under `puppet/modules/role/files/`). The alert rules are deployed by the monitoring phase to `/etc/prometheus/alert-rules.yml`, which Puppet's `prometheus.yml` loads via `rule_files`.

**Alert rules (19).** Availability (nodes, MariaDB/Nginx/Apache exporters, HTTP 5xx), resource pressure (disk/memory), backup/restore health, and the high-availability set: `EtcdNoQuorum` (fewer than two of the three etcd members reachable), `DRBDPeerDown`, `DRBDSplitBrain`, `DRBDNoPrimary` and `KeepalivedState` (the VIP must be held by exactly one balancer). DRBD and keepalived metrics are published through node_exporter's textfile collector, so no extra exporter is required; two systemd timers write the `.prom` files every minute (`cms-drbd-metrics.timer` on `internal-master1/2`, `cms-keepalived-metrics.timer` on both balancers). `cms-ha verify` fails if Prometheus has no `cms_drbd_connected` or `cms_keepalived_vip_present` series, because a stopped publisher leaves the corresponding panels blank and the `DRBDPeerDown`/`KeepalivedState` alerts silent without tripping any other probe.

**Load testing.** `./cms-ha traffic` runs a k6 profile in a pinned `grafana/k6` container against the VIP or an internal frontend and reports RPS, p95/p99 and error rate. With `traffic.prometheus_rw_url` set in `config.yaml`, those metrics are streamed into Prometheus so they appear on the HA Overview dashboard during the test; `role::monitor` starts Prometheus with `--web.enable-remote-write-receiver` so the endpoint accepts them. k6's remote-write output has no histograms by default: each trend becomes one gauge per requested statistic (`k6_http_req_duration_p95`), which is why the phase requests `K6_PROMETHEUS_RW_TREND_STATS=p(95),p(99)` and the panels query those gauges rather than `_sum`/`_count` series that only exist for native histograms. Once the run is over, the phase evaluates every panel query of every shipped dashboard against Prometheus and fails if one returns no series — the same answer the browser renders as "No data". The integration driver runs the profile after verify when it is called with `--with-traffic`.

### Critical Monitored Metrics
*   **Infrastructure**: CPU usage (`node_cpu_seconds_total`), available memory (`node_memory_MemAvailable_bytes`), disk usage (`node_filesystem_avail_bytes`), and network traffic (`node_network_receive_bytes_total`).
*   **Services**: Nginx active connections (`nginx_connections_active`), Apache workers (`apache_workers`), MariaDB queries (`mysql_global_status_queries`), and K3s pod replicas.

### Alertmanager Notifications

Prometheus forwards every firing alert to Alertmanager, which groups and
deduplicates them, but the shipped configuration uses a **no-op receiver**: by
default nothing leaves the node. To be notified (webhook, email, Slack, …) you
add a receiver to the Alertmanager configuration.

**The file is managed by Puppet.** The source of truth lives in the repository;
editing `/etc/prometheus/alertmanager.yml` directly on `internal-monitor` gets
overwritten on the next Puppet run:

```bash
# 1. Edit the source of truth (repository root)
$EDITOR puppet/modules/role/files/prometheus/alertmanager.yml

# 2. Re-converge internal-monitor (Puppet server is jumpstart)
ssh internal-monitor 'sudo puppet agent -t'

# 3. Validate and reload on internal-monitor
ssh internal-monitor
amtool check-config /etc/prometheus/alertmanager.yml
sudo systemctl reload prometheus-alertmanager
```

Replace the `default` receiver with one of the following. Keep the name
`default`, or point the existing `route` at your own receiver.

**Webhook** — any HTTP endpoint that accepts the Alertmanager JSON payload:

```yaml
receivers:
  - name: 'default'
    webhook_configs:
      - url: 'https://hooks.example.com/alertmanager'
        send_resolved: true
```

**Email** — requires a reachable SMTP relay:

```yaml
receivers:
  - name: 'default'
    email_configs:
      - to: 'ops@example.com'
        from: 'alertmanager@internal.local'
        smarthost: 'smtp.example.com:587'
        auth_username: 'alertmanager@example.com'
        auth_password: 'CHANGE_ME'
        require_tls: true
```

**Slack** — via an incoming-webhook URL:

```yaml
receivers:
  - name: 'default'
    slack_configs:
      - api_url: 'https://hooks.slack.com/services/XXX/YYY/ZZZ'
        channel: '#alerts'
        send_resolved: true
```

**Send a test alert** and confirm it reaches the receiver (it also shows up at
`http://192.168.10.20:9093`):

```bash
ssh internal-monitor
amtool alert add alertname=TestAlert severity=warning
amtool alert query
```

To throttle or split notifications, extend `route` with child routes. The
`matchers` syntax requires Alertmanager ≥ 0.22, which Ubuntu 22.04 ships:

```yaml
route:
  receiver: 'default'
  routes:
    - matchers: [ severity = "warning" ]
      receiver: 'default'
      repeat_interval: 12h
```

> **Secrets.** `alertmanager.yml` is committed to the repository, so do not
> hard-code real tokens or passwords: treat webhook/Slack URLs and SMTP
> credentials as secrets, keep them out of the repo, and rotate them if they
> leak.

---

## 5. Scaling and Node Expansion

### 5.1 Adding a New Hot-desk Workstation (e.g., `main-hotdesk4`)

The simplest path is to raise `hotdesks.count` in `config.yaml` and re-run
`cms-ha deploy` (or `cms-ha vm install-batches`). The manual steps below are the
equivalent when the VM must be created on its own.

1.  **Register in Cobbler (on the `jumpstart` VM)**:
    ```bash
    cobbler system add \
      --name=main-hotdesk4 \
      --hostname=main-hotdesk4.main.local \
      --mac=52:54:00:10:02:cc \
      --ip-address=192.168.20.204 \
      --profile=ubuntu-24.04-x86_64 \
      --interface=ens3 \
      --static=1
    cobbler sync
    ```
2.  **Create and start the VM on the hypervisor**:
    ```bash
    sudo virt-install \
      --connect qemu:///system \
      --name=main-hotdesk4 \
      --ram=768 --vcpus=1 \
      --disk path=/home/$USER/vm_storage/main-hotdesk4.qcow2,size=10,format=qcow2,bus=virtio \
      --network network=main,mac="52:54:00:10:02:cc",model=virtio \
      --pxe \
      --boot network,hd \
      --os-variant=ubuntu24.04 \
      --noautoconsole --wait=0
    ```
3.  The VM will PXE boot, install unattended, and self-configure through Puppet.

---

## 6. Maintenance and Backups

### 6.1 Database Backups (MariaDB)

**Automated backups** run daily at 02:00 UTC via a Kubernetes CronJob (`kubernetes/backups/mariadb-backup-cronjob.yaml`):

1. `mysqldump` writes a compressed dump to the local staging volume
   (`mariadb-backup-pvc`, `/mnt/data/mariadb-backups`), rotating files older
   than `backup.retention_days` (default 7).
2. An `rclone` container uploads the dump to the **off-DRBD Garage S3 bucket**
   (`backup.bucket`, default `cms-backups`) running as a container on the
   hypervisor, and deletes bucket objects older than the retention window.

Because the authoritative copies live **outside the DRBD device**, a
split-brain on the replicated volume cannot take the backups with it.

**Restore drill.** A second CronJob (`mariadb-restore-test-cronjob.yaml`) runs
daily at 03:30 UTC: it downloads the newest object from the bucket, restores it
into a throwaway MariaDB instance, and validates that the WordPress schema was
recovered (`wp_posts`, `wp_options`, `wp_users`). It records the restore time
and duration as metrics.

The store runs on the host network and binds the S3 API directly on the
hypervisor's internal address (never the WAN); the other Garage APIs stay on
loopback. Do not switch it back to Docker port publishing: the backup CronJob
runs inside the cluster, and a published port is not reachable from the libvirt
guests in the CI lab. `cms-ha verify` checks the endpoint from `internal-master1`
(expects Garage's anonymous `403`) so a store that only answers on the
hypervisor fails the deploy instead of the nightly backup.

**Alerts.** Both jobs publish metrics through the node-exporter textfile
collector:

- `BackupMissing` — no successful offsite backup in the last 26 hours.
- `BackupRestoreTestFailing` — the restore drill has not succeeded in 48 hours.

**Configuration.** The endpoint/bucket/region are injected into a
`backup-config` ConfigMap and the credentials into a `backup-secret` Secret by
the `kubernetes` phase, all derived from the `backup:` block in `config.yaml`
(the S3 secret must be encrypted: `cms-ha secrets encrypt`).

To perform a manual backup:
```bash
# Via Go CLI (writes a local .sql file):
./cms-ha backup db

# Or direct command from the internal network:
mysqldump -h 192.168.10.11 -P 30306 -u root -p wordpress | gzip > backup_wordpress.sql.gz

# Or pull the latest offsite copy with rclone (any node):
rclone --s3-endpoint "http://192.168.10.254:3900" --s3-provider Other \
  ls s3:cms-backups
```

### 6.2 WordPress Media and File Backups
Uploaded media (`wp-content/uploads`) is centrally stored on the NFS server `internal-storage` at `/srv/nfs/wp-uploads` and mounted across both frontend nodes.

To back up the shared uploads volume:
```bash
# On internal-storage (192.168.10.15):
tar -czf wp-uploads-backup.tar.gz /srv/nfs/wp-uploads/
```

To back up application files on either frontend node:
```bash
tar -czf wp-files-backup.tar.gz --exclude='wp-content/uploads' /var/www/html/
```

### 6.3 Operational Drills

The deploy and verify phases prove that the cluster is serving, but they never
run the paths that only fire on a timer. `cms-ha test drills` exercises them for
real:

```bash
./cms-ha test drills
```

- **Backup and restore**: creates one-off Jobs from the `mariadb-backup` and
  `mariadb-restore-test` CronJobs, waits for both, and asserts that Prometheus
  received fresh `cms_backup_last_success_timestamp_seconds` and
  `cms_backup_restore_last_success_timestamp_seconds` series. A drill job that
  fails is reported with the Job, Pod and container logs.
- **TLS renewal**: runs the production `cms-tls-renew.sh` on the backup
  balancer with `RENEW_FORCE=1`, so the CA is really contacted over mTLS and
  the certificate is really rotated (step rejects an `--expires-in` above the
  certificate lifetime, so forcing is the deterministic path). The drill
  compares the certificate **serial** before and after the run, because a
  script that silently reports "not due" would otherwise pass; it then checks
  that nginx is active and the balancer answers HTTPS.
- **Alert hygiene**: asserts that no alert is firing on the healthy lab, which
  catches rules that are permanently noisy or a stale backup/restore metric.

The integration pipeline enables it with `--with-drills` after the traffic test.

---

## 7. Failover and Disaster Recovery

### 7.1 Master 1 Failure (DRBD/K3s Primary)

The MariaDB data directory (`/mnt/data/mariadb`) lives directly on the replicated DRBD device (`/dev/drbd0`) and is mounted on the active Primary node, so replication is synchronous. The MariaDB ConfigMap disables InnoDB's native AIO, which is not reliable on DRBD+qcow2 volumes (see section 8.3d).

If `internal-master1` goes down completely, the MariaDB pod on it becomes unreachable and the DRBD watchdog on `internal-master2` takes over automatically: it fences the failed domain, promotes DRBD, mounts the replicated volume, moves the `drbd-status=primary` label and force-deletes the zombie pod so the StatefulSet recreates it on the surviving master. Nothing needs to be done by hand, and the pod cannot simply be deleted gracefully: while the old node is unreachable the pod object stays in `Terminating` and the StatefulSet will not create a replacement.

If that automated path ever fails, the manual fallback is:
```bash
# 1. Check cluster status from master2
ssh ubuntu@192.168.10.12 'sudo -H kubectl get nodes; sudo -H kubectl get pods -n cms'

# 2. Force-delete the zombie pod
ssh ubuntu@192.168.10.12 'sudo -H kubectl delete pod mariadb-0 -n cms --force --grace-period=0'

# 3. If MariaDB does not start on master2, promote DRBD there and migrate the PV
# (requires manual intervention — see full procedure in section 8)
```

### 7.2 Jumpstart Node Shutdown
Once the entire infrastructure is deployed, the `jumpstart` node can be safely shut down to free CPU/RAM resources on the physical server:
```bash
virsh -c qemu:///system shutdown jumpstart
```
The CMS infrastructure (WordPress, Nginx, MariaDB, K3s, Monitoring) will continue operating normally without depending on the provisioning server.

> **Important:** If `jumpstart` is shut down, the internal NTP server becomes unavailable. Node clocks may drift if they remain powered off for extended periods. Restart it before resuming the lab to ensure clock synchronisation.

### 7.3 Automated Failover Testing
Run the chaos engineering test suite to validate HA behaviour:
```bash
# Via Go CLI:
./cms-ha test failover

# (Optional: inspect state without auto-restoring)
./cms-ha test failover --skip-restore
```
This tests DRBD master failover (graceful and power-cut), CMS frontend failover, K3s worker failover, and load-balancer VIP failover with timing metrics. The DRBD scenarios read the role from **both** masters and take down whichever one actually holds `Primary` (never a Secondary), after checking that the survivor's `drbd-failover-watchdog` is active and that `mariadb-0` is Running and **Ready** on the writer; they then assert that the pod comes back Running and Ready on the survivor. The worker scenario asserts that `mariadb-0` stays Running and Ready on a master and that the VIP keeps serving — this design pins the stateful workload to the DRBD primary (node affinity + hostPath), so it is not expected to reschedule onto a worker. The VIP scenario stops whichever balancer currently owns the floating address, asserts that the survivor takes it over and serves **HTTPS** (a plain-HTTP redirect is not accepted), then restarts the node and waits until both balancers are reachable and exactly one owns the VIP again. Scenarios that cannot run (fewer than 2 masters, 2 frontends, 2 workers or 2 balancers) are reported as **`SKIP`** — they are no longer counted as failures. Every scenario verifies that the node actually reached `shut off`, and a restore is only successful once the node accepts SSH again; a failed restore fails the scenario instead of passing. With `--skip-restore` the failed node is intentionally left down, so later DRBD scenarios report **`SKIP`** when the pair can no longer be inspected and the post-restore VIP check is omitted. Cancelled runs return an error instead of a silent success.

---

## 8. Lab Resume Procedure

This section describes the procedure to **resume the lab** when VMs have been paused (`virsh suspend`) or shut down (`virsh shutdown` / `shut off`).

### 8.1 Quick Procedure (automated CLI)

From the hypervisor server, run in order:

```bash
# 1. Start all VMs (paused → resume, stopped → start)
./cms-ha vm start

# 2. Wait ~60s for K3s nodes to become ready, then repair the cluster
./cms-ha repair k8s

# 3. Synchronise clocks across all nodes
./cms-ha repair clocks

# 4. Verify full infrastructure status
./cms-ha verify
```

### 8.2 Detailed Procedure (step-by-step)

#### Step 1 — Resume Paused VMs
```bash
# Resume all paused VMs at once
for vm in ufw-router jumpstart internal-monitor main-lb1 main-lb2 main-cms1 main-cms2 main-hotdesk1 main-hotdesk2 main-hotdesk3; do
  sudo virsh resume "$vm" 2>/dev/null && echo "OK: $vm"
done
```

#### Step 2 — Start Stopped VMs (K3s nodes)
```bash
# Order matters: masters first, then workers and storage
for vm in internal-master1 internal-master2 internal-master3 internal-storage internal-worker1 internal-worker2; do
  sudo virsh start "$vm" && sleep 6
done
```

#### Step 3 — Repair K3s Cluster After Extended Pause
```bash
./cms-ha repair k8s
```
It synchronises the clocks, cleans up stale K3s processes, restarts K3s on the masters and workers to recover etcd quorum, and verifies that all five nodes reach `Ready`.

#### Step 4 — Verify MariaDB
```bash
# The pod should reach 1/1 Running within ~2 minutes
ssh ubuntu@192.168.10.11 'sudo -H kubectl get pod mariadb-0 -n cms -w'
```

If MariaDB enters `CrashLoopBackOff`, see section 8.4.

### 8.3 Permanent Boot-time Configurations

The deploy installs the following configurations; they activate on every boot without manual intervention:

#### a) `drbd-cms-boot.service` (on both masters)

Location: `/etc/systemd/system/drbd-cms-boot.service` and
`/usr/local/sbin/drbd-boot.sh` on `internal-master1` and `internal-master2`.

```ini
[Unit]
Description=Resolve the DRBD cms_data role before k3s starts
After=drbd.service network-online.target
Wants=drbd.service network-online.target
Before=k3s.service

[Service]
Type=oneshot
RemainAfterExit=yes
Environment=PROMOTE=1        # 0 on internal-master2
ExecStart=/usr/local/sbin/drbd-boot.sh

[Install]
WantedBy=multi-user.target
```

**Effect:** brings the resource up and resolves its role before K3s starts. A
node that reboots still carries its previous role in the DRBD metadata: after a
power cut, a hypervisor crash or a fence, that role can be `Primary` even though
the peer took over in the meantime. Attaching as `Primary` in that situation is
a split brain (`after-sb-2pri disconnect` leaves two disconnected writers, and
both watchdogs would mount the volume). The script therefore demotes and
reconnects when it finds `Primary` with `cstate=StandAlone`, lets
`after-sb-1pri discard-secondary` resync the disk from the peer, and only then
promotes — and only `internal-master1`, and only if DRBD accepts (it refuses
while the peer is `Primary`). The old promote-only `drbd-cms-primary.service` is
removed and disabled by the deploy.

```bash
# Verify the service status
systemctl status drbd-cms-boot.service
journalctl -u drbd-cms-boot.service -b
```

#### b) MariaDB Data Directory Guarantee (`tmpfiles.d`)

Location: `/etc/tmpfiles.d/mariadb-datadir.conf` on `internal-master1`

```
d /mnt/data          0755 root root -
d /mnt/data/mariadb  0755 999  999  -
```

**Effect:** The directory `/mnt/data/mariadb` (owner `mysql` UID=999) is automatically created on every boot even if the system loses it.

#### c) Internal NTP — All Nodes Synchronise to Jumpstart

All cluster nodes use `192.168.10.10` (jumpstart) as their primary NTP server.
Configuration applied in `/etc/chrony/chrony.conf` on each node:

```
server 192.168.10.10 iburst prefer
makestep 1.0 -1
```

`makestep 1.0 -1` forces an **immediate** clock correction upon resuming from pause, instead of the default gradual slew. This prevents TLS authentication and etcd failures caused by clock skew.

On `jumpstart` (`/etc/chrony/chrony.conf`):
```
allow 192.168.10.0/24
allow 192.168.20.0/24
local stratum 8
```

#### d) MariaDB ConfigMap — InnoDB without `io_uring` (in K3s)

InnoDB's native AIO (`io_uring`) is not reliable on DRBD+qcow2 block devices and can corrupt `ibdata1` during initialisation. The `cms` namespace therefore ships a ConfigMap that disables it and forces `fsync` flushing:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: mariadb-config
  namespace: cms
data:
  custom.cnf: |
    [mysqld]
    innodb_use_native_aio=0
    innodb_flush_method=fsync
    innodb_flush_log_at_trx_commit=2
```

Mounted in the StatefulSet as `/etc/mysql/conf.d/custom.cnf`.

```bash
# Verify the ConfigMap exists
ssh ubuntu@192.168.10.11 'sudo -H kubectl get configmap mariadb-config -n cms'
```

#### e) Automatic DRBD Failover Watchdog (`drbd-failover-watchdog.service`)

Location: `/etc/systemd/system/drbd-failover-watchdog.service` and `/usr/local/bin/drbd-watchdog.sh` (active on both `internal-master1` and `internal-master2`).

```ini
[Unit]
Description=DRBD Automatic Failover Watchdog Daemon
After=network-online.target drbd.service drbd-cms-boot.service
Wants=network-online.target drbd.service drbd-cms-boot.service
Before=k3s.service

[Service]
Type=simple
ExecStart=/usr/local/bin/drbd-watchdog.sh
Restart=always
RestartSec=3
KillMode=process
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

**Functionality & High Availability Guarantees:**
- **Continuous Health Polling:** Evaluates DRBD replication status and peer node responsiveness every 5 seconds.
- **Anti-Split-Brain Quorum Verification:**
  - *Default Gateway Reachability (`192.168.10.1`):* If the local node cannot reach the gateway, failover is suppressed to prevent split-brain during network partition events where the local node is isolated.
  - *Peer Heartbeat (ICMP):* Verifies the peer master node is completely unreachable before taking action, avoiding failovers during transient DRBD replication reconnections.
  - *Debounce Strike Counter:* Requires 3 consecutive failed checks (15 seconds) before triggering failover promotion.
- **Autonomous Recovery Actions:**
  - Invokes `/usr/local/bin/drbd-failover.sh promote` on the healthy node.
  - Promotes DRBD resource `cms_data` to Primary.
  - Mounts `/dev/drbd0` on `/mnt/data/mariadb`.
  - Reassigns Kubernetes label `drbd-status=primary` to the local node.
  - Triggers reschedule of the MariaDB StatefulSet pod to the active storage node.
- **Autonomous Recovery & Split-Brain Healing:**
  - When the offline master node powers back on, `drbd-cms-boot.service` stands it down if its metadata still says `Primary` while the peer also holds `Primary` (`cstate=StandAlone`), then reconnects so DRBD kernel handlers (`after-sb-1pri discard-secondary`) resynchronise block differences. The restarted node ends up as the Secondary; it is checked by the failover test (both the graceful and the hard power-cut scenario).
  - The watchdog on the Secondary master node ensures `/mnt/data/mariadb` is unmounted and clears any stale `drbd-status=primary` labels from Kubernetes.

```bash
# Check watchdog daemon status on any master node
systemctl status drbd-failover-watchdog.service

# View watchdog real-time audit logs
journalctl -u drbd-failover-watchdog.service -f
```

### 8.4 Troubleshooting Common Issues After Boot

#### MariaDB in `CrashLoopBackOff` with InnoDB Error

Symptom in logs (`kubectl logs mariadb-0 -n cms`):
```
[ERROR] InnoDB: The data file './ibdata1' has the wrong space ID.
[ERROR] InnoDB: Plugin initialization aborted with error Data structure corruption
```

Cause: the data directory contains files from an incomplete initialisation (the process was interrupted before InnoDB wrote the `ibdata1` header).

Solution:
```bash
# 1. On internal-master1, clean up corrupted data
ssh ubuntu@192.168.10.11
sudo rm -rf /mnt/data/mariadb/*
sudo chown 999:999 /mnt/data/mariadb

# 2. Force pod recreation
sudo -H kubectl delete pod mariadb-0 -n cms --force --grace-period=0

# 3. Wait for clean reinitialisation (~2 min)
sudo -H kubectl get pod mariadb-0 -n cms -w
```

After reinitialisation, recreate the WordPress database and user by re-running
the idempotent Kubernetes phase. It re-applies the init Job and reads the
credentials from the existing `mariadb-secret`; there is no need to enter a
MariaDB prompt:
```bash
./cms-ha phase setup-kubernetes
```

#### K3s Pods in `Unknown` State After Boot

Symptom: `kubectl get pods -A` shows pods in `Unknown` on nodes that were powered off.

Solution: these are zombie pods from the previous state. K3s cleans them automatically within ~5 minutes, or they can be force-deleted:
```bash
ssh ubuntu@192.168.10.11 \
  'sudo -H kubectl get pods -A --field-selector=status.phase=Unknown -o json | \
   python3 -c "import json,sys,subprocess; [subprocess.run([\"kubectl\",\"delete\",\"pod\",\"-n\",i[\"metadata\"][\"namespace\"],i[\"metadata\"][\"name\"],\"--force\",\"--grace-period=0\"]) for i in json.load(sys.stdin)[\"items\"]]"'
```

#### Missing `rancher/mirrored-pause` Image on a Node

Symptom: `FailedCreatePodSandBox` due to missing pause image on a node that was powered off.

Solution: export the image from a node that has it (e.g. master2) and import
it on the affected node (example: master1):
```bash
# Export on master2
ssh ubuntu@192.168.10.12 \
  'sudo -H ctr -n k8s.io images export /tmp/pause-3.6.tar docker.io/rancher/mirrored-pause:3.6'
scp ubuntu@192.168.10.12:/tmp/pause-3.6.tar /tmp/pause-3.6.tar

# Import on master1
scp /tmp/pause-3.6.tar ubuntu@192.168.10.11:/tmp/pause-3.6.tar
ssh ubuntu@192.168.10.11 'sudo -H ctr -n k8s.io images import /tmp/pause-3.6.tar'
```

#### Puppet SSL Certificate Mismatch

Two related failures are common:

1. **Private key mismatch.** `puppet agent -t` fails with:
   ```text
   Error: The certificate for /CN=internal-master1.internal.local does not match its private key
   ```
   The Go CLI (`cms-ha phase setup-puppet`) detects this message, cleans the
   stale certificate on the Puppet server, removes the client SSL state and
   retries the catalog once. Re-running the phase is usually enough:
   ```bash
   ./cms-ha phase setup-puppet
   ```

2. **Certificate signature failure.** `puppet agent -t` fails with:
   ```text
   Error: certificate verify failed [certificate signature failure for /CN=internal-master1.internal.local]
   ```
   Cause: the node was reprovisioned or the Puppet Server CA regenerated its
   certificates, so the serial/signature no longer matches. The phase treats
   this like a private-key mismatch: it cleans the server-side certificate,
   removes the client SSL state and retries once. If the retry still fails,
   use the manual recovery below.

Manual recovery:
```bash
# 1. Clean certificate on Jumpstart
ssh ubuntu@192.168.10.10 "sudo /opt/puppetlabs/bin/puppetserver ca clean --certname <NODE_FQDN>"

# 2. Reset SSL state and request fresh certificate on client node
ssh ubuntu@<NODE_IP> "sudo -H rm -rf /etc/puppetlabs/puppet/ssl && sudo -H /opt/puppetlabs/bin/puppet agent -t || [ \$? -eq 2 ]"
```

#### DRBD Hostname Mismatch & Kubernetes Node Affinity

Symptom:
`drbdadm status` reports:
```text
'cms_data' not defined in your config (for this host)
```
or MariaDB pod stays in `Pending` state with message:
```text
0/4 nodes are available: 4 node(s) didn't match Pod's node affinity/selector.
```

Cause:
DRBD evaluates the local hostname against `uname -n` (which returns the FQDN e.g., `internal-master1.internal.local`). If the config only specifies the short hostname, DRBD rejects the resource. Likewise, Kubernetes node names match the FQDN.

Solution:
Ensure the DRBD template references both FQDN and short hostname:
```text
on internal-master1.internal.local internal-master1 {
    node-id 0;
    ...
}
```
And ensure Kubernetes is labeled using the active primary node:
```bash
kubectl label node internal-master1.internal.local drbd-status=primary --overwrite 2>/dev/null || \
kubectl label node internal-master1 drbd-status=primary --overwrite
```

#### Router NAT Rules Inactive or Stale

Symptom:
Nodes on the `main` network cannot access the internet (HTTP/HTTPS downloads fail), or external traffic on port 80/443 fails to reach the load balancer (`192.168.20.100`).

Cause:
The `*nat` block in `/etc/ufw/before.rules` must appear *before* the `*filter`
section, reference the correct dynamic WAN interface, and use the configured
`network.internal.cidr`, `network.main.cidr` and `network.main.vip`. The UFW
phase now verifies the live nat table after reloading UFW, so a stale block
fails the phase instead of leaving NAT silently broken.

Solution:
Run the idempotent UFW phase:
```bash
./cms-ha phase setup-ufw
```

Manual Verification:
```bash
ssh ubuntu@192.168.10.1 "sudo -H ufw status verbose"
ssh ubuntu@192.168.10.1 "sudo -H iptables -t nat -S"
ssh ubuntu@192.168.10.1 "sudo -H grep -A 10 '^\*nat' /etc/ufw/before.rules"
```
