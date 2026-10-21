# PHASE 10 — Traffic, End-to-End Tests, and Jumpstart Shutdown

## Objectives to Achieve

- [x] Generate HTTP load against the CMS.
- [x] Run the functional end-to-end checks.
- [x] Shut down the jumpstart node and prove the stack keeps serving.

---

## Technical Implementation

### Load profile (`cms-ha traffic`)

`cms-ha traffic` runs a k6 profile (`templates/traffic/cms-smoke.js`) inside a
pinned `grafana/k6` container and reports RPS, p95/p99 and error rate. With
`traffic.prometheus_rw_url` set in `config.yaml`, the metrics stream into
Prometheus and appear on the HA Overview dashboard during the run.

```bash
# Through the keepalived VIP (default)
./cms-ha traffic --duration 120

# From the internal network against a CMS frontend, plus a DB round-trip
./cms-ha traffic --internal --with-db
```

### Functional checks (`cms-ha verify`, phase 10)

`cms-ha verify` adds application-level checks on top of the HTTP checks of
phase 08, so a green run proves the stack *works*, not only that its units are
active. `tests/integration/run.sh --with-traffic` adds a short k6 profile after
it, so CI gates on both.

| Check | What it proves |
|:------|:---------------|
| `CMS pages` | `/wp-login.php` returns the credential form, `/feed/` generates an RSS/Atom document from the database, an asset referenced by the home page is served, and plain HTTP redirects to HTTPS |
| `CMS REST API` | `/wp-json/` lists its namespaces, `/wp-json/wp/v2/posts` returns the post `wp core install` created, and that post's permalink renders as HTML through the VIP |
| `CMS frontends` | Each frontend's own `wp-cli` finds WordPress installed and counts posts, and the uploads directory is the NFS export instead of local disk |
| `Load balancers` | Each load balancer answers the site directly, so a broken backup node does not stay hidden behind the VIP |
| `MariaDB` | The NodePort is reachable and a SQL round-trip reads `wp_posts`/`wp_options` and writes a throwaway table, proving the database accepts writes |
| `Backup store` | A cluster node reaches the Garage S3 API on the hypervisor (expects `403`), so a host-only store fails here instead of in the nightly backup |
| `Prometheus probes` | The alert rules are loaded and every `blackbox_exporter` probe of the VIP and the frontends reports success |
| `Grafana` | The datasource and at least one dashboard are provisioned |

### Jumpstart Node Shutdown Test

```bash
# 1. Verify everything works before shutdown
curl -sk https://192.168.20.100/
ssh ubuntu@192.168.10.11 "sudo -H kubectl get nodes"

# 2. Shut down the jumpstart provisioning node
sudo virsh shutdown jumpstart

# 3. Verify that the CMS keeps serving
curl -sk https://192.168.20.100/                             # CMS accessible
ssh ubuntu@192.168.10.11 "sudo -H kubectl get nodes"         # cluster operational
ssh ubuntu@192.168.10.11 "sudo -H kubectl get pods -n cms"   # MariaDB running

# 4. Generate post-shutdown traffic
./cms-ha traffic --internal --duration 30
```

### Expected Results

- The CMS keeps responding after the jumpstart server is shut down.
- Prometheus/Grafana keep scraping and displaying metrics.
- All five K3s nodes stay `Ready`.
- Only provisioning (Cobbler/PXE) is offline.
