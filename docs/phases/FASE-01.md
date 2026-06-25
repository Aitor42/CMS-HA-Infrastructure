# FASE 01 — Nodo Jumpstart / Cobbler



## Objetivos a conseguir

- [x] Desplegar nodo Jumpstart conectado a ambas subredes (Punto 14).
- [x] Configurar aprovisionamiento automático de SO (Punto 13).
- [x] Preparar scripts o integraciones con Cobbler (Punto 5/Jumpstart).

---

## Implementación Técnica

### Nodo Jumpstart

- **IP internal:** 192.168.10.10
- **IP main:** 192.168.20.10
- **Servicios:** Cobbler (PXE + DHCP + TFTP + DNS), Puppet Server 8

### Cobbler: Configuración

1. **Paquetes:** cobbler, cobbler-web, isc-dhcp-server, tftpd-hpa, apache2, xinetd
2. **Fichero de configuración:** `/etc/cobbler/settings.yaml` (formato YAML en Cobbler 3.x)
3. **Parámetros clave:**
   - `server: 192.168.10.10`
   - `next_server: 192.168.10.10`
   - `manage_dhcp: true`
   - `manage_dns: true`
   - `manage_tftpd: true`

### Aprovisionamiento Automático

- **Distro:** Ubuntu 24.04 LTS (Noble) importada desde ISO oficial
- **Autoinstall:** Template cloud-init para instalación desatendida
- **DHCP:** Configurado para ambas subredes con reservas por MAC
- **DNS:** Zonas `internal.local` y `main.local`

A continuación se muestra una captura del proceso de instalación automática por red (PXE) gestionado por Cobbler:

![Instalación por PXE](../pxe-installation.png)


### Scripts Asociados

- `scripts/00_setup_cobbler.sh` — Instalación y configuración de Cobbler en jumpstart
- `scripts/add_cobbler_nodes.sh` — Registro de todos los nodos en Cobbler

### Verificación

```bash
ssh root@192.168.10.10 "cobbler check"
ssh root@192.168.10.10 "cobbler system list"
ssh root@192.168.10.10 "cobbler distro list"
ssh root@192.168.10.10 "systemctl status cobblerd"
```
