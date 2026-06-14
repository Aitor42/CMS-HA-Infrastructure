#!/bin/bash
# add_cobbler_nodes.sh
#
#
# Descripción:
#   Agrega la topología de red completa y las especificaciones de hardware (IPs, MACs, FQDNs)
#   al inventario de Cobbler mediante SSH en el nodo Jumpstart. Esto permite que el servidor DHCP
#   asigne IPs estáticas durante el arranque por red y que el instalador PXE sirva la plantilla
#   Autoinstall personalizada para cada nodo.

set -euo pipefail

# Cargar configuraciones globales
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/config.sh"

PROFILE="ubuntu-24.04-x86_64"

echo ">>> Añadiendo nodos a Cobbler via SSH a ${JUMPSTART_IP} <<<"

# Generar la lista de puestos hot-desk dinámicamente según el límite NUM_HOTDESKS
HOTDESK_ENTRIES=""
for i in $(seq 1 "$NUM_HOTDESKS"); do
    ip_last=$((200 + i))
    hex_id=$(printf '%02x' "$ip_last")
    HOTDESK_ENTRIES+="    \"main-hotdesk${i}|52:54:00:10:02:${hex_id}|192.168.20.${ip_last}|main-hotdesk${i}.main.local\""$'\n'
done

ssh ${SSH_OPTS} root@${JUMPSTART_IP} <<REMOTE_EOF
set -euo pipefail

PROFILE="${PROFILE}"

# ==============================================================================
# DEFINICIÓN DEL INVENTARIO DE NODOS
# Formato: NOMBRE_VM|DIRECCIÓN_MAC|DIRECCIÓN_IP|FQDN_HOSTNAME
# ==============================================================================
declare -a NODES=(
    # Router perimetral (Se conecta a la red internal para el arranque por red PXE)
    "ufw-router|52:54:00:10:01:02|192.168.10.1|ufw-router.internal.local"
    
    # Nodos pertenecientes a la red interna (192.168.10.0/24)
    "internal-monitor|52:54:00:10:01:10|192.168.10.20|internal-monitor.internal.local"
    "internal-master1|52:54:00:10:01:11|192.168.10.11|internal-master1.internal.local"
    "internal-master2|52:54:00:10:01:12|192.168.10.12|internal-master2.internal.local"
    "internal-worker1|52:54:00:10:01:13|192.168.10.13|internal-worker1.internal.local"
    "internal-worker2|52:54:00:10:01:14|192.168.10.14|internal-worker2.internal.local"
    "internal-storage|52:54:00:10:01:15|192.168.10.15|internal-storage.internal.local"
    
    # Nodos pertenecientes a la red de clientes/producción (192.168.20.0/24)
    "main-lb|52:54:00:10:02:64|192.168.20.100|main-lb.main.local"
    "main-cms1|52:54:00:10:02:65|192.168.20.101|main-cms1.main.local"
    "main-cms2|52:54:00:10:02:66|192.168.20.102|main-cms2.main.local"
    
    # Lista de puestos dinámicos hot-desks generados en el host
${HOTDESK_ENTRIES})

echo "[+] Registrando \${#NODES[@]} sistemas en Cobbler..."

for ENTRY in "\${NODES[@]}"; do
    IFS='|' read -r NAME MAC IP HOSTNAME <<< "\${ENTRY}"
    
    # Determinar la IP del servidor de aprovisionamiento según la subred del nodo
    if [[ "\${IP}" == 192.168.20.* ]]; then
        COBBLER_SERVER_IP="192.168.20.10"
    else
        COBBLER_SERVER_IP="192.168.10.10"
    fi

    echo "  [+] Añadiendo: \${NAME} (IP: \${IP}, MAC: \${MAC}, Gateway Cobbler: \${COBBLER_SERVER_IP})..."

    # Borrar el registro previo para permitir la idempotencia del script
    cobbler system remove --name="\${NAME}" &>/dev/null || true

    # Agregar el sistema con sus opciones del kernel de instalación (NFS root + Autoinstall URL)
    cobbler system add \\
        --name="\${NAME}" \\
        --mac="\${MAC}" \\
        --ip-address="\${IP}" \\
        --hostname="\${HOSTNAME}" \\
        --profile="\${PROFILE}" \\
        --interface=ens3 \\
        --static=1 \\
        --netboot-enabled=true \\
        --kernel-options="autoinstall ds=nocloud-net;s=http://\${COBBLER_SERVER_IP}/cblr/svc/op/autoinstall/system/\${NAME}/ netboot=nfs nfsroot=\${COBBLER_SERVER_IP}:/var/www/cobbler/distro_mirror/ubuntu-24.04 boot=casper ip=dhcp" \\
        --autoinstall-meta="hostname=\${NAME}"

    echo "    [✓] \${NAME} registrado correctamente."
done

# Sincronizar cambios en los ficheros de configuración de DHCP y DNS (bind/dhcpd)
echo "[+] Sincronizando configuraciones de Cobbler..."
cobbler sync

echo "[+] Listado de sistemas registrados en Cobbler:"
cobbler system list

REMOTE_EOF

echo ">>> Registro finalizado: Todos los nodos clientes configurados en Cobbler <<<"
