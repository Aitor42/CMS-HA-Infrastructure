# PHASE 01 — Jumpstart Node / Cobbler

## Objectives to Achieve

- [x] Deploy Jumpstart node connected to both subnets.
- [x] Configure automated OS provisioning (PXE).
- [x] Prepare helper scripts or integrations with Cobbler.

---

## Technical Implementation

### Jumpstart Node

- **Internal IP:** 192.168.10.10
- **Main IP:** 192.168.20.10
- **Services:** Cobbler (PXE + DHCP + TFTP + DNS), Puppet Server 8

### Cobbler: Configuration

1. **Packages:** cobbler, apache2, isc-dhcp-server, tftpd-hpa, nfs-kernel-server, libapache2-mod-wsgi-py3, python3-yaml, bind9, bind9utils, pxelinux, ipxe, shim-signed, grub-efi-amd64-signed, syslinux-common, curl, wget, rsync
2. **Configuration file:** `/etc/cobbler/settings.yaml` (YAML format in Cobbler 3.x)
3. **Key parameters:**
   - `server: 192.168.10.10`
   - `next_server: 192.168.10.10`
   - `manage_dhcp: true`
   - `manage_dns: true`
   - `manage_tftpd: true`

### Automated Provisioning

- **Distro:** Ubuntu 24.04 LTS (Noble) imported from official ISO
- **Autoinstall:** cloud-init template for unattended installation
- **DHCP:** Configured for both subnets with MAC address reservations
- **DNS:** Zones `internal.local` and `main.local`
- **Node registration (`phase register-nodes`):** every node requires a MAC address — a node without one is rejected instead of being registered as a zero-MAC Cobbler system that would never PXE-boot reliably. The Cobbler system creation and the `cobbler sync` run with `set -e` and verified exit codes.

Below is a screenshot of the automated network installation process (PXE) managed by Cobbler:

![PXE Installation](../pxe-installation.png)

### Implementation

- `internal/phases/cobbler` (`cms-ha phase setup-cobbler`) — installs and configures Cobbler, DHCP, TFTP, DNS and NFS on the jumpstart node.
- `internal/phases/register` (`cms-ha phase register-nodes`) — registers every node, its MAC address and static IP in Cobbler.

### Verification

```bash
ssh ubuntu@192.168.10.10 "sudo cobbler check"
ssh ubuntu@192.168.10.10 "sudo cobbler system list"
ssh ubuntu@192.168.10.10 "sudo cobbler distro list"
ssh ubuntu@192.168.10.10 "sudo systemctl status cobblerd"
```
