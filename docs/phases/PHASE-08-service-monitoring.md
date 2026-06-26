# PHASE 08 — Service Monitoring

## Objectives to Achieve

- [x] Configure advanced and specific monitoring for the CMS.
- [x] Include service-specific exporters (e.g. Apache SNMP/HTTP exporter, MySQL Exporter).
- [x] Create saturation alerts.

---

## Technical Implementation

### Service Exporters

| Exporter | Node(s) | Port | Monitored Metrics |
|:---------|:--------|:-----|:------------------|
| **mysqld_exporter** | internal-master1 (192.168.10.11) | 9104 | Queries/s, active connections, replication status, InnoDB metrics |
| **nginx-prometheus-exporter** | main-lb (192.168.20.100) | 9113 | Active connections, requests/s, upstream status |
| **apache_exporter** | main-cms1/2 (192.168.20.101-102) | 9117 | Busy/idle workers, accesses/s, traffic rate |

### Prometheus Configuration — Service Scrape Configs

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

Configured with the `.my.cnf` file:
```ini
[client]
user=exporter
password=exporterpass
host=localhost
port=3306
```

**Key Metrics:**
- `mysql_global_status_queries` — Total queries executed
- `mysql_global_status_connections` — Active client connections
- `mysql_global_status_slow_queries` — Total slow queries
- `mysql_info_schema_table_rows` — Row count per table

### nginx-prometheus-exporter

Requires the `stub_status` module enabled in Nginx:
```nginx
location /nginx_status {
    stub_status on;
    allow 127.0.0.1;
    deny all;
}
```

**Key Metrics:**
- `nginx_connections_active` — Active connections
- `nginx_http_requests_total` — Total HTTP requests
- `nginx_connections_accepted` — Accepted connection count

### apache_exporter

Requires the `mod_status` module enabled in Apache:
```apache
<Location "/server-status">
    SetHandler server-status
    Require local
</Location>
```

**Key Metrics:**
- `apache_workers` — Busy vs idle workers
- `apache_accesses_total` — Total HTTP accesses
- `apache_sent_kilobytes_total` — Outbound traffic in KB

### Grafana Dashboards

- **MySQL Overview** — Queries/s, connections, replication topology
- **Nginx Overview** — Requests/s, active connections, upstream backend health
- **Apache Overview** — Worker utilization, traffic, error rates

### Associated Scripts

- `scripts/08_setup_monitoring.sh` — Installation and setup of service-specific exporters

### Verification

```bash
# Query mysqld_exporter metrics
curl -s http://192.168.10.11:9104/metrics | grep mysql_global_status_queries

# Query nginx_exporter metrics
curl -s http://192.168.20.100:9113/metrics | grep nginx_connections_active

# Query apache_exporter metrics on cms1
curl -s http://192.168.20.101:9117/metrics | grep apache_workers
```
