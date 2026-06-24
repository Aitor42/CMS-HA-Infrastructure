#!/bin/bash
# shrink_vm_ram.sh
#
#
# Descripción:
#   Una vez finalizado el aprovisionamiento PXE (que requiere un mínimo de 3-4GB de RAM
#   para evitar desbordamientos Squashfs), este script se encarga de apagar de forma limpia
#   todas las VMs, reconfigurar sus parámetros de memoria RAM física y vCPUs en libvirt
#   al tamaño objetivo final de producción (optimizando recursos), y volver a arrancarlas
#   opcionalmente.
#
# Uso:
#   bash scripts/shrink_vm_ram.sh                 # Apaga y reconfigura las VMs (quedan apagadas)
#   START_VMS=1 bash scripts/shrink_vm_ram.sh      # Apaga, reconfigura y enciende de inmediato

set -euo pipefail

export LIBVIRT_DEFAULT_URI="${LIBVIRT_DEFAULT_URI:-qemu:///system}"
VIRSH="${VIRSH:-sudo virsh}"
SHUTDOWN_TIMEOUT="${SHUTDOWN_TIMEOUT:-90}"
START_VMS="${START_VMS:-0}"

# ==============================================================================
# INVENTARIO Y DIMENSIONAMIENTO FINAL DE PRODUCCIÓN
# Formato: nombre_vm|RAM_FINAL_MB|vCPUS_FINALES
# ==============================================================================
VM_SPECS=(
  "jumpstart|2048|2"
  "ufw-router|768|1"
  "internal-monitor|1024|1"
  "internal-master1|1024|1"
  "internal-master2|1024|1"
  "internal-worker1|768|1"
  "internal-worker2|768|1"
  "internal-storage|1024|1"
  "main-lb|768|1"
  "main-cms1|1024|1"
  "main-cms2|1024|1"
  "main-hotdesk1|768|1"
  "main-hotdesk2|768|1"
  "main-hotdesk3|768|1"
)

# Apaga de manera limpia (ACPI) la VM indicada, cayendo en force-stop tras timeout
stop_vm() {
  local vm="$1"
  if ! $VIRSH dominfo "$vm" &>/dev/null; then
    return 0
  fi
  local state
  state=$($VIRSH domstate "$vm" 2>/dev/null || echo unknown)
  [[ "$state" == "shut off" ]] && return 0

  echo "  [-] Deteniendo: $vm (estado actual: $state)..."
  if [[ "$state" == "running" ]]; then
    $VIRSH shutdown "$vm" 2>/dev/null || true
    local waited=0
    while [[ $waited -lt $SHUTDOWN_TIMEOUT ]]; do
      state=$($VIRSH domstate "$vm" 2>/dev/null || echo unknown)
      [[ "$state" == "shut off" ]] && return 0
      sleep 3
      waited=$((waited + 3))
    done
    echo "    [!] Timeout superado. Forzando apagado eléctrico para $vm..."
  fi
  $VIRSH destroy "$vm" 2>/dev/null || true
  sleep 1
}

# Modifica los límites de RAM/vCPUs del XML persistente con la VM apagada
apply_spec_offline() {
  local vm="$1" mb="$2" vcpus="$3"
  local kib=$((mb * 1024))

  if ! $VIRSH dominfo "$vm" &>/dev/null; then
    echo "[SKIP] VM $vm no registrada"
    return 0
  fi

  local state
  state=$($VIRSH domstate "$vm" 2>/dev/null || echo unknown)
  if [[ "$state" != "shut off" ]]; then
    echo "[ERROR] La VM $vm está en estado '$state'; no se puede modificar memoria"
    return 1
  fi

  echo "[+] Reconfigurando $vm → ${mb} MB, ${vcpus} vCPU..."
  $VIRSH setmaxmem "$vm" "$kib" --config
  $VIRSH setmem "$vm" "$kib" --config
  $VIRSH setvcpus "$vm" "$vcpus" --maximum --config 2>/dev/null || true
  $VIRSH setvcpus "$vm" "$vcpus" --config
}

# ==============================================================================
# SECUENCIA DE AJUSTE
# ==============================================================================
echo ">>> Deteniendo todas las VMs registradas (URI: $LIBVIRT_DEFAULT_URI) <<<"
for entry in "${VM_SPECS[@]}"; do
  IFS='|' read -r vm _ _ <<< "$entry"
  stop_vm "$vm"
done

echo ""
echo ">>> Aplicando reducción de RAM/vCPU (Modificaciones persistentes offline) <<<"
for entry in "${VM_SPECS[@]}"; do
  IFS='|' read -r vm mb vcpu <<< "$entry"
  apply_spec_offline "$vm" "$mb" "$vcpu"
done

# Levantar de nuevo el clúster si se activa START_VMS=1
if [[ "$START_VMS" == "1" ]]; then
  echo ""
  echo ">>> Iniciando todas las VMs con su memoria optimizada <<<"
  for entry in "${VM_SPECS[@]}"; do
    IFS='|' read -r vm _ _ <<< "$entry"
    if $VIRSH dominfo "$vm" &>/dev/null; then
      echo "  [+] Iniciando: $vm"
      $VIRSH start "$vm" 2>/dev/null || echo "    [WARN] No se pudo arrancar la VM: $vm"
    fi
  done
else
  echo ""
  echo "VMs detenidas y reconfiguradas. Para arrancarlas, invoque: START_VMS=1 $0"
fi

# Volcar estadísticas del hipervisor
echo ""
echo "=== Estado de memoria física del host ==="
free -h | head -2
echo ""
echo "=== Resumen de memoria asignada por VM ==="
total=0
for entry in "${VM_SPECS[@]}"; do
  IFS='|' read -r vm mb _ <<< "$entry"
  if $VIRSH dominfo "$vm" &>/dev/null; then
    max=$($VIRSH dominfo "$vm" | awk -F: '/Max memory/{gsub(/[^0-9]/,"",$2); print $2}')
    state=$($VIRSH domstate "$vm" 2>/dev/null)
    echo "  $vm: $state, RAM máxima = ${max} KiB (Objetivo: $((mb * 1024)) KiB)"
    max=${max:-0}
    total=$((total + max))
  fi
done
echo "  Consumo de RAM sumado máximo: ~$((total / 1024)) MB ($((total / 1024 / 1024)) GiB)"
