# FASE 07 — Monitorización de Infraestructura



## Objetivos a conseguir

- [x] Desplegar Nodo Monitor dentro de la red `internal` (Punto 10).
- [x] Recolectar variables: CPU, disco, memoria y red.
- [x] Crear el dashboard maestro en Prometheus/Grafana.

---

## Implementación Técnica

### Nodo Monitor (192.168.10.20)

- **Prometheus 2.x:** Motor de scraping y almacenamiento de métricas
- **Grafana 11.x:** Plataforma de visualización y dashboards

### node_exporter

Instalado en todos los nodos gestionados via `prometheus-node-exporter` (paquete apt). Puerto: 9100.

**Nodos monitorizados:**
- 192.168.10.10 (jumpstart), 192.168.10.11-15 (internal), 192.168.10.20 (monitor)
- 192.168.20.100-102 (main-lb, cms1, cms2)

### Configuración de Prometheus

```yaml
global:
  scrape_interval: 15s

scrape_configs:
  - job_name: 'node_exporter'
    static_configs:
      - targets:
        - '192.168.10.10:9100'
        - '192.168.10.11:9100'
        - '192.168.10.12:9100'
        - '192.168.10.13:9100'
        - '192.168.10.14:9100'
        - '192.168.10.15:9100'
        - '192.168.10.20:9100'
        - '192.168.20.100:9100'
        - '192.168.20.101:9100'
        - '192.168.20.102:9100'
```

### Métricas de Infraestructura (Punto 10)

| Métrica | Query PromQL | Descripción |
|---------|-------------|-------------|
| CPU | `100 - (avg(rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100)` | Uso de CPU por nodo |
| Memoria | `node_memory_MemTotal_bytes - node_memory_MemAvailable_bytes` | RAM utilizada |
| Disco | `node_filesystem_avail_bytes{mountpoint="/"}` | Espacio libre |
| Red | `rate(node_network_receive_bytes_total[5m])` | Tráfico de red |

### Provisión de Grafana

- **Datasource:** Prometheus configurado automáticamente vía `/etc/grafana/provisioning/datasources/`
- **Dashboard:** Node Exporter Full (ID 1860) descargado e importado automáticamente

### Scripts Asociados

- `scripts/04_setup_monitoring.sh` — Instalación de node_exporter, Prometheus y Grafana

### Verificación

```bash
# Prometheus targets
curl -s http://192.168.10.20:9090/api/v1/targets | python3 -m json.tool | grep health

# Grafana
curl -s http://192.168.10.20:3000/api/health  # {"database":"ok"}

# node_exporter en un nodo
curl -s http://192.168.10.11:9100/metrics | head -5
```
