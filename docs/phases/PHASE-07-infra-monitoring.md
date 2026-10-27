# PHASE 07 — Infrastructure Monitoring

## Objectives to Achieve

- [x] Deploy Monitor Node within the `internal` network.
- [x] Collect key metrics: CPU, disk, memory, and network usage.
- [x] Create the master dashboard in Prometheus/Grafana.

---

## Technical Implementation

### Monitor Node (192.168.10.20)

- **Prometheus 2.x:** Metrics collection (scraping) and storage engine
- **Grafana 13.2.4:** Data visualization and dashboard platform

### node_exporter

Installed on all managed nodes via the `prometheus-node-exporter` apt package. Port: 9100.

**Monitored Nodes:**
- 192.168.10.1 (ufw-router), 192.168.10.10 (jumpstart), 192.168.10.11-16 (internal subnet), 192.168.10.20 (monitor itself)
- 192.168.20.101-104 (cms1, cms2, main-lb1, main-lb2)

### Prometheus Configuration

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
        - '192.168.10.16:9100'
        - '192.168.10.13:9100'
        - '192.168.10.14:9100'
        - '192.168.10.15:9100'
        - '192.168.10.20:9100'
        - '192.168.20.103:9100'
        - '192.168.20.104:9100'
        - '192.168.20.101:9100'
        - '192.168.20.102:9100'
```

### Infrastructure Metrics

| Metric | PromQL Query | Description |
|:-------|:-------------|:------------|
| CPU | `100 - (avg(rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100)` | CPU usage per node |
| Memory | `node_memory_MemTotal_bytes - node_memory_MemAvailable_bytes` | RAM used |
| Disk | `node_filesystem_avail_bytes{mountpoint="/"}` | Free disk space |
| Network | `rate(node_network_receive_bytes_total[5m])` | Inbound network traffic rate |

### Grafana Provisioning

- **Datasource:** Prometheus, provisioned from `/etc/grafana/provisioning/datasources/`.
- **Dashboards:** *Node Exporter Overview* and *HA Overview*, provisioned from `role::monitor`; the HA Overview is the home dashboard.

### Implementation

- `internal/phases/monitoring` (`cms-ha phase setup-monitoring`) — verifies the exporters, mints the Prometheus token, deploys the alert rules and checks the services.
- `puppet/modules/role` (`role::monitor`) — keeps the exporters and configuration converged.

### Verification

```bash
# Prometheus targets health check
curl -s http://192.168.10.20:9090/api/v1/targets | python3 -m json.tool | grep health

# Grafana API health check
curl -s http://192.168.10.20:3000/api/health  # Expect: {"database":"ok"}

# Query node_exporter on a node
curl -s http://192.168.10.11:9100/metrics | head -5
```
