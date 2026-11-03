# PHASE 02 — Network Redundancy

## Objectives to Achieve

- [x] Enable STP on the libvirt bridges.
- [ ] Implement physical cable/uplink redundancy (not provided by this single-host lab).
- [ ] Integrate and validate LACP / RSTP (not implemented).
- [x] Provide service/node failover scenarios; runtime acceptance requires a recorded run.

---

## Technical Implementation

### Redundancy Strategy

The implementation combines bridge loop prevention with service redundancy:

1. **Network level (L2):** STP (Spanning Tree Protocol) enabled on the virtual bridges (`stp="on"` in the XML definition of the libvirt networks). This prevents loops where applicable; it does not create a second physical path. No bonded uplinks, LACP or RSTP configuration is supplied.
2. **Service level:** Three K3s masters (etcd quorum), dual CMS frontends, DRBD replication between master nodes.
3. **Application level:** Nginx distributes traffic between the 2 frontends; keepalived moves the LB VIP. MariaDB remains pinned to the mounted DRBD primary, not to workers. The single hypervisor, router and NFS server remain shared availability dependencies.

### STP Configuration in Virtual Networks

In the XML definitions of the virtual networks (`internal-net.xml`, `main-net.xml`):
```xml
<bridge name="virbr-int" stp="on" delay="0"/>
<bridge name="virbr-main" stp="on" delay="0"/>
```

### Implemented Scenarios and Expected Behavior

These are test expectations, not evidence of a completed live run. Use
`cms-ha test failover --require-all --results-file integration-artifacts/failover.json`
after creating the output directory.

| Component | Simulated Failure | Expected Behavior |
|:----------|:------------------|:------------------|
| CMS Frontend 1 | Shut down `main-cms1` | Nginx redirects all traffic to `main-cms2` |
| K3s Agent 1 | Shut down `internal-worker1` | MariaDB stays Running/Ready on a master and the VIP keeps serving |
| DRBD Primary | Shut down or power-cut the current Primary | Survivor confirms fencing, promotes DRBD and recovers MariaDB |
| LB VIP holder | Shut down the current VIP holder | Surviving balancer owns the VIP and serves HTTPS |
| Network Cable | Not covered | No alternate physical path is configured |

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
