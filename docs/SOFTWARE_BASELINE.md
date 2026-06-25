# Software & Firmware Baseline — CMS High-Availability Infrastructure

> **Reference date:** June 2026  
> **Revision:** 2.0

This document records the complete, unified inventory of all operating systems, software components, dependencies, and hardware/virtualisation firmware used in the CMS infrastructure.

---

## 1. Operating Systems and Hypervisor

| Component | Version / Release | Description | Source URL | Scope |
|:----------|:------------------|:------------|:-----------|:------|
| **Ubuntu Server** | 24.04.4 LTS (Noble Numbat) | Base operating system for the solution (Kernel Linux 6.8.0-31-generic) | [Ubuntu Noble](https://releases.ubuntu.com/noble/) | All client nodes and the Jumpstart server |
| **Ubuntu Desktop / Server** | 24.04 LTS (or later) | Operating system on the physical hypervisor host | [Ubuntu Releases](https://releases.ubuntu.com/) | Physical lab server |
| **libvirt** | 10.0.0-2ubuntu8 | API and daemon for KVM/QEMU hypervisor management | [libvirt.org](https://libvirt.org/) | Hypervisor host |
| **QEMU-KVM** | 1:8.2.1+dfsg-3ubuntu2 | Hardware-accelerated virtualisation emulator and hypervisor | [qemu.org](https://www.qemu.org/) | Hypervisor host |

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
| **Cobbler** | 3.3.7 | Zero-touch bare-metal provisioning | Python 3.12, Apache2, pyyaml, django | [Cobbler Project](https://cobbler.github.io/) | `jumpstart` |
| **Puppet Server** | 8.4.0 | Central configuration management server | OpenJDK 17, Ruby, openssl | [Puppet Server](https://www.puppet.com/) | `jumpstart` |
| **Puppet Agent** | 8.4.0 | Local configuration enforcement client | OpenSSL, libc6 | [Puppet Agent](https://www.puppet.com/) | All nodes except router |
| **Nginx** | 1.24.0 | Reverse proxy and load balancer | OpenSSL 3.0.x, zlib, pcre2 | [Nginx News](https://nginx.org/) | `main-lb` |
| **K3s** | v1.29.3+k3s1 | Lightweight high-availability Kubernetes cluster | Containerd, Flannel CNI, Traefik, SQLite | [K3s Official](https://k3s.io/) | `master1/2`, `worker1/2` |
| **MariaDB** | 10.11.7 (LTS) | CMS relational database (K3s StatefulSet) | DRBD 9 Volume, libc6 | [MariaDB Foundation](https://mariadb.org/) | K3s cluster (replicated) |
| **WordPress** | 6.5.2 | Content Management System (CMS) | PHP 8.3, Apache2, MySQL/MariaDB | [WordPress.org](https://wordpress.org/) | `main-cms1`, `main-cms2` |
| **Apache HTTP Server** | 2.4.58 | Web server hosting WordPress | `libapache2-mod-php8.3`, OpenSSL | [Apache HTTP Server](https://httpd.apache.org/) | `main-cms1`, `main-cms2` |
| **PHP Engine** | 8.3.6 | PHP code interpreter and execution engine | `php-mysql`, `php-curl`, `php-gd`, `php-zip` | [PHP Downloads](https://www.php.net/) | `main-cms1`, `main-cms2` |
| **DRBD** | 9.2.8 | Synchronous network block-level replication | Linux kernel module `drbd.ko` | [LINBIT DRBD](https://linbit.com/) | `master1`, `master2` |
| **drbd-utils** | 9.27.0 | DRBD management tools and CLI | `rpcbind`, `keyutils` | [LINBIT GitHub](https://github.com/LINBIT/) | `master1`, `master2` |
| **Prometheus** | 2.45.3 | Time-series metrics collection engine | Static Go binaries | [Prometheus](https://prometheus.io/) | `internal-monitor` |
| **Grafana** | 10.4.1 | Data visualisation and operational dashboards | prometheus-datasource | [Grafana Labs](https://grafana.com/) | `internal-monitor` |
| **UFW** | 0.36.2 | Perimeter and per-node firewall management | `iptables`, `nftables`, `python3` | [Launchpad UFW](https://launchpad.net/ufw) | `ufw-router`, all nodes |
| **Terraform** | 1.9.x | Declarative VM/network infrastructure provisioning | `libvirt` daemon, KVM support | [HashiCorp Terraform](https://www.terraform.io/) | Hypervisor Host (alternative) |
| **terraform-provider-libvirt** | 0.8.0 | Libvirt/KVM integration provider for Terraform | `libvirt-dev` library, Go runtime | [dmacvicar/libvirt](https://github.com/dmacvicar/terraform-provider-libvirt) | Hypervisor Host (alternative) |

---

## 4. Critical System Dependencies and Libraries

For the baseline to function consistently, the following system libraries and dependencies are automatically installed on nodes by the Cobbler and Puppet provisioning scripts:

*   **Cryptographic Libraries**: `openssl (3.0.13)`, `libssl3` (required for HTTPS, Puppet encrypted communication, and K3s tokens).
*   **Core Network Services**:
    *   `isc-dhcp-server (4.4.3-P1)`: Provides DHCP PXE leases from the `jumpstart` node.
    *   `tftpd-hpa (5.2)`: Serves PXE bootloaders `pxelinux.0` and `ldlinux.c32` for network boot.
    *   `bind9 (9.18.24)`: Primary DNS server for the `.internal.local` and `.main.local` domains.
    *   `nfs-kernel-server (2.6.4)`: Serves the Ubuntu 24.04 live-server image via NFS over TCP to the Subiquity installer.
*   **Language Modules and Connectors**:
    *   `python3-yaml (6.0.1)` and `python3-pip (24.0)`: For structured Cobbler configuration.
    *   `libapache2-mod-wsgi-py3 (5.0.0)`: For Cobbler's HTTP administration interface.
    *   `django (4.2)`: Web framework required for the Cobbler web console.

---

## 5. Consistency Guarantee Notes

1.  **Container Images**: The MariaDB image in Kubernetes uses the stable tag `mariadb:10.11` from Docker Hub.
2.  **System Patches**: All client nodes and the Jumpstart server apply the latest security patches from Ubuntu's `noble-security` repository during unattended installation.
3.  **Virtualisation Firmware (SeaBIOS)**: Ensures non-UEFI VMs boot deterministically using legacy PXE bootstrap code (`pxelinux.0`) without requiring Secure Boot certificate management.
