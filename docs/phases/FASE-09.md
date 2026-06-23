# FASE 09 — Puestos Hot-desks



## Objetivos a conseguir

- [x] Desplegar 8 puestos hot-desk conectados a la red `main` (Punto 5).
- [x] Probar ping hacia el CMS internamente.

---

## Implementación Técnica

### Hot-desks Desplegados

| Hostname | IP | MAC | RAM | Disco |
|----------|-----|-----|-----|-------|
| main-hotdesk1 | 192.168.20.201 | 52:54:00:10:02:c9 | 512 MB | 5 GB |
| main-hotdesk2 | 192.168.20.202 | 52:54:00:10:02:ca | 512 MB | 5 GB |
| main-hotdesk3 | 192.168.20.203 | 52:54:00:10:02:cb | 512 MB | 5 GB |
| main-hotdesk4 | 192.168.20.204 | 52:54:00:10:02:cc | 512 MB | 5 GB |
| main-hotdesk5 | 192.168.20.205 | 52:54:00:10:02:cd | 512 MB | 5 GB |
| main-hotdesk6 | 192.168.20.206 | 52:54:00:10:02:ce | 512 MB | 5 GB |
| main-hotdesk7 | 192.168.20.207 | 52:54:00:10:02:cf | 512 MB | 5 GB |
| main-hotdesk8 | 192.168.20.208 | 52:54:00:10:02:d0 | 512 MB | 5 GB |

### Aprovisionamiento

Los hot-desks se crean automáticamente en `00_init_vms.sh` y se aprovisionan vía PXE/Cobbler con el perfil `ubuntu-24.04-x86_64`. Puppet les aplica el rol `role::hotdesk` que instala utilidades básicas.

### Conectividad

Cada hot-desk puede:
- ✅ Acceder al CMS: `curl http://192.168.20.100`
- ✅ Acceder a internet: `curl http://example.com` (vía NAT del router)
- ✅ Acceder a nodos de la red internal: `ping 192.168.10.11`

### Ampliar Hot-desks

Ver [`docs/MANUAL.md`](../MANUAL.md) → Sección 5 "Ampliar Puestos Hot-Desk" para instrucciones paso a paso.

### Verificación

```bash
# Ping al LB desde un hot-desk
ssh root@192.168.20.201 "ping -c 2 192.168.20.100"

# Acceso al CMS
ssh root@192.168.20.201 "curl -sk https://192.168.20.100/ | head -5"

# Acceso a internet
ssh root@192.168.20.201 "curl -s http://example.com | head -5"
```
