# PHASE 09 — Hot-desk Workstations

## Objectives to Achieve

- [x] Deploy 3 hot-desk workstations (scalable up to 8) connected to the `main` subnet.
- [x] Verify internal ping connectivity to the CMS load balancer.

---

## Technical Implementation

### Deployed Hot-desks

`config.yaml` defines `hotdesks.count: 3` (with `max: 8`). The IP last octet, FQDN and MAC are derived deterministically from the index, so adding hot-desks up to 8 needs no per-node MAC/IP bookkeeping:

| Hostname | IP | MAC | RAM | Disk |
|:---------|:---|:---|:----|:-----|
| main-hotdesk1 | 192.168.20.201 | 52:54:00:10:02:c9 | 768 MB | 10 GB |
| main-hotdesk2 | 192.168.20.202 | 52:54:00:10:02:ca | 768 MB | 10 GB |
| main-hotdesk3 | 192.168.20.203 | 52:54:00:10:02:cb | 768 MB | 10 GB |

> Hot-desks 4-8 (up to `192.168.20.208`, MAC `…:cc`-`…:d0`) are provisioned on demand by raising `hotdesks.count` (see MANUAL section 5).

### Provisioning

The hot-desk VMs are created automatically in `00_init_vms.sh` and provisioned via PXE/Cobbler using the `ubuntu-24.04-x86_64` profile. Puppet applies the `role::hotdesk` class to install basic utilities.

### Connectivity

Each hot-desk node can successfully:
- Access the CMS web interface: `curl http://192.168.20.100`
- Access the internet: `curl http://example.com` (via NAT on the router)
- Reach nodes in the internal network: `ping 192.168.10.11`

### Expanding Hot-desks

See [`docs/MANUAL.md`](../MANUAL.md) → Section 5 "Scaling and Node Expansion" for step-by-step instructions.

### Verification

```bash
# Ping the load balancer from a hot-desk
ssh ubuntu@192.168.20.201 "sudo ping -c 2 192.168.20.100"

# Access CMS
ssh ubuntu@192.168.20.201 "curl -sk https://192.168.20.100/ | head -5"

# Access internet
ssh ubuntu@192.168.20.201 "curl -s http://example.com | head -5"
```
