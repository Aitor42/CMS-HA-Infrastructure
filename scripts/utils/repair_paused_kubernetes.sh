#!/bin/bash
# repair_paused_kubernetes.sh
# Repara el clúster de Kubernetes después de que las VMs hayan estado pausadas por mucho tiempo.
# Sincroniza los relojes de todos los nodos, limpia procesos bloqueados y reinicia los servicios en paralelo.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/config.sh"

CUR_TIME=$(date +%s)
echo -e "${GREEN}=========================================================${NC}"
echo -e "${GREEN}    REPARADOR DE KUBERNETES TRAS PAUSA DE VM            ${NC}"
echo -e "${GREEN}=========================================================${NC}"
echo "Hora de referencia del hipervisor: $(date)"
echo ""

# 1. Sincronizar relojes en todas las VMs
echo ">>> 1. Sincronizando relojes en todas las VMs..."
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
    (
        if ssh $SSH_OPTS -o ConnectTimeout=3 root@$ip "date -s @$CUR_TIME && (systemctl restart chrony 2>/dev/null && chronyc makestep 2>/dev/null || systemctl restart systemd-timesyncd 2>/dev/null || true)" &>/dev/null; then
            echo -e "  [+] $name ($ip): ${GREEN}Sincronizado${NC}"
        else
            echo -e "  [-] $name ($ip): ${RED}Error de conexión/SSH${NC}"
        fi
    ) &
done
wait

echo ""
# 2. Forzar la muerte de cualquier proceso de K3s colgado
echo ">>> 2. Deteniendo y matando procesos K3s colgados en el clúster..."
cluster_nodes=(
    "$MASTER1_IP|k3s"
    "$MASTER2_IP|k3s"
    "$WORKER1_IP|k3s-agent"
    "$WORKER2_IP|k3s-agent"
)

for entry in "${cluster_nodes[@]}"; do
    IFS='|' read -r ip svc <<< "$entry"
    (
        ssh $SSH_OPTS -o ConnectTimeout=3 root@$ip "systemctl stop $svc 2>/dev/null; killall -9 k3s k3s-server k3s-agent 2>/dev/null" &>/dev/null
        echo -e "  [+] Proceso K3s limpio en $ip"
    ) &
done
wait

echo ""
# 3. Iniciar servicios de K3s en paralelo en los maestros para alcanzar quórum
echo ">>> 3. Iniciando servicios K3s en los maestros (en paralelo para quórum)..."
for ip in "$MASTER1_IP" "$MASTER2_IP"; do
    (
        ssh $SSH_OPTS -o ConnectTimeout=5 root@$ip "systemctl start k3s" &>/dev/null && echo -e "  [+] Maestro $ip: ${GREEN}Iniciado${NC}" || echo -e "  [-] Maestro $ip: ${RED}Error de inicio${NC}"
    ) &
done
wait

echo ""
# 4. Iniciar agentes de K3s en los trabajadores
echo ">>> 4. Iniciando agentes K3s en los trabajadores..."
for ip in "$WORKER1_IP" "$WORKER2_IP"; do
    (
        ssh $SSH_OPTS -o ConnectTimeout=5 root@$ip "systemctl start k3s-agent" &>/dev/null && echo -e "  [+] Trabajador $ip: ${GREEN}Iniciado${NC}" || echo -e "  [-] Trabajador $ip: ${RED}Error de inicio${NC}"
    ) &
done
wait

echo ""
# 5. Verificar estado de los nodos
echo ">>> 5. Esperando estabilidad y verificando estado del clúster..."
sleep 5
if ssh $SSH_OPTS -o ConnectTimeout=5 root@$MASTER1_IP "kubectl get nodes" 2>/dev/null; then
    echo -e "\n${GREEN}[OK] El clúster de Kubernetes se ha recuperado correctamente.${NC}"
else
    echo -e "\n${RED}[ERROR] No se pudo obtener el estado de los nodos. K3s podría estar tardando en iniciar.${NC}"
fi

echo ""
echo -e "${GREEN}=========================================================${NC}"
echo -e "${GREEN}             Reparación Finalizada                       ${NC}"
echo -e "${GREEN}=========================================================${NC}"
