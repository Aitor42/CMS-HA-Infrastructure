#!/bin/bash
# verify_all.sh
#
#
# Descripción:
#   Realiza un diagnóstico estructurado fase a fase para validar el correcto funcionamiento
#   y estado de todos los componentes desplegados (máquinas virtuales, redes, servicios de aprovisionamiento,
#   Puppet, balanceador Nginx, Apache, clúster K3s, MariaDB, monitorización y replicación DRBD).
#   Debe ejecutarse desde el hipervisor local.

set -uo pipefail

# Cargar la configuración global
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/config.sh"

FAILS=0
VIRSH="${VIRSH:-virsh -c qemu:///system}"

echo -e "${GREEN}=========================================================${NC}"
echo -e "${GREEN}   CMS Infrastructure Verifier (Health Check)   ${NC}"
echo -e "${GREEN}=========================================================${NC}"

# ==============================================================================
# FASE 00: ESTADO DE VIRTUALIZACIÓN (NETWORKS & VMs)
# ==============================================================================
echo ""
info "=== Fase 00: Estado de las Redes y Máquinas Virtuales en Libvirt ==="
vms=(
    "jumpstart" "ufw-router" "internal-monitor" "internal-storage"
    "internal-master1" "internal-master2" "internal-worker1" "internal-worker2"
    "main-lb" "main-cms1" "main-cms2"
)
# Agregar dinámicamente los puestos de trabajo hot-desk definidos en libvirt
while read -r hd; do
    if [ -n "$hd" ]; then
        vms+=("$hd")
    fi
done < <($VIRSH list --all --name 2>/dev/null | grep '^main-hotdesk' || true)

all_vms_running=true
for vm in "${vms[@]}"; do
    state=$($VIRSH domstate "$vm" 2>/dev/null || echo "not-defined")
    if [ "$state" = "running" ]; then
        success "VM $vm está en estado: $state"
    else
        error "VM $vm está en estado: $state (debería estar running)"
        all_vms_running=false
        ((FAILS++)) || true
    fi
done

# Validar redes virtuales asociadas en libvirt
for net in "internal" "main"; do
    net_active=$($VIRSH net-info "$net" 2>/dev/null | grep -i "active:" | awk '{print $2}' || echo "no")
    if [ "$net_active" = "yes" ]; then
        success "Red virtual '$net' está activa"
    else
        error "Red virtual '$net' NO se encuentra activa"
        ((FAILS++)) || true
    fi
done

# ==============================================================================
# FASE 01: SERVIDOR COBBLER (JUMPSTART)
# ==============================================================================
echo ""
info "=== Fase 01: Servidor Cobbler en Jumpstart ==="
if ssh $SSH_OPTS root@$JUMPSTART_IP true 2>/dev/null; then
    success "Acceso SSH a jumpstart ($JUMPSTART_IP) OK"
    
    # Comprobar estado de los servicios internos de aprovisionamiento
    for svc in "cobblerd" "apache2" "isc-dhcp-server" "bind9" "tftpd-hpa"; do
        if ssh $SSH_OPTS root@$JUMPSTART_IP "systemctl is-active --quiet $svc" 2>/dev/null; then
            success "  Servicio $svc en jumpstart está activo"
        else
            error "  Servicio $svc en jumpstart NO está activo"
            ((FAILS++)) || true
        fi
    done
    
    # Comprobar volumen de hosts registrados en el PXE
    sys_count=$(ssh $SSH_OPTS root@$JUMPSTART_IP "cobbler system list 2>/dev/null | wc -l")
    sys_count=${sys_count:-0}
    if [ "$sys_count" -ge 13 ]; then
        success "Cobbler tiene $sys_count sistemas registrados (correcto)"
    else
        warn "Cobbler tiene $sys_count sistemas registrados (esperados >= 13)"
    fi
else
    error "Imposible conectar por SSH con jumpstart ($JUMPSTART_IP). Saltando comprobación."
    ((FAILS++)) || true
fi

