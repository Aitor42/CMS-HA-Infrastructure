#!/bin/bash
# install_by_batches.sh
#
#
# Descripción:
#   Aprovisiona las máquinas virtuales en lotes secuenciales para evitar saturar
#   la memoria RAM física del servidor (limitada a 27 GB). Permite realizar la
#   instalación desatendida de un grupo, esperar a que finalice y responda a SSH,
#   y reducir su asignación de RAM física al valor final de producción antes de pasar
#   al siguiente lote.
#
# Uso:
#   bash scripts/install_by_batches.sh             # Despliega solo las VMs que falten
#   bash scripts/install_by_batches.sh --force      # Destruye y reinstala todas las VMs

set -euo pipefail

# Cargar configuraciones centrales del proyecto
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/config.sh"

# Estado y banderas de ejecución por defecto
FORCE_REINSTALL=false
NON_INTERACTIVE=false

# Procesar parámetros pasados por el usuario
for arg in "$@"; do
    case "$arg" in
        --force)
            FORCE_REINSTALL=true
            ;;
        --non-interactive|-y|--auto)
            NON_INTERACTIVE=true
            ;;
    esac
done

# Confirmar la destrucción del clúster si se utiliza --force de modo interactivo
if $FORCE_REINSTALL; then
    if ! $NON_INTERACTIVE; then
        echo -e "\033[1;33m[WARN] Modo --force activo: Se destruirán y recrearán TODAS las máquinas virtuales.\033[0m"
        read -p "¿Confirmar borrado completo de discos duros virtuales? (s/N): " confirmacion
        if [[ "$confirmacion" != "s" && "$confirmacion" != "S" ]]; then
            echo "Operación cancelada."
            exit 0
        fi
    fi
fi

# Mapeo estático auxiliar para obtener IPs por nombre de VM
get_ip() {
    local name="$1"
    case "$name" in
        "ufw-router") echo "192.168.10.1" ;;
        "internal-monitor") echo "192.168.10.20" ;;
        "internal-storage") echo "192.168.10.15" ;;
        "internal-master1") echo "192.168.10.11" ;;
        "internal-master2") echo "192.168.10.12" ;;
        "internal-worker1") echo "192.168.10.13" ;;
        "internal-worker2") echo "192.168.10.14" ;;
        "main-lb") echo "192.168.20.100" ;;
        "main-cms1") echo "192.168.20.101" ;;
        "main-cms2") echo "192.168.20.102" ;;
        "main-hotdesk1") echo "192.168.20.201" ;;
        "main-hotdesk2") echo "192.168.20.202" ;;
        "main-hotdesk3") echo "192.168.20.203" ;;
        *) echo "" ;;
    esac
}

# Sondeo de red para verificar la correcta finalización de la instalación
wait_for_ssh() {
    local nombre="$1"
    local ip="$2"
    info "    Esperando a que $nombre ($ip) responda por SSH..."
    local start_time=$(date +%s)
    local rebooted=false
    while true; do
        if ssh -i "${HOST_KEY_FILE}" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=2 root@"$ip" "true" &>/dev/null; then
            success "    [✓] $nombre ($ip) accesible por SSH."
            return 0
        fi
        
        local current_time=$(date +%s)
        local elapsed=$((current_time - start_time))
        
        # Corrección de cuelgue de reinicios post-instalación de Ubuntu (NFS lock / cdrom.mount reboot hang)
        # Si la VM tarda más de 12 minutos sin responder, se fuerza un apagado y encendido (reboot de hardware)
        if [ $elapsed -gt 720 ] && [ "$rebooted" = "false" ]; then
            warn "    [!] $nombre ($ip) inactivo tras $elapsed segundos. Forzando reinicio eléctrico (virsh destroy)..."
            $SUDO_CMD virsh -c "$VIRSH_URI" destroy "$nombre" >/dev/null 2>&1 || true
            sleep 2
            $SUDO_CMD virsh -c "$VIRSH_URI" start "$nombre" >/dev/null 2>&1 || true
            rebooted=true
        fi
        
        # Parada de seguridad tras 20 minutos
        if [ $elapsed -gt 1200 ]; then
            warn "    [WARN] Excedido límite de espera (20 min) para $nombre ($ip). Continuando despliegue..."
            return 1
        fi
        
        sleep 10
    done
}

# Configuración del puente a libvirt
VIRSH_URI="${LIBVIRT_DEFAULT_URI:-qemu:///system}"
VIRT_TYPE="kvm"
SUDO_CMD="sudo"

if [ ! -e /dev/kvm ] || [ ! -w /dev/kvm ]; then
    VIRT_TYPE="qemu"
fi

# Elimina una máquina virtual y sus discos del hipervisor
cleanup_vm() {
    local nombre="$1"
    if $SUDO_CMD virsh -c "$VIRSH_URI" dominfo "$nombre" &>/dev/null; then
        info "  [-] Eliminando VM previa: $nombre"
        $SUDO_CMD virsh -c "$VIRSH_URI" destroy "$nombre" 2>/dev/null || true
        $SUDO_CMD virsh -c "$VIRSH_URI" undefine "$nombre" 2>/dev/null || true
    fi
    rm -f "$VM_DIR/${nombre}.qcow2"
    rm -f "$VM_DIR/${nombre}-drbd.qcow2"
}

