# FASE 00 — Diseño de Red y Direccionamiento



## Objetivos a conseguir

- [x] Dividir la red en subredes `internal` y `main` (Punto 1).
- [x] Diseñar esquema GNS3 definiendo direcciones L3 y VLANs.
- [x] Ubicar los 6 nodos fijos de `internal` y los 3 fijos de `main`.

---

## Implementación Técnica

### Diseño de Subredes

| Red | CIDR | Gateway | Función |
|-----|------|---------|---------|
| **internal** | 192.168.10.0/24 | 192.168.10.1 | Clúster K3s, monitorización, almacenamiento |
| **main** | 192.168.20.0/24 | 192.168.20.1 | Balanceador, frontales CMS, hot-desks |
| **WAN** | DHCP | — | Acceso a internet (interfaz del router) |

### Inventario de Direcciones IP

**Red Internal (6 nodos):**

| Hostname | IP | MAC | Rol | RAM |
|----------|-----|-----|-----|-----|
| internal-monitor | 192.168.10.20 | 52:54:00:10:01:10 | Prometheus + Grafana | 512 MB |
| internal-master1 | 192.168.10.11 | 52:54:00:10:01:11 | K3s master + DRBD | 1024 MB |
| internal-master2 | 192.168.10.12 | 52:54:00:10:01:12 | K3s master + DRBD | 1024 MB |
| internal-worker1 | 192.168.10.13 | 52:54:00:10:01:13 | K3s worker | 768 MB |
| internal-worker2 | 192.168.10.14 | 52:54:00:10:01:14 | K3s worker | 768 MB |
| internal-storage | 192.168.10.15 | 52:54:00:10:01:15 | Almacenamiento | 1024 MB |

**Red Main (3 nodos + 8 hot-desks):**

| Hostname | IP | MAC | Rol | RAM |
|----------|-----|-----|-----|-----|
| main-lb | 192.168.20.100 | 52:54:00:10:02:64 | Nginx LB | 512 MB |
| main-cms1 | 192.168.20.101 | 52:54:00:10:02:65 | WordPress + Apache | 512 MB |
| main-cms2 | 192.168.20.102 | 52:54:00:10:02:66 | WordPress + Apache | 512 MB |
| main-hotdesk1..8 | 192.168.20.201..208 | dinámicas | Puesto empleado | 768 MB |

**Otros:**

| Hostname | IP | Rol |
|----------|-----|-----|
| jumpstart | 192.168.10.10 / 192.168.20.10 | Cobbler + Puppet (ambas redes) |
| ufw-router | 192.168.10.1 / 192.168.20.1 / DHCP | Firewall perimetral |

### Scripts Asociados

- `scripts/00_init_vms.sh` — Crea las redes virtuales (bridges) y todas las VMs
- `scripts/start_all_vms.sh` — Arranca VMs existentes (reanudación tras `shrink_vm_ram.sh`)
- `scripts/shrink_vm_ram.sh` — Apaga VMs y ajusta RAM mínima de laboratorio en GAR

### Verificación

```bash
sudo virsh net-list --all      # Verificar redes internal y main
sudo virsh list --all          # Verificar 20 VMs creadas
```