# ==============================================================================
# FASE 02: GESTIÓN DE CONFIGURACIÓN (PUPPET)
# ==============================================================================
echo ""
info "=== Fase 02: Puppet Server y Agentes ==="
if ssh $SSH_OPTS root@$JUMPSTART_IP true 2>/dev/null; then
    # Validar el daemon del servidor
    if ssh $SSH_OPTS root@$JUMPSTART_IP "systemctl is-active --quiet puppetserver" 2>/dev/null; then
        success "Puppet Server está activo en jumpstart"
    else
        error "Puppet Server NO está activo en jumpstart"
        ((FAILS++)) || true
    fi
    
    # Validar el almacén de certificados firmados
    signed_certs=$(ssh $SSH_OPTS root@$JUMPSTART_IP "puppetserver ca list --all 2>/dev/null | grep -c '(SHA256)'" || echo "0")
    if [ "$signed_certs" -ge 9 ]; then
        success "Puppet tiene $signed_certs certificados firmados (correcto)"
    else
        warn "Puppet tiene $signed_certs certificados firmados (esperados >= 9)"
    fi
else
    error "Puppet Server no verificado (Jumpstart inalcanzable)"
    ((FAILS++)) || true
fi

# Validar el estado local del daemon agent en los nodos clientes
nodes=(
    "192.168.10.20|internal-monitor"
    "192.168.10.11|internal-master1"
    "192.168.10.12|internal-master2"
    "192.168.10.13|internal-worker1"
    "192.168.10.14|internal-worker2"
    "192.168.10.15|internal-storage"
    "192.168.20.100|main-lb"
    "192.168.20.101|main-cms1"
    "192.168.20.102|main-cms2"
)
for entry in "${nodes[@]}"; do
    IFS='|' read -r ip name <<< "$entry"
    if ssh $SSH_OPTS root@$ip true 2>/dev/null; then
        if ssh $SSH_OPTS root@$ip "systemctl is-active --quiet puppet" 2>/dev/null; then
            success "  Agente Puppet activo en $name ($ip)"
        else
            warn "  Agente Puppet inactivo en $name ($ip)"
        fi
    else
        warn "  Nodo $name ($ip) inaccesible por SSH"
    fi
done

# ==============================================================================
# FASE 03: BALANCEADOR Y CAPA WEB (NGINX & APACHE)
# ==============================================================================
echo ""
info "=== Fase 03: Balanceador Nginx y Servidores WordPress ==="
if ssh $SSH_OPTS root@$LB_IP true 2>/dev/null; then
    if ssh $SSH_OPTS root@$LB_IP "systemctl is-active --quiet nginx" 2>/dev/null; then
        success "Nginx Load Balancer activo en main-lb ($LB_IP)"
        
        # Validar la presencia física del certificado SSL de producción
        if ssh $SSH_OPTS root@$LB_IP "test -f /etc/nginx/ssl/cms.crt" 2>/dev/null; then
            success "  Certificado SSL de Nginx disponible en main-lb"
        else
            error "  Certificado SSL de Nginx ausente en main-lb"
            ((FAILS++)) || true
        fi
    else
        error "Nginx Load Balancer inactivo en main-lb"
        ((FAILS++)) || true
    fi
else
    warn "main-lb ($LB_IP) inaccesible por SSH"
fi

for cms_ip in "192.168.20.101" "192.168.20.102"; do
    if ssh $SSH_OPTS root@$cms_ip true 2>/dev/null; then
        if ssh $SSH_OPTS root@$cms_ip "systemctl is-active --quiet apache2" 2>/dev/null; then
            success "Apache2 activo en frontal CMS $cms_ip"
            # Validar inyección del wp-config.php
            if ssh $SSH_OPTS root@$cms_ip "test -f /var/www/html/wp-config.php" 2>/dev/null; then
                success "  WordPress instalado correctamente en $cms_ip"
            else
                error "  wp-config.php ausente en frontal $cms_ip"
                ((FAILS++)) || true
            fi
        else
            error "Apache2 inactivo en frontal CMS $cms_ip"
            ((FAILS++)) || true
        fi
    else
        warn "Frontal CMS $cms_ip inaccesible por SSH"
    fi
