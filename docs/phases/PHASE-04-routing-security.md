# PHASE 04 — Routing and L3 Hardening

## Objectives to Achieve

- [x] Publish only HTTP/S on the LB VIP to the WAN.
- [x] Allow `main` and `internal` outbound access through NAT.
- [x] Allow inter-zone traffic while keeping the WAN out of both subnets.
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

The block is rendered from `templates/ufw/nat-rules` with the configured
`network.internal.cidr`, `network.main.cidr`, `network.main.vip` and the WAN
interface discovered at runtime. **DNAT** publishes the keepalived VIP to the
WAN; **MASQUERADE** gives both lab subnets outbound access:

```
*nat
:PREROUTING ACCEPT [0:0]
:POSTROUTING ACCEPT [0:0]
-A PREROUTING -i <wan> -p tcp --dport 80 -j DNAT --to-destination <lb-vip>:80
-A PREROUTING -i <wan> -p tcp --dport 443 -j DNAT --to-destination <lb-vip>:443
-A POSTROUTING -s <internal-cidr> -o <wan> -j MASQUERADE
-A POSTROUTING -s <main-cidr> -o <wan> -j MASQUERADE
COMMIT
```

### Forwarding Rules

| Source | Destination | Action | Project Requirement |
|:-------|:------------|:-------|:--------------------|
| Internet (eth0) → LB | main (eth2) | ALLOW (DNAT) | External Access to LB |
| main (eth2) → Internet (eth0) | WAN | ALLOW | Main subnet outbound access |
| main (eth2) → internal (eth1) | internal | ALLOW | Main to internal access |
| internal (eth1) → main (eth2) | main | ALLOW | Internal to main access |
| internal (eth1) → Internet (eth0) | WAN | ALLOW (NAT) | Package and Puppet downloads |

### Implementation

- `internal/phases/ufw` (`cms-ha phase setup-ufw`) — applies the perimeter and node firewall rules.
- `internal/phases/router` (`cms-ha phase bootstrap-router`) — sets the gateway addresses, IP forwarding and NAT.

### Verification

```bash
# On the router:
ssh ubuntu@192.168.10.1 "sudo ufw status verbose"
ssh ubuntu@192.168.10.1 "sudo iptables -t nat -L -n -v"

# From a hot-desk node: should access the internet and the internal subnet
ssh ubuntu@192.168.20.201 "sudo curl -s http://example.com"
ssh ubuntu@192.168.20.201 "sudo ping -c 2 192.168.10.11"

# From the internal subnet: should reach the internet through NAT (needed for
# package/Puppet downloads) and the main-network VIP
ssh ubuntu@192.168.10.11 "sudo curl -s --connect-timeout 5 http://example.com"
ssh ubuntu@192.168.10.11 "sudo ping -c 2 192.168.20.100"  # Should work
```
