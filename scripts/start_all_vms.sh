#!/bin/bash
# start_all_vms.sh
#
#
# Descripción:
#   Levanta de forma escalonada todas las máquinas virtuales del clúster siguiendo el orden
#   lógico de dependencias de red (Enrutador -> Servidor Aprovisionador -> Control y bases de datos -> Frontales).
#   Introduce una pequeña pausa (stagger) de seguridad de 5 segundos entre arranques para
#   no saturar la carga de operaciones de E/S del disco físico del host.

set -euo pipefail

export LIBVIRT_DEFAULT_URI="${LIBVIRT_DEFAULT_URI:-qemu:///system}"
VIRSH="${VIRSH:-sudo virsh}"
STAGGER="${STAGGER:-5}"  # Pausa en segundos para escalonar el encendido

# Orden de arranque prioritario: gateway -> aprovisionamiento -> base de datos -> frontales -> puestos
VMS=(
  ufw-router
  jumpstart
  internal-monitor
  internal-master1
  internal-master2
  internal-worker1
  internal-worker2
  internal-storage
  main-lb
  main-cms1
  main-cms2
  main-hotdesk1
  main-hotdesk2
  main-hotdesk3
)

# Inicia un nodo individual comprobando su estado previo en libvirt
start_one() {
  local vm="$1"
  if ! $VIRSH dominfo "$vm" &>/dev/null; then
    echo "[SKIP] VM $vm no registrada en libvirt"
    return 0
  fi
  local state
  state=$($VIRSH domstate "$vm" 2>/dev/null || echo unknown)
  if [[ "$state" == "running" ]]; then
    echo "[OK] La VM $vm ya se encuentra en ejecución"
    return 0
  fi
  echo "[+] Arrancando VM: $vm..."
  if ! $VIRSH start "$vm" 2>/dev/null; then
    echo "  ⚠ Ocurrió una advertencia al intentar arrancar $vm"
  fi
  sleep "$STAGGER"
}

# ==============================================================================
# EJECUCIÓN DEL ARRANQUE GENERAL
# ==============================================================================
echo ">>> Arrancando VMs (Hipervisor: $LIBVIRT_DEFAULT_URI, Pausa: ${STAGGER}s) <<<"
for vm in "${VMS[@]}"; do
  start_one "$vm"
done

echo ""
$VIRSH list --all
