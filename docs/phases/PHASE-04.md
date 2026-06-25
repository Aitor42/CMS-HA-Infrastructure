# PHASE 04 — Routing and L3 Hardening

## Objectives to Achieve

- [x] Rules: Restrict internet access exclusively to the load balancer (LB).
- [x] Rules: Allow outbound internet access from the `main` subnet and permit `internal` communication.
- [x] Rules: Restrict the `internal` subnet to communicate outbound only with `main`.
- [x] Verify subnet isolation.

---

## Technical Implementation

### Router/Firewall (ufw-router)

| Interface | Network | IP |
|:----------|:--------|:---|
| eth0 | WAN (Internet) | DHCP |
| eth1 | internal | 192.168.10.1 |
| eth2 | main | 192.168.20.1 |

### IP Forwarding

```bash
# /etc/sysctl.conf
net.ipv4.ip_forward=1
```

### Default Policies (UFW)

```bash
ufw default deny incoming
ufw default deny forward
ufw default allow outgoing
```

### NAT Rules (/etc/ufw/before.rules)

**DNAT (External access → LB):**
```
*nat
:PREROUTING ACCEPT [0:0]
:POSTROUTING ACCEPT [0:0]
-A PREROUTING -i eth0 -p tcp --dport 80 -j DNAT --to-destination 192.168.20.100:80
-A PREROUTING -i eth0 -p tcp --dport 443 -j DNAT --to-destination 192.168.20.100:443
-A POSTROUTING -s 192.168.20.0/24 -o eth0 -j MASQUERADE
COMMIT
```

### Forwarding Rules

| Source | Destination | Action | Project Requirement |
|:-------|:------------|:-------|:--------------------|
| Internet (eth0) → LB | main (eth2) | ALLOW (DNAT) | External Access to LB |
| main (eth2) → Internet (eth0) | WAN | ALLOW | Main subnet outbound access |
| main (eth2) → internal (eth1) | internal | ALLOW | Main to internal access |
| internal (eth1) → main (eth2) | main | ALLOW | Internal to main access |
| internal (eth1) → Internet (eth0) | WAN | DENY (implicit) | Block internal network from internet |

### Associated Scripts

- `scripts/05_setup_ufw.sh` — Configuration of perimeter and nodal firewalls

### Verification

```bash
# On the router:
ssh root@192.168.10.1 "ufw status verbose"
ssh root@192.168.10.1 "iptables -t nat -L -n -v"

# From a hot-desk node: should access the internet and the internal subnet
ssh root@192.168.20.201 "curl -s http://example.com"
ssh root@192.168.20.201 "ping -c 2 192.168.10.11"

# From internal subnet: must not access the internet
ssh root@192.168.10.11 "curl -s --connect-timeout 5 http://example.com"  # Should fail
ssh root@192.168.10.11 "ping -c 2 192.168.20.100"  # Should work
```
