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

1. **Packages:** cobbler, cobbler-web, isc-dhcp-server, tftpd-hpa, apache2, xinetd
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

Below is a screenshot of the automated network installation process (PXE) managed by Cobbler:

![PXE Installation](../pxe-installation.png)

### Associated Scripts

- `scripts/01_setup_cobbler.sh` — Installation and configuration of Cobbler on the jumpstart node
- `scripts/02_register_cobbler_nodes.sh` — Registers all nodes in Cobbler

### Verification

```bash
ssh root@192.168.10.10 "cobbler check"
ssh root@192.168.10.10 "cobbler system list"
ssh root@192.168.10.10 "cobbler distro list"
ssh root@192.168.10.10 "systemctl status cobblerd"
```