done

# ==============================================================================
# FASE 04: CLÚSTER KUBERNETES Y BASE DE DATOS (K3S & MARIADB)
# ==============================================================================
echo ""
info "=== Fase 04: Clúster K3s HA y Base de Datos MariaDB ==="
if ssh $SSH_OPTS root@$MASTER1_IP true 2>/dev/null; then
    # Esperar a que kubectl responda (hasta 45 segundos)
    k3s_ready=false
    for i in {1..9}; do
        if ssh $SSH_OPTS root@$MASTER1_IP "kubectl get nodes &>/dev/null" 2>/dev/null; then
            k3s_ready=true
            break
        fi
        info "  Esperando a que el plano de control de K3s responda (reintento $i/9)..."
        sleep 5
    done

    if [ "$k3s_ready" = "true" ]; then
        success "kubectl operativo en internal-master1"
        ssh $SSH_OPTS root@$MASTER1_IP "kubectl get nodes"
        
        # Validar que el pod con la base de datos se encuentra levantado
        db_pod_status=$(ssh $SSH_OPTS root@$MASTER1_IP "kubectl get pods -n cms -l app=mariadb -o jsonpath='{.items[0].status.phase}' 2>/dev/null" || echo "Unknown")
        if [ "$db_pod_status" = "Running" ]; then
            success "Pod de MariaDB (Namespace 'cms') en estado Running"
        else
            error "Pod de MariaDB (Namespace 'cms') en estado: $db_pod_status"
            ((FAILS++)) || true
        fi
    else
        error "Comando kubectl inaccesible o K3s no operativo en internal-master1"
        ((FAILS++)) || true
    fi
else
    warn "internal-master1 ($MASTER1_IP) inaccesible por SSH"
fi

# ==============================================================================
# FASE 05: MONITORIZACIÓN (PROMETHEUS + GRAFANA)
# ==============================================================================
echo ""
info "=== Fase 05: Prometheus y Grafana ==="
if ssh $SSH_OPTS root@$MONITOR_IP true 2>/dev/null; then
    for svc in "prometheus" "grafana-server"; do
        if ssh $SSH_OPTS root@$MONITOR_IP "systemctl is-active --quiet $svc" 2>/dev/null; then
            success "Servicio $svc en monitor activo"
        else
            error "Servicio $svc en monitor inactivo"
            ((FAILS++)) || true
        fi
    done
else
    warn "internal-monitor ($MONITOR_IP) inaccesible por SSH"
fi

# Validar que node_exporter corre localmente en cada nodo para colectar recursos
for entry in "${nodes[@]}"; do
    IFS='|' read -r ip name <<< "$entry"
    if ssh $SSH_OPTS root@$ip true 2>/dev/null; then
        if ssh $SSH_OPTS root@$ip "systemctl is-active --quiet prometheus-node-exporter" 2>/dev/null; then
            success "  node_exporter activo en $name ($ip)"
        else
            warn "  node_exporter inactivo en $name ($ip)"
        fi
    fi
done

# ==============================================================================
# FASE 06: SEGURIDAD (UFW ROUTING & LOCAL FIREWALLS)
# ==============================================================================
echo ""
info "=== Fase 06: Cortafuegos UFW ==="
if ssh $SSH_OPTS root@$ROUTER_IP true 2>/dev/null; then
    if ssh $SSH_OPTS root@$ROUTER_IP "ufw status | grep -q 'Status: active'" 2>/dev/null; then
        success "Cortafuegos UFW activo en ufw-router"
        
        # Validar enrutamiento
        forward=$(ssh $SSH_OPTS root@$ROUTER_IP "sysctl -n net.ipv4.ip_forward")
        forward=${forward:-0}
        if [ "$forward" -eq 1 ]; then
            success "  IP Forwarding (Enrutamiento) activo en ufw-router"
        else
            error "  IP Forwarding deshabilitado en ufw-router"
            ((FAILS++)) || true
        fi
    else
        error "Cortafuegos UFW inactivo en ufw-router"
        ((FAILS++)) || true
    fi
