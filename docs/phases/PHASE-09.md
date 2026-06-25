# PHASE 09 — Hot-desk Workstations

## Objectives to Achieve

- [x] Deploy 8 hot-desk workstations connected to the `main` subnet.
- [x] Verify internal ping connectivity to the CMS load balancer.

---

## Technical Implementation

### Deployed Hot-desks

| Hostname | IP | MAC | RAM | Disk |
|:---------|:---|:---|:----|:-----|
| main-hotdesk1 | 192.168.20.201 | 52:54:00:10:02:c9 | 512 MB | 5 GB |
| main-hotdesk2 | 192.168.20.202 | 52:54:00:10:02:ca | 512 MB | 5 GB |
| main-hotdesk3 | 192.168.20.203 | 52:54:00:10:02:cb | 512 MB | 5 GB |
| main-hotdesk4 | 192.168.20.204 | 52:54:00:10:02:cc | 512 MB | 5 GB |
| main-hotdesk5 | 192.168.20.205 | 52:54:00:10:02:cd | 512 MB | 5 GB |
| main-hotdesk6 | 192.168.20.206 | 52:54:00:10:02:ce | 512 MB | 5 GB |
| main-hotdesk7 | 192.168.20.207 | 52:54:00:10:02:cf | 512 MB | 5 GB |
| main-hotdesk8 | 192.168.20.208 | 52:54:00:10:02:d0 | 512 MB | 5 GB |

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
ssh root@192.168.20.201 "ping -c 2 192.168.20.100"

# Access CMS
ssh root@192.168.20.201 "curl -sk https://192.168.20.100/ | head -5"

# Access internet
ssh root@192.168.20.201 "curl -s http://example.com | head -5"
```
