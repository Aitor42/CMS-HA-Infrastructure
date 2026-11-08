# Software & Firmware Baseline — CMS High-Availability Infrastructure

> **Reference date:** November 2026 · **Revision:** 2.2

This document records the software inventory and version-selection policy implemented by the repository. Explicit pins/floors below come from code or configuration; unpinned APT/image components are not a measured installed-version inventory. Capture package, kernel and firmware versions from the target host/guests for a runtime baseline.

---

## 1. Operating Systems and Hypervisor

| Component | Version / Release | Description | Source URL | Scope |
|:----------|:------------------|:------------|:-----------|:------|
| **Ubuntu Server** | 24.04 LTS (Noble); ISO/cloud image selected at provisioning | Base operating system; kernel and point release depend on the image and updates | [Ubuntu Noble](https://releases.ubuntu.com/noble/) | All client nodes and the Jumpstart server |
| **Ubuntu Desktop / Server** | 24.04 LTS (or later) | Operating system on the physical hypervisor host | [Ubuntu Releases](https://releases.ubuntu.com/) | Physical lab server |
| **libvirt** | Host APT package (not pinned) | API and daemon for KVM/QEMU hypervisor management | [libvirt.org](https://libvirt.org/) | Hypervisor host |
| **QEMU-KVM** | Host APT package (not pinned) | Hardware-accelerated virtualisation emulator and hypervisor | [qemu.org](https://www.qemu.org/) | Hypervisor host |

---

## 2. Device Firmware (Virtual and Physical)

In a virtualised environment, device firmware selection is critical for compatibility and stability. The following firmware is used for each network and compute component:

| Device | Model / Emulator | Firmware / Image | Type | Description |
|:-------|:-----------------|:-----------------|:-----|:------------|
| **Virtual NICs** | VirtIO Network Device | `virtio-net` | Device Driver Firmware | High-performance virtual network firmware for client-hypervisor communication |
| **Virtual Storage Controllers** | VirtIO Block Device | `virtio-blk` | Device Driver Firmware | Virtual storage firmware for direct access to QCOW2 images on the hypervisor |

---

## 3. Infrastructure and Service Software Inventory

| Software | Version / Release | Function | Key Dependencies | Source URL | Node(s) |
|:---------|:------------------|:---------|:-----------------|:-----------|:--------|
| **cms-ha (Go CLI)** | Go 1.27 | Unified infrastructure orchestrator & CLI | Cobra, yaml.v3, age (X25519), `golang.org/x/crypto` ≥ 0.56.0, slog | [GitHub Repo](https://github.com/Aitor42/CMS-HA-Infrastructure) | Hypervisor Host |
| **Cobbler** | APT package (not pinned) | Zero-touch bare-metal provisioning | Python 3.12, Apache2, pyyaml, django | [Cobbler Project](https://cobbler.github.io/) | `jumpstart` |
| **Puppet Server** | Puppet 8 repository; package revision not pinned | Central configuration management server | OpenJDK 17, Ruby, openssl | [Puppet Server](https://www.puppet.com/) | `jumpstart` |
| **Puppet Agent** | Puppet 8 repository; package revision not pinned | Local configuration enforcement client | OpenSSL, libc6 | [Puppet Agent](https://www.puppet.com/) | All nodes (incl. router) |
| **Nginx** | Ubuntu APT package (not pinned) | Reverse proxy and load balancer | OpenSSL 3.0.x, zlib, pcre2 | [Nginx News](https://nginx.org/) | `main-lb1`, `main-lb2` |
| **keepalived** | Ubuntu APT package (not pinned) | VRRP failover for the shared LB VIP (192.168.20.100) | libc6, iproute2 | [keepalived.org](https://keepalived.org/) | `main-lb1`, `main-lb2` |
| **K3s** | v1.37.2+k3s1 | Lightweight high-availability Kubernetes cluster | Containerd, Flannel CNI, Traefik, embedded etcd | [K3s Official](https://k3s.io/) | `master1/2/3`, `worker1/2` |
| **etcd** | Embedded in the pinned K3s release (not independently pinned) | Distributed key-value store providing the control-plane quorum (3 members tolerate 1 failure) | Bundled with K3s (no separate install) | [etcd.io](https://etcd.io/) | `master1/2/3` |
| **MariaDB** | `10.11` image pinned by digest (see § 5) | CMS relational database (K3s StatefulSet) | DRBD volume, libc6 | [MariaDB Foundation](https://mariadb.org/) | K3s cluster (replicated) |
| **WordPress** | `latest.tar.gz` at bootstrap (not version-pinned) | Content Management System (CMS) | PHP 8.3, Apache2, MySQL/MariaDB | [WordPress.org](https://wordpress.org/) | `main-cms1`, `main-cms2` |
| **Apache HTTP Server** | Ubuntu APT package (not pinned) | Web server hosting WordPress | `libapache2-mod-php8.3`, OpenSSL | [Apache HTTP Server](https://httpd.apache.org/) | `main-cms1`, `main-cms2` |
| **PHP Engine** | ≥ 8.3.6-0ubuntu0.24.04.13 (configured package floor) | mod_php runtime with Apache-enforced 128M memory limit and four prefork workers | `php-mysql`, `php-curl`, `php-gd`, `php-zip` | [PHP Downloads](https://www.php.net/) | `main-cms1`, `main-cms2` |
| **DRBD** | Kernel-provided module (not version-pinned) | Synchronous network block-level replication | Linux kernel module `drbd.ko` | [LINBIT DRBD](https://linbit.com/) | `master1`, `master2` |
| **drbd-utils** | Ubuntu APT package (not pinned) | DRBD management tools and CLI | `rpcbind`, `keyutils` | [LINBIT GitHub](https://github.com/LINBIT/) | `master1`, `master2` |
| **Prometheus** | Ubuntu APT package (not pinned) | Time-series metrics collection engine | Static Go binaries | [Prometheus](https://prometheus.io/) | `internal-monitor` |
| **Alertmanager** | Ubuntu APT package (not pinned) | Alert routing, grouping and deduplication for Prometheus | `prometheus-alertmanager` (APT) | [Alertmanager](https://prometheus.io/docs/alerting/latest/alertmanager/) | `internal-monitor` |
| **Grafana** | 13.3.0 | Data visualisation and operational dashboards | prometheus-datasource | [Grafana Labs](https://grafana.com/) | `internal-monitor` |
| **blackbox_exporter** | Ubuntu APT package (not pinned) | End-to-end HTTP/TCP reachability probes (LB and CMS frontends) | None (standalone) | [Prometheus blackbox](https://github.com/prometheus/blackbox_exporter) | `internal-monitor` |
| **UFW** | Ubuntu APT package (not pinned) | Perimeter and per-node firewall management | `iptables`, `nftables`, `python3` | [Launchpad UFW](https://launchpad.net/ufw) | `ufw-router`, all nodes |
| **step-ca** | 0.30.2 | Private Certificate Authority for internal PKI | step CLI, SQLite | [Smallstep CA](https://github.com/smallstep/certificates) | `jumpstart` |
| **step** | 0.31.0 | Command-line tool for interacting with step-ca | libc6 | [Smallstep CLI](https://github.com/smallstep/cli) | All nodes |
| **Ansible** | CI ansible-core 2.22.0; local installation operator-managed | Optional declarative creation of networks, disks and VM domains; the Go `init-vms` phase is the default/gate path | Python 3, SSH, libvirt daemon | [Ansible](https://www.ansible.com/) | Hypervisor Host |
| **community.libvirt** | 2.4.0 (`ansible/requirements.yml`) | Ansible collection for the optional libvirt provisioning path | Ansible core, `libvirt-python` | [community.libvirt](https://github.com/ansible-collections/community.libvirt) | Hypervisor Host |
| **Garage** | v2.5.0 | S3-compatible object store for off-DRBD backups | Container runtime (Docker/Podman), SQLite | [Garage](https://garagehq.deuxfleurs.fr/) | Hypervisor Host |
| **rclone** | 1.76.0 | S3 client used by the backup/restore CronJobs | Alpine libc | [rclone](https://rclone.org/) | K3s cluster (Jobs) |
| **k6** | 2.3.0 | HTTP load/smoke generator for the CMS (`cms-ha traffic`) | Container runtime (Docker/Podman) | [k6](https://k6.io/) | Hypervisor Host |
| **gosec** | v2.30.0 in CI | Go static analysis (SAST) in CI (`security-scan`) | Go toolchain | [gosec](https://github.com/securego/gosec) | CI runner |
| **govulncheck** | v1.9.0 in CI | Reachable-vulnerability scanner for Go dependencies | Go toolchain, vuln DB | [govulncheck](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) | CI runner |
| **Trivy** | v0.76.2 through `trivy-action` v0.36.0 in CI | Blocking HIGH/CRITICAL vulnerability/secret scan; misconfiguration scan report-only | vuln DB | [Trivy](https://trivy.dev/) | CI runner |

---

## 4. Critical System Dependencies and Libraries

For the baseline to function consistently, the following system libraries and dependencies are automatically installed on nodes by the Cobbler and Puppet provisioning scripts:

These are package families, not exact package pins or observed runtime versions.

*   **Cryptographic Libraries**: `openssl`, `libssl3` (required for HTTPS, Puppet encrypted communication, and K3s tokens).
*   **Core Network Services**:
    *   `isc-dhcp-server`: Provides DHCP PXE leases from the `jumpstart` node.
    *   `tftpd-hpa`: Serves PXE bootloaders `pxelinux.0` and `ldlinux.c32` for network boot.
    *   `bind9`: Primary DNS server for the `.internal.local` and `.main.local` domains.
    *   `nfs-kernel-server`: Serves the Ubuntu 24.04 live-server image via NFS over TCP to the Subiquity installer.
*   **Language Modules and Connectors**:
    *   `python3-yaml` and `python3-pip`: For structured Cobbler configuration.
    *   `libapache2-mod-wsgi-py3`: For Cobbler's HTTP administration interface.
    *   `django` (fallback installer range `>=3.2,<5`): Web framework required for the Cobbler web console.

---

## 5. Consistency Guarantee Notes

1.  **Container Images**: The MariaDB image in Kubernetes is pinned **by digest** (`mariadb:10.11@sha256:d769e379…`) so a re-tagged `10.11` cannot silently change the deployed bits; the off-DRBD backup stack pins `dxflrs/garage:v2.5.0` and `rclone/rclone:1.76.0`, and the load generator pins `grafana/k6:2.3.0` (`traffic.k6_version` in `config.yaml`; a floating `latest` tag is rejected at config validation).
2.  **System Patches**: All client nodes and the Jumpstart server apply the latest security patches from Ubuntu's `noble-security` repository during unattended installation.
3.  **Virtualisation Firmware (SeaBIOS)**: Ensures non-UEFI VMs boot deterministically using legacy PXE bootstrap code (`pxelinux.0`) without requiring Secure Boot certificate management.
4.  **Dependency Security**: `golang.org/x/crypto` is held at **≥ 0.56.0** to clear GO-2026-6354 / GO-2026-6355; `govulncheck`, Gitleaks and Trivy enforce this in CI (see `docs/SECURITY.md`).
5.  **PHP Runtime Controls**: `role::cms_frontend` and functional verification enforce **≥ 8.3.6-0ubuntu0.24.04.13** for `php8.3-cli`, `php8.3-common` and `libapache2-mod-php8.3`. This is a package floor, not an exact installed version or a replacement for checking current vendor advisories. Apache enforces `php_admin_value memory_limit 128M`, upload-progress tracking off, `post_max_size 32M`, `upload_max_filesize 16M` and `LimitRequestBody 33554432`. The standard prefork configuration sets `ServerLimit 4` and `MaxRequestWorkers 4`; request-body caps alone do not bound PHP allocations, and PHP/native/Apache overhead still needs memory headroom.
6.  **Uploads Safety**: Frontends use `_netdev,auto,rw,hard,timeo=600,retrans=2,nfsvers=4`. Apache requires the expected writable hard export; the local directory is protected when unmounted. Storage exports and `2049/tcp` rules allow only generated CMS frontend IPs with UID/GID 33 squashing. The single NFS server remains a shared availability dependency. Migration details are in [MANUAL section 6.2](MANUAL.md#62-wordpress-media-and-file-backups).
7.  **Fencing Trust**: Derived SSH fencing uses a dedicated restricted forced-command key, exact `destroy`/`domstate` requests for the DRBD pair only, and persistent pins sourced from local hypervisor sshd public keys. Changed pins or unsafe managed paths fail closed; custom command pairs remain operator-managed. See [MANUAL section 7.1](MANUAL.md#71-master-1-failure-drbdk3s-primary).
8.  **Evidence Scope**: Identity-bound checkpoints require the original executable/host/deployment inputs. Release publication requires tagged-SHA CI and a full ISO/PXE gate with chaos and teardown. Database restore duration includes S3 download and ephemeral DB validation; failover durations include preflight/restoration. Neither is CMS RTO, and configured gates are not runtime results. See [MANUAL](MANUAL.md#37-end-to-end-integration-test).
