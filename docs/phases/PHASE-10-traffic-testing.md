# PHASE 10 — TrafficMix, End-to-End Tests, and Jumpstart Shutdown

## Objectives to Achieve

- [x] Test traffic generation with TrafficMix from the WAN subnet.
- [x] Run database queries or management scripts from the integrated hot-desks.
- [x] Shut down the Jumpstart node and demonstrate full system survival.

---

## Technical Implementation

### TrafficMix Script

The script `scripts/11_traffic_mix.sh` generates simulated traffic against the CMS web infrastructure. It supports two modes:

**External Mode** (simulates client traffic from the Internet):
```bash
./scripts/11_traffic_mix.sh --external --target <ROUTER_WAN_IP> --duration 120
```

**Internal Mode** (runs locally from a hot-desk node):
```bash
./scripts/11_traffic_mix.sh --internal --duration 60 --with-db
```

### Types of Generated Traffic

| Traffic Type | Tool Used | Description |
|:-------------|:----------|:------------|
| WordPress Pages | `curl` | HTTP GET requests to `/`, `/wp-login.php`, `/?s=query`, `/feed/`, REST API |
| Concurrent Load | `ab` (ApacheBench) | Concurrent requests sent to the load balancer |
| POST Forms | `curl` | Authentication attempts, search POST requests |
| Database Queries | `mysql` | Direct `SELECT`, `SHOW TABLES` on the MariaDB NodePort |

### Jumpstart Node Shutdown Test

```bash
# 1. Verify everything works before shutdown
curl -sk https://192.168.20.100/
ssh root@192.168.10.11 "kubectl get nodes"

# 2. Shut down the jumpstart provisioning node
sudo virsh shutdown jumpstart

# 3. Verify that the entire CMS infrastructure CONTINUES functioning
curl -sk https://192.168.20.100/           # CMS remains accessible
ssh root@192.168.10.11 "kubectl get nodes"  # K3s cluster operational
ssh root@192.168.10.11 "kubectl get pods -n cms"  # MariaDB running

# 4. Generate post-shutdown traffic
./scripts/11_traffic_mix.sh --internal --duration 30
```

### Expected Results

- The CMS continues responding normally after shutting down the jumpstart server.
- Prometheus/Grafana metrics continue to be scraped and displayed.
- The K3s cluster remains fully operational with all 4 nodes.
- Only node provisioning capability (Cobbler/PXE) is offline.

### Verification

```bash
# View TrafficMix report
./scripts/11_traffic_mix.sh --internal --duration 60

# The script outputs:
# - Total requests executed
# - Successful vs failed requests
# - Request rate (reqs/sec) if ab is used
```