# Verifica la presencia física de la VM y su disco
vm_exists() {
    local nombre="$1"
    if $SUDO_CMD virsh -c "$VIRSH_URI" dominfo "$nombre" &>/dev/null && [ -f "$VM_DIR/${nombre}.qcow2" ]; then
        return 0
    fi
    return 1
}

# Registra y enciende la VM en modo PXE
crear_vm() {
    local nombre="$1"
    local ram="$2"
    local vcpus="$3"
    local disco="$4"
    local mac="$5"
    local red="$6"
    shift 6
    local extra_args=("$@")

    if ! $FORCE_REINSTALL && vm_exists "$nombre"; then
        success "  [=] VM $nombre ya configurada. Omitiendo instalación (Utilice --force para forzar recreación)"
        return 0
    fi

    info "  [+] Desplegando VM: $nombre en la red $red (Asignación inicial: $ram MB RAM, $vcpus vCPUs)..."
    cleanup_vm "$nombre"

    $SUDO_CMD virt-install \
        --connect "$VIRSH_URI" \
        --virt-type "$VIRT_TYPE" \
        --name="$nombre" \
        --ram="$ram" \
        --vcpus="$vcpus" \
        --disk "path=$VM_DIR/$nombre.qcow2,size=$disco,format=qcow2,bus=virtio" \
        --network network="$red",mac="$mac",model=virtio \
        --pxe \
        --boot hd,network \
        --os-variant=ubuntu24.04 \
        --noautoconsole \
        --wait=0 \
        "${extra_args[@]}"

    success "  [✓] VM $nombre creada y arrancada en modo PXE"
}

# Reduce la asignación de RAM estática una vez completado el autoinstall
shrink_vm() {
    local nombre="$1"
    local ram_final_mb="$2"
    local ram_final_kb=$((ram_final_mb * 1024))

    if ! $SUDO_CMD virsh -c "$VIRSH_URI" dominfo "$nombre" &>/dev/null; then
        return 0
    fi

    info "  [-] Ajustando memoria RAM de $nombre a ${ram_final_mb} MB..."
    
    # Intento de apagado limpio por ACPI
    if $SUDO_CMD virsh -c "$VIRSH_URI" shutdown "$nombre" >/dev/null 2>&1; then
        for i in {1..15}; do
            if ! $SUDO_CMD virsh -c "$VIRSH_URI" domstate "$nombre" 2>/dev/null | grep -q "running"; then
                break
            fi
            sleep 1
        done
    fi

    # Si no se apaga, apagar de manera forzada
    if $SUDO_CMD virsh -c "$VIRSH_URI" domstate "$nombre" 2>/dev/null | grep -q "running"; then
        $SUDO_CMD virsh -c "$VIRSH_URI" destroy "$nombre" >/dev/null 2>&1 || true
    fi
    sleep 1

    # Guardar límites en la configuración XML persistente
    $SUDO_CMD virsh -c "$VIRSH_URI" setmaxmem "$nombre" "$ram_final_kb" --config >/dev/null 2>&1 || true
    $SUDO_CMD virsh -c "$VIRSH_URI" setmem "$nombre" "$ram_final_kb" --config >/dev/null 2>&1 || true
    
    # Levantar de nuevo con la memoria optimizada
    $SUDO_CMD virsh -c "$VIRSH_URI" start "$nombre" >/dev/null 2>&1 || warn "No se pudo iniciar $nombre tras el ajuste"
    success "  [✓] VM $nombre reiniciada con RAM de ${ram_final_mb} MB"
}

