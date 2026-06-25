# PHASE 02 — Network Redundancy

## Objectives to Achieve

- [x] Design network redundancy to bypass cable failures.
- [x] Integrate LACP / RSTP in the virtual switches.
- [x] Test interface failure and fault tolerance.

---

## Technical Implementation

### Redundancy Strategy

Redundancy is implemented at multiple levels:

1. **Network level (L2):** STP (Spanning Tree Protocol) enabled on the virtual bridges (`stp="on"` in the XML definition of the libvirt networks).
2. **Service level:** Dual K3s master (HA), dual CMS frontends, DRBD replication between master nodes.
3. **Application level:** Nginx LB distributes traffic between the 2 frontends; K3s automatically redistributes pods upon worker node failure.

### STP Configuration in Virtual Networks

In the XML definitions of the virtual networks (`internal-net.xml`, `main-net.xml`):
```xml
<bridge name="virbr-int" stp="on" delay="0"/>
<bridge name="virbr-main" stp="on" delay="0"/>
```

### Tested Fault Tolerance

| Component | Simulated Failure | Expected Behavior |
|:----------|:------------------|:------------------|
| CMS Frontend 1 | Shut down `main-cms1` | Nginx redirects all traffic to `main-cms2` |
| K3s Agent 1 | Shut down `internal-worker1` | Pods migrate to `internal-worker2` |
| K3s Master 1 | Shut down `internal-master1` | `internal-master2` takes control; DRBD failover occurs |
| Network Cable | Disconnect interface | STP reconverges; traffic takes alternative path |

### Verification

```bash
# Verify active STP on bridges
brctl showstp virbr-int
brctl showstp virbr-main

# Fault tolerance test: shut down a frontend and verify access
sudo virsh shutdown main-cms1
curl -sk https://192.168.20.100/  # Should still work

# Restore
sudo virsh start main-cms1
```
