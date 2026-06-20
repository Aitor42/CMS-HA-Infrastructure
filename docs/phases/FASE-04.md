# FASE 04 — Enrutamiento y Securización L3



## Objetivos a conseguir

- [x] Reglas: Acceso desde internet restringido únicamente al LB (Punto 6).
- [x] Reglas: `main` hacia internet e `internal` permitido (Punto 7).
- [x] Reglas: `internal` con salida única a `main` (Punto 8).
- [x] Verificar aislamiento de subredes.

---

## Implementación Técnica

### Router/Firewall (ufw-router)

| Interfaz | Red | IP |
|----------|-----|-----|
| eth0 | WAN (Internet) | DHCP |
| eth1 | internal | 192.168.10.1 |
| eth2 | main | 192.168.20.1 |

### IP Forwarding

```bash
# /etc/sysctl.conf
net.ipv4.ip_forward=1
```

### Políticas por Defecto (UFW)

```bash
ufw default deny incoming
ufw default deny forward
ufw default allow outgoing
```

### Reglas de NAT (/etc/ufw/before.rules)

**DNAT (acceso externo → LB):**
```
*nat
:PREROUTING ACCEPT [0:0]
:POSTROUTING ACCEPT [0:0]
-A PREROUTING -i eth0 -p tcp --dport 80 -j DNAT --to-destination 192.168.20.100:80
-A PREROUTING -i eth0 -p tcp --dport 443 -j DNAT --to-destination 192.168.20.100:443
-A POSTROUTING -s 192.168.20.0/24 -o eth0 -j MASQUERADE
COMMIT
```

### Reglas de Forwarding

| Origen | Destino | Acción | Punto del enunciado |
|--------|---------|--------|---------------------|
| Internet (eth0) → LB | main (eth2) | ALLOW (DNAT) | Punto 6 |
| main (eth2) → Internet (eth0) | WAN | ALLOW | Punto 7 |
| main (eth2) → internal (eth1) | internal | ALLOW | Punto 7 |
| internal (eth1) → main (eth2) | main | ALLOW | Punto 8 |
| internal (eth1) → Internet (eth0) | WAN | DENY (implicit) | Punto 8 |

### Scripts Asociados

- `scripts/05_setup_ufw.sh` — Configuración del firewall perimetral y nodal

### Verificación

```bash
# En el router:
ssh root@192.168.10.1 "ufw status verbose"
ssh root@192.168.10.1 "iptables -t nat -L -n -v"

# Desde un hot-desk: debe acceder a internet y a internal
ssh root@192.168.20.201 "curl -s http://example.com"
ssh root@192.168.20.201 "ping -c 2 192.168.10.11"

# Desde internal: no debe acceder a internet
ssh root@192.168.10.11 "curl -s --connect-timeout 5 http://example.com"  # Debe fallar
ssh root@192.168.10.11 "ping -c 2 192.168.20.100"  # Debe funcionar
```
