# FASE 03 — Clúster HA y Base de Datos



## Objetivos a conseguir

- [x] Instalar clúster de alta disponibilidad: 2 maestros, 2 workers (Punto 2).
- [x] Configurar base de datos SQL dentro del clúster (Punto 3).
- [x] Verificar sincronización y failover de BBDD y controladores.

---

## Implementación Técnica

### Clúster K3s (Kubernetes Ligero)

| Nodo | IP | Rol K3s |
|------|-----|---------|
| internal-master1 | 192.168.10.11 | Server (init, HA) |
| internal-master2 | 192.168.10.12 | Server (join, HA) |
| internal-worker1 | 192.168.10.13 | Agent |
| internal-worker2 | 192.168.10.14 | Agent |

**Instalación:**
- Master 1: `curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="server --cluster-init --node-ip 192.168.10.11 --tls-san 192.168.10.11 --tls-san 192.168.10.12 --write-kubeconfig-mode 644" sh -`
- Master 2: Se une con token al cluster existente
- Workers: Se unen como agentes con token

### Base de Datos MariaDB y Replicación (DRBD)

- **Desplegada como:** StatefulSet en namespace `cms` del clúster K3s con 1 réplica.
- **Imagen:** `mariadb:10.11`
- **Almacenamiento HA (DRBD):** PersistentVolume de 5 GiB (tipo `hostPath` apuntando a `/mnt/data/mariadb`).
  - El directorio `/mnt/data/mariadb` es en realidad el punto de montaje del recurso DRBD `cms_data` (sincronización síncrona a nivel de bloque en `/dev/drbd0` sobre el disco `/dev/vdb` entre los dos nodos maestros).
  - Solo el máster promovido a primario (`internal-master1` por defecto) monta el volumen DRBD.
- **Dynamic Node Pinning (Kubernetes + DRBD):** 
  - Para evitar que MariaDB se ejecute en nodos sin el almacenamiento DRBD montado, se ha configurado un `nodeSelector: drbd-status: primary` en `mariadb-statefulset.yaml`.
  - El script `03_setup_kubernetes.sh` etiqueta automáticamente a `internal-master1` con `drbd-status=primary`.
  - El script de failover local (`/usr/local/bin/drbd-failover.sh`) reasigna dinámicamente esta etiqueta durante el proceso de promoción y degradación, haciendo que Kubernetes mueva el pod de MariaDB de forma automática al nodo máster activo.
- **Exposición:** ClusterIP (3306) + NodePort (30306) para acceso desde red main.
- **Base de datos:** `wordpress`
- **Usuario:** `wp_user` con GRANT ALL sobre `wordpress`.

### Manifiestos Kubernetes

Los manifiestos se encuentran en `kubernetes/`:
- `namespace.yaml` — Namespace `cms`
- `mariadb-secret.yaml` — Credenciales de BD
- `mariadb-pv.yaml` / `mariadb-pvc.yaml` — Almacenamiento persistente (apuntando a `/mnt/data/mariadb`)
- `mariadb-statefulset.yaml` — StatefulSet de MariaDB (incluye el `nodeSelector` para DRBD)
- `mariadb-service.yaml` — Servicios ClusterIP y NodePort
- `init-db-job.yaml` — Job de inicialización de BD

### Scripts Asociados

- `scripts/03_setup_kubernetes.sh` — Instalación de K3s, etiquetado de nodo y despliegue de MariaDB.
- `scripts/06_setup_drbd.sh` — Configuración e instalación de DRBD en los másters y generación del script de failover `/usr/local/bin/drbd-failover.sh`.

### Verificación

```bash
# Verificar nodos en K3s
ssh root@192.168.10.11 "kubectl get nodes"

# Verificar que el pod de MariaDB se ejecuta en internal-master1 (tiene la etiqueta)
ssh root@192.168.10.11 "kubectl get pods -n cms -o wide"

# Verificar estado de DRBD en el nodo primario
ssh root@192.168.10.11 "drbdadm status cms_data"

# Probar conexión a la base de datos a través de NodePort
mysql -h 192.168.10.11 -P 30306 -u wp_user -p'WpS3cur3P4ss!' wordpress -e "SHOW TABLES;"
```
