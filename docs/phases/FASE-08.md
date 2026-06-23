# FASE 08 — Monitorización de Servicios



## Objetivos a conseguir

- [x] Configurar monitorización avanzada y específica para el CMS (Punto 11).
- [x] Incluir extensiones (e.g., Apache SNMP exporter, MySQL Exporter).
- [x] Crear alertas de saturación.

---

## Implementación Técnica

### Exportadores de Servicios

| Exportador | Nodo(s) | Puerto | Métricas |
|------------|---------|--------|----------|
| **mysqld_exporter** | internal-master1 (192.168.10.11) | 9104 | Queries/s, conexiones, replicación, InnoDB |
| **nginx-prometheus-exporter** | main-lb (192.168.20.100) | 9113 | Conexiones activas, peticiones/s, upstreams |
| **apache_exporter** | main-cms1/2 (192.168.20.101-102) | 9117 | Workers busy/idle, accesos/s, tráfico |

### Configuración de Prometheus — Scrape Configs de Servicios

```yaml
scrape_configs:
  - job_name: 'mysql'
    static_configs:
      - targets: ['192.168.10.11:9104']

  - job_name: 'nginx'
    static_configs:
      - targets: ['192.168.20.100:9113']

  - job_name: 'apache'
    static_configs:
      - targets: ['192.168.20.101:9117', '192.168.20.102:9117']
```

### mysqld_exporter

Configurado con fichero `.my.cnf`:
```ini
[client]
user=exporter
password=exporterpass
host=localhost
port=3306
```

**Métricas clave:**
- `mysql_global_status_queries` — Total de queries
- `mysql_global_status_connections` — Conexiones activas
- `mysql_global_status_slow_queries` — Queries lentas
- `mysql_info_schema_table_rows` — Filas por tabla

### nginx-prometheus-exporter

Requiere módulo `stub_status` en Nginx:
```nginx
location /nginx_status {
    stub_status on;
    allow 127.0.0.1;
    deny all;
}
```

**Métricas clave:**
- `nginx_connections_active` — Conexiones activas
- `nginx_http_requests_total` — Total de peticiones
- `nginx_connections_accepted` — Conexiones aceptadas

### apache_exporter

Requiere módulo `mod_status` en Apache:
```apache
<Location "/server-status">
    SetHandler server-status
    Require local
</Location>
```

**Métricas clave:**
- `apache_workers` — Workers busy vs idle
- `apache_accesses_total` — Total de accesos
- `apache_sent_kilobytes_total` — Tráfico enviado

### Dashboards en Grafana

- **MySQL Overview** — Queries/s, conexiones, replicación
- **Nginx Overview** — Peticiones/s, conexiones activas, upstream health
- **Apache Overview** — Workers, tráfico, errores

### Scripts Asociados

- `scripts/04_setup_monitoring.sh` — Instalación de exportadores de servicios

### Verificación

```bash
# mysqld_exporter
curl -s http://192.168.10.11:9104/metrics | grep mysql_global_status_queries

# nginx_exporter
curl -s http://192.168.20.100:9113/metrics | grep nginx_connections_active

# apache_exporter
curl -s http://192.168.20.101:9117/metrics | grep apache_workers
```
