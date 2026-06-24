#!/bin/bash
# sync_vm_clocks.sh
# Sincroniza los relojes de todas las máquinas virtuales con la hora actual del hipervisor.
# Útil después de pausar/reanudar las VMs para evitar desajustes en K3s/etcd.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/config.sh"

CUR_TIME=$(date +%s)
echo -e "${GREEN}=========================================================${NC}"
echo -e "${GREEN}   SINCRONIZADOR DE RELOJES DE MÁQUINAS VIRTUALES        ${NC}"
echo -e "${GREEN}=========================================================${NC}"
echo "Hora de referencia (Hipervisor): $(date)"
echo ""

# Lista completa de IPs de las VMs (incluyendo router)
vms=(
    "$ROUTER_IP|ufw-router"
    "$JUMPSTART_IP|jumpstart"
    "$MONITOR_IP|internal-monitor"
    "$STORAGE_IP|internal-storage"
    "$MASTER1_IP|internal-master1"
    "$MASTER2_IP|internal-master2"
    "$WORKER1_IP|internal-worker1"
    "$WORKER2_IP|internal-worker2"
    "$LB_IP|main-lb"
    "$CMS1_IP|main-cms1"
    "$CMS2_IP|main-cms2"
)

for entry in "${vms[@]}"; do
    IFS='|' read -r ip name <<< "$entry"
    echo -n "[+] Sincronizando $name ($ip)... "
    
    # Intentar SSH rápido
    if ssh $SSH_OPTS -o ConnectTimeout=3 root@$ip "date -s @$CUR_TIME && (systemctl restart systemd-timesyncd || true)" &>/dev/null; then
        echo -e "${GREEN}OK${NC}"
    else
        echo -e "${RED}FALLIDO (Inaccesible o error de SSH)${NC}"
    fi
done

echo ""
echo -e "${GREEN}=========================================================${NC}"
echo -e "${GREEN}             Sincronización Finalizada                   ${NC}"
echo -e "${GREEN}=========================================================${NC}"
