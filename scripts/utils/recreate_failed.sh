#!/bin/bash
# recreate_failed.sh
#
#
# Descripción:
#   Script utilitario para forzar la destrucción y reinstalación selectiva de todas
#   las máquinas virtuales del clúster (a excepción del servidor Jumpstart). Es sumamente
#   útil si se produce una interrupción en el aprovisionamiento PXE o si se quiere restablecer
#   el estado inicial de los nodos clientes de forma rápida.

set -euo pipefail

# Cargar configuración global
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/config.sh"

VIRSH_URI="${LIBVIRT_DEFAULT_URI:-qemu:///system}"
VIRT_TYPE="kvm"
SUDO_CMD="sudo"

echo ">>> Iniciando recreación selectiva de VMs fallidas <<<"

# Comprobar si KVM está disponible
if [ ! -e /dev/kvm ] || [ ! -w /dev/kvm ]; then
    VIRT_TYPE="qemu"
fi

# Elimina la VM y sus discos del hipervisor
cleanup_vm() {
    local nombre="$1"
    if $SUDO_CMD virsh -c "$VIRSH_URI" dominfo "$nombre" &>/dev/null; then
        echo "  [-] Eliminando VM existente: $nombre"
        $SUDO_CMD virsh -c "$VIRSH_URI" destroy "$nombre" 2>/dev/null || true
        $SUDO_CMD virsh -c "$VIRSH_URI" undefine "$nombre" 2>/dev/null || true
    fi
    rm -f "$VM_DIR/${nombre}.qcow2"
    rm -f "$VM_DIR/${nombre}-drbd.qcow2"
}

# Registra e inicia una VM en modo PXE
crear_vm() {
    local nombre="$1"
    local ram="$2"
    local vcpus="$3"
    local disco="$4"
    local mac="$5"
    local red="$6"
    shift 6
    local extra_args=("$@")

    echo "[+] Creando VM: $nombre en la red $red (RAM: $ram MB, vCPUs: $vcpus, Disco: $disco GB)..."
    cleanup_vm "$nombre"

    $SUDO_CMD virt-install \
        --connect "$VIRSH_URI" \
        --virt-type "$VIRT_TYPE" \
        --name="$nombre" \
        --ram="$ram" \
        --vcpus="$vcpus" \
        --disk "path=$VM_DIR/$nombre.qcow2,size=$disco,format=qcow2,bus=virtio" \
        --network network=$red,mac=$mac,model=virtio \
        --pxe \
        --boot hd,network \
        --os-variant=ubuntu24.04 \
        --noautoconsole \
        --wait=0 \
        "${extra_args[@]}"

    echo "  [OK] VM $nombre creada y arrancada en modo PXE"
}

# 1. Recrear el Router perimetral (UFW-Router - 3 NICs)
echo "[+] Recreando ufw-router..."
cleanup_vm "ufw-router"
ROUTER_WAN_NET="default"
if ! $SUDO_CMD virsh -c "$VIRSH_URI" net-info default &>/dev/null 2>&1; then
    ROUTER_WAN_NET="main"
fi
$SUDO_CMD virt-install \
    --connect "$VIRSH_URI" \
    --virt-type "$VIRT_TYPE" \
    --name=ufw-router \
    --ram=3072 \
    --vcpus=1 \
    --disk "path=$VM_DIR/ufw-router.qcow2,size=5,format=qcow2,bus=virtio" \
    --network network=internal,mac=52:54:00:10:01:02,model=virtio \
    --network network=main,mac=52:54:00:10:02:02,model=virtio \
    --network "network=$ROUTER_WAN_NET,mac=52:54:00:10:00:02,model=virtio" \
    --pxe \
    --boot hd,network \
    --os-variant=ubuntu24.04 \
    --noautoconsole \
    --wait=0
echo "  [OK] VM ufw-router creada"

# 2. Recrear internal-monitor
crear_vm "internal-monitor" 3072 1 4 "52:54:00:10:01:10" "internal"

# 3. Recrear internal-storage
crear_vm "internal-storage" 3072 1 8 "52:54:00:10:01:15" "internal"

# 4. Recrear internal-master1 (Con disco secundario para DRBD)
crear_vm "internal-master1" 4096 1 8 "52:54:00:10:01:11" "internal" --disk "path=$VM_DIR/internal-master1-drbd.qcow2,size=3,format=qcow2,bus=virtio"

# 5. Recrear internal-master2 (Con disco secundario para DRBD)
crear_vm "internal-master2" 4096 1 8 "52:54:00:10:01:12" "internal" --disk "path=$VM_DIR/internal-master2-drbd.qcow2,size=3,format=qcow2,bus=virtio"

# 6. Recrear internal-worker1
crear_vm "internal-worker1" 4096 1 8 "52:54:00:10:01:13" "internal"

# 7. Recrear internal-worker2
crear_vm "internal-worker2" 4096 1 8 "52:54:00:10:01:14" "internal"

# 8. Recrear main-lb (Nginx Load Balancer)
crear_vm "main-lb" 3072 1 4 "52:54:00:10:02:64" "main"

# 9. Recrear main-cms1
crear_vm "main-cms1" 3072 1 4 "52:54:00:10:02:65" "main"

# 10. Recrear main-cms2
crear_vm "main-cms2" 3072 1 4 "52:54:00:10:02:66" "main"

# 11. Recrear puestos dinámicos hotdesks
crear_vm "main-hotdesk1" 3072 1 3 "52:54:00:10:02:c9" "main"
crear_vm "main-hotdesk2" 3072 1 3 "52:54:00:10:02:ca" "main"
crear_vm "main-hotdesk3" 3072 1 3 "52:54:00:10:02:cb" "main"

echo ">>> Recreación finalizada. VMs iniciadas en modo PXE. <<<"
