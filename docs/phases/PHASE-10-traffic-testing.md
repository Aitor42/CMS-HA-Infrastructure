# PHASE 10 — TrafficMix, End-to-End Tests, and Jumpstart Shutdown

## Objectives to Achieve

- [x] Test traffic generation with TrafficMix from the WAN subnet.
- [x] Run database queries or management scripts from the integrated hot-desks.
- [x] Shut down the Jumpstart node and demonstrate full system survival.

---

## Technical Implementation

### TrafficMix Script

> **Current implementation:** `cms-ha traffic` runs a **k6** profile
> (`templates/traffic/cms-smoke.js`) inside a pinned `grafana/k6` container and
> reports RPS, p95/p99 and error rate (optionally streamed to Prometheus).
> ```bash
> ./cms-ha traffic --external --duration 120
> ./cms-ha traffic --internal --duration 60 --with-db
> ```
> The v1 shell script `scripts/11_traffic_mix.sh` shown below is **deprecated**
> (reference only, see `scripts/README.md`).

The legacy script `scripts/11_traffic_mix.sh` generates simulated traffic against the CMS web infrastructure. It supports two modes:

**External Mode** (simulates client traffic from the Internet):
```bash
./scripts/11_traffic_mix.sh --external --target <ROUTER_WAN_IP> --duration 120
```

**Internal Mode** (runs locally from a hot-desk node):
```bash
./scripts/11_traffic_mix.sh --internal --duration 60 --with-db
```

### Functional Checks (`cms-ha verify`, Phase 10)

> **Automated:** `cms-ha verify` runs a functional phase on top of the
> end-to-end HTTP checks of Phase 08. It proves the deployed stack *works as an
> application*, not only that its units are active. `tests/integration/run.sh
> --with-traffic` (used by the CI workflows) adds a short k6 load profile after
> it, so every CI run gates on both.

| Check | What it proves |
|:------|:---------------|
| `CMS pages` | `/wp-login.php` returns the credential form, `/feed/` generates an RSS/Atom document from the database, an asset referenced by the home page is served, and plain HTTP redirects to HTTPS |
| `CMS REST API` | `/wp-json/` lists its namespaces, `/wp-json/wp/v2/posts` returns the post `wp core install` created, and that post's permalink renders as HTML through the VIP |
| `CMS frontends` | Each frontend's own `wp-cli` finds WordPress installed and counts posts (its whole database path), and the uploads directory is the NFS export instead of local disk |
| `Load balancers` | Each load balancer answers the site directly, so a broken backup node does not stay hidden behind the VIP |
| `MariaDB` | The NodePort is reachable and a SQL round-trip reads `wp_posts`/`wp_options` and writes a throwaway table, proving the database accepts writes |
| `Prometheus probes` | The alert rules are loaded and every `blackbox_exporter` probe of the VIP and the frontends reports success |
| `Grafana` | The datasource and at least one dashboard are provisioned |

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
ssh ubuntu@192.168.10.11 "sudo -H kubectl get nodes"

# 2. Shut down the jumpstart provisioning node
sudo virsh shutdown jumpstart

# 3. Verify that the entire CMS infrastructure CONTINUES functioning
curl -sk https://192.168.20.100/           # CMS remains accessible
ssh ubuntu@192.168.10.11 "sudo -H kubectl get nodes"  # K3s cluster operational
ssh ubuntu@192.168.10.11 "sudo -H kubectl get pods -n cms"  # MariaDB running

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