else
    warn "ufw-router ($ROUTER_IP) inaccesible por SSH"
fi

# ==============================================================================
# FASE 07: ALTA DISPONIBILIDAD DE DISCO (DRBD)
# ==============================================================================
echo ""
info "=== Fase 07: Replicación DRBD ==="
drbd_ok=false
drbd_status=""
active_node=""

# Sondear el estado del recurso distribuido en master1 o master2
if ssh $SSH_OPTS root@$MASTER1_IP true 2>/dev/null; then
    drbd_status=$(ssh $SSH_OPTS root@$MASTER1_IP "drbdadm status cms_data 2>/dev/null" || echo "Offline")
    if [[ "$drbd_status" != "Offline" ]]; then
        drbd_ok=true
        active_node="internal-master1"
    fi
fi

if [ "$drbd_ok" = "false" ]; then
    if ssh $SSH_OPTS root@$MASTER2_IP true 2>/dev/null; then
        drbd_status=$(ssh $SSH_OPTS root@$MASTER2_IP "drbdadm status cms_data 2>/dev/null" || echo "Offline")
        if [[ "$drbd_status" != "Offline" ]]; then
            drbd_ok=true
            active_node="internal-master2"
        fi
    fi
fi

if [ "$drbd_ok" = "true" ]; then
    success "DRBD recurso cms_data está activo (Verificado desde $active_node):"
    
    # Extraer los roles de los nodos en la replicación
    role_m1="Unknown"
    role_m2="Unknown"
    
    if [ "$active_node" = "internal-master1" ]; then
        role_m1=$(echo "$drbd_status" | grep "role:" | head -n 1 | awk -F'role:' '{print $2}' | awk '{print $1}')
        role_m2=$(echo "$drbd_status" | grep "peer role:" | head -n 1 | awk -F'role:' '{print $2}' | awk '{print $1}')
    else
        role_m2=$(echo "$drbd_status" | grep "role:" | head -n 1 | awk -F'role:' '{print $2}' | awk '{print $1}')
        role_m1=$(echo "$drbd_status" | grep "peer role:" | head -n 1 | awk -F'role:' '{print $2}' | awk '{print $1}')
    fi
    
    echo "  • internal-master1: $role_m1"
    echo "  • internal-master2: $role_m2"
    
    # Comprobar el punto de montaje activo (/mnt/data/mariadb) en la pareja de maestros
    mounted_m1=false
    mounted_m2=false
    
    if ssh $SSH_OPTS root@$MASTER1_IP "mountpoint -q /mnt/data/mariadb" 2>/dev/null; then
        mounted_m1=true
    fi
    if ssh $SSH_OPTS root@$MASTER2_IP "mountpoint -q /mnt/data/mariadb" 2>/dev/null; then
        mounted_m2=true
    fi
    
    if [ "$mounted_m1" = "true" ]; then
        success "FileSystem DRBD montado en /mnt/data/mariadb en internal-master1 (Primary)"
    elif [ "$mounted_m2" = "true" ]; then
        success "FileSystem DRBD montado en /mnt/data/mariadb en internal-master2 (Primary)"
    else
        warn "FileSystem DRBD NO montado en ningún maestro (comprobar promoción manual)"
    fi
else
    error "DRBD inactivo o desconfigurado en ambos nodos maestros"
    ((FAILS++)) || true
fi

# ==============================================================================
# CONCLUSIÓN Y CÓDIGO DE RETORNO
# ==============================================================================
echo ""
echo -e "${GREEN}=========================================================${NC}"
echo -e "${GREEN}             Diagnóstico E2E Completado                  ${NC}"
echo -e "${GREEN}=========================================================${NC}"
echo ""
echo "════════════════════════════════════"
if [ $FAILS -eq 0 ]; then
  success "  ✔ Todas las comprobaciones de salud pasaron con éxito"
  exit 0
else
  error "  ✗ Se detectaron $FAILS errores en el diagnóstico de la infraestructura"
  exit 1
fi