# Orquesta el ciclo completo de creación, espera y optimización de un grupo
procesar_grupo() {
    local num_grupo="$1"
    local desc_grupo="$2"
    shift 2
    local vms_definiciones=("$@")

    echo -e "\n=========================================================================="
    echo -e "${GREEN}  LOTE (GRUPO) $num_grupo: $desc_grupo ${NC}"
    echo -e "=========================================================================="

    # 1. Crear e iniciar el lote de VMs
    for def in "${vms_definiciones[@]}"; do
        IFS='|' read -r nombre ram vcpus disco mac red extra_drbd ram_final <<< "$def"
        if [ "$nombre" == "ufw-router" ]; then
            if ! $FORCE_REINSTALL && vm_exists "ufw-router"; then
                success "  [=] VM ufw-router ya existe. Omitiendo instalación."
            else
                info "  [+] Creando VM: ufw-router (RAM inicial: $ram MB)..."
                cleanup_vm "ufw-router"
                ROUTER_WAN_NET="default"
                if ! $SUDO_CMD virsh -c "$VIRSH_URI" net-info default &>/dev/null 2>&1; then
                    ROUTER_WAN_NET="main"
                fi
                $SUDO_CMD virt-install \
                    --connect "$VIRSH_URI" \
                    --virt-type "$VIRT_TYPE" \
                    --name=ufw-router \
                    --ram="$ram" \
                    --vcpus="$vcpus" \
                    --disk "path=$VM_DIR/ufw-router.qcow2,size=$disco,format=qcow2,bus=virtio" \
                    --network network=internal,mac=52:54:00:10:01:02,model=virtio \
                    --network network=main,mac=52:54:00:10:02:02,model=virtio \
                    --network "network=$ROUTER_WAN_NET,mac=52:54:00:10:00:02,model=virtio" \
                    --pxe \
                    --boot hd,network \
                    --os-variant=ubuntu24.04 \
                    --noautoconsole \
                    --wait=0
                success "  [✓] VM ufw-router creada en modo PXE"
            fi
        else
            if [ "$extra_drbd" == "drbd" ]; then
                crear_vm "$nombre" "$ram" "$vcpus" "$disco" "$mac" "$red" --disk "path=$VM_DIR/${nombre}-drbd.qcow2,size=3,format=qcow2,bus=virtio"
            else
                crear_vm "$nombre" "$ram" "$vcpus" "$disco" "$mac" "$red"
            fi
        fi
    done

    # 2. Esperar al fin del autoinstall
    warn "\n>>> Esperando a que el Lote $num_grupo complete su instalación PXE y responda SSH..."
    
    if $NON_INTERACTIVE; then
        for def in "${vms_definiciones[@]}"; do
            IFS='|' read -r nombre _ _ _ _ _ _ _ <<< "$def"
            local ip=$(get_ip "$nombre")
            if [ -n "$ip" ]; then
                wait_for_ssh "$nombre" "$ip"
            fi
        done
        success ">>> Lote $num_grupo instalado con éxito."
        sleep 5
    else
        echo "Puede comprobar el log del servidor DHCP en Jumpstart:"
        echo "  ssh root@192.168.10.10 \"tail -f /var/log/syslog | grep -E 'DHCPACK|tftp'\""
        read -p "Presione [ENTER] una vez que las VMs del grupo hayan iniciado sesión limpia por SSH..."
    fi

    # 3. Reducir la memoria RAM al footprint de producción
    echo ""
    info "Reduciendo memoria de los nodos del Lote $num_grupo..."
    for def in "${vms_definiciones[@]}"; do
        IFS='|' read -r nombre _ _ _ _ _ _ ram_final <<< "$def"
        shrink_vm "$nombre" "$ram_final"
    done
}

# ==============================================================================
# CONFIGURACIÓN DE LOS LOTES DE APROVISIONAMIENTO
# Formato: NOMBRE|RAM_INICIAL|VCPUS|DISCO|MAC|RED|EXTRA_DRBD|RAM_FINAL
# ==============================================================================
GRUPO1=(
    "ufw-router|3072|1|5|52:54:00:10:01:02|internal||768"
    "internal-monitor|3072|1|4|52:54:00:10:01:10|internal||1024"
    "internal-storage|3072|1|8|52:54:00:10:01:15|internal||1024"
)

GRUPO2=(
    "internal-master1|4096|1|8|52:54:00:10:01:11|internal|drbd|1024"
    "internal-master2|4096|1|8|52:54:00:10:01:12|internal|drbd|1024"
    "main-lb|3072|1|4|52:54:00:10:02:64|main||768"
)

GRUPO3=(
    "internal-worker1|4096|1|8|52:54:00:10:01:13|internal||768"
    "internal-worker2|4096|1|8|52:54:00:10:01:14|internal||768"
)

GRUPO4=(
    "main-cms1|3072|1|4|52:54:00:10:02:65|main||1024"
    "main-cms2|3072|1|4|52:54:00:10:02:66|main||1024"
)

GRUPO5=(
    "main-hotdesk1|3072|1|3|52:54:00:10:02:c9|main||768"
    "main-hotdesk2|3072|1|3|52:54:00:10:02:ca|main||768"
    "main-hotdesk3|3072|1|3|52:54:00:10:02:cb|main||768"
)

echo "=========================================================================="
echo "    Instalación secuencial por lotes de VMs (CMS Infrastructure)         "
echo "=========================================================================="
info "Se mantendrá activa la VM jumpstart y se instalarán las demás en 5 grupos."
info "RAM total requerida por grupo activo: ~9-11 GB."

procesar_grupo "1" "Infraestructura Base (Router, Monitor, Storage)" "${GRUPO1[@]}"
procesar_grupo "2" "Nodos Maestros y Balanceador (Master1, Master2, LB)" "${GRUPO2[@]}"
procesar_grupo "3" "Nodos Workers (Worker1, Worker2)" "${GRUPO3[@]}"
procesar_grupo "4" "Servidores CMS (CMS1, CMS2)" "${GRUPO4[@]}"
procesar_grupo "5" "Puestos de Trabajo (Hotdesks 1-3)" "${GRUPO5[@]}"

echo -e "\n=========================================================================="
success "¡Proceso de instalación finalizado para todos los lotes de VMs!"
echo "=========================================================================="
$SUDO_CMD virsh -c "$VIRSH_URI" list --all
