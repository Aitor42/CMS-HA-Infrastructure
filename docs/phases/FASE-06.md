# FASE 06 — Firewalling Nodal (End-point Security)



## Objetivos a conseguir

- [x] Implementar UFW para limitar acceso a los servicios instalados por nodo (Punto 9).
- [x] Asegurar que los puertos de monitorización y SSH queden abiertos solo al originador correcto.

---

## Implementación Técnica

### Política General por Nodo

Cada nodo tiene UFW configurado con:
- `ufw default deny incoming` — Deniega todo tráfico entrante por defecto
- Solo se abren los puertos específicos del servicio que ejecuta el nodo
- SSH (22) permitido solo desde el jumpstart (192.168.10.10)
- node_exporter (9100) permitido solo desde el monitor (192.168.10.20)

### Reglas por Nodo

| Nodo | Puertos Abiertos | Justificación |
|------|-------------------|---------------|
| **internal-monitor** | 22, 9090, 3000, 9100 | SSH, Prometheus, Grafana, node_exporter |
| **internal-master1** | 22, 6443, 30306, 9100, 9104, 7788 | SSH, K3s API, MariaDB NodePort, exporters, DRBD |
| **internal-master2** | 22, 6443, 9100, 7788 | SSH, K3s API, node_exporter, DRBD |
| **internal-worker1/2** | 22, 9100, 10250 | SSH, node_exporter, kubelet |
| **internal-storage** | 22, 9100 | SSH, node_exporter |
| **main-lb** | 22, 80, 443, 9100, 9113 | SSH, HTTP, HTTPS, node_exporter, nginx_exporter |
| **main-cms1/2** | 22, 80, 9100, 9117 | SSH, Apache, node_exporter, apache_exporter |

### Scripts Asociados

- `scripts/05_setup_ufw.sh` — Sección de firewalling nodal (segunda parte del script)

### Verificación

```bash
# Verificar reglas en cada nodo
ssh root@192.168.10.20 "ufw status numbered"
ssh root@192.168.10.11 "ufw status numbered"
ssh root@192.168.20.100 "ufw status numbered"

# Test de puertos cerrados (desde un nodo externo):
# El puerto 3306 de master1 NO debe ser accesible directamente (solo 30306 NodePort)
nc -zv 192.168.10.11 3306   # Debería fallar
nc -zv 192.168.10.11 30306  # Debería funcionar
```
