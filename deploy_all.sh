#!/bin/bash
# deploy_all.sh
#
# Orquestador principal y punto de entrada para desplegar la infraestructura CMS
# de alta disponibilidad en el entorno simulado.
#
# Ajustes especiales para el Servidor GAR:
#   - Se ejecuta sin privilegios de superusuario (usando qemu:///session si es necesario).
#   - Los discos virtuales se almacenan en /home/$USER/vm_storage debido a falta de espacio en el raíz.
#   - Se redirige TMPDIR a /home/$USER/tmp para evitar fallos de disco lleno en /tmp.

set -euo pipefail

# ==============================================================================
# 1. CONFIGURACIÓN DE COLORES PARA LA SALIDA
# ==============================================================================
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # Sin color

# ==============================================================================
# 2. FUNCIONES AUXILIARES DE MENSAJERÍA
# ==============================================================================
info()    { echo -e "${BLUE}[*]${NC} $*"; }
success() { echo -e "${GREEN}[OK]${NC} $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC} $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }

# Capturar y reportar cualquier error inesperado indicando la línea exacta del fallo
trap 'error "Fallo en la línea $LINENO del script $(basename "$0")"; exit 1' ERR

# ==============================================================================
# 3. DIRECCIONAMIENTO IP DE REFERENCIA DE LA RED
# ==============================================================================
# --- Red Internal (192.168.10.0/24) ---
IP_JUMPSTART_INT="192.168.10.10"
IP_MASTER1="192.168.10.11"
IP_MASTER2="192.168.10.12"
IP_WORKER1="192.168.10.13"
IP_WORKER2="192.168.10.14"
IP_STORAGE="192.168.10.15"
IP_MONITOR="192.168.10.20"

# --- Red Main (192.168.20.0/24) ---
IP_JUMPSTART_MAIN="192.168.20.10"
IP_LB="192.168.20.100"
IP_CMS1="192.168.20.101"
IP_CMS2="192.168.20.102"

# --- Router / Puerta de Enlace (Gateway) ---
IP_ROUTER_INT="192.168.10.1"
IP_ROUTER_MAIN="192.168.20.1"

# ==============================================================================
# 4. CONFIGURACIÓN GENERAL DE ESPERA Y COMPORTAMIENTO
# ==============================================================================
# Cantidad de puestos hot-desk a desplegar
NUM_HOTDESKS="${NUM_HOTDESKS:-3}"

# Nodos cuya conectividad SSH se verificará antes de continuar
WAIT_NODES=(
    "$IP_ROUTER_INT"
    "$IP_MASTER1" "$IP_MASTER2"
    "$IP_WORKER1" "$IP_WORKER2"
    "$IP_STORAGE" "$IP_MONITOR"
    "$IP_LB" "$IP_CMS1" "$IP_CMS2"
)

# Añadir los hot-desks definidos a la lista de espera
for i in $(seq 1 "$NUM_HOTDESKS"); do
    WAIT_NODES+=("192.168.20.$((200 + i))")
done

# Tiempo límite (timeout) para la respuesta SSH de cada nodo (en segundos)
SSH_TIMEOUT="${SSH_TIMEOUT:-600}"

# Omitir esperas de red (útil en pruebas rápidas)
SKIP_WAIT="${SKIP_WAIT:-0}"

# Arrancar VMs existentes sin reinstalar desde el servidor PXE de Cobbler
SKIP_VM_CREATE="${SKIP_VM_CREATE:-0}"

# Forzar la recreación completa de todas las máquinas virtuales (borra discos)
export RECREATE_VMS="${RECREATE_VMS:-0}"

# Directorio base de los scripts de aprovisionamiento
SCRIPTS_DIR="$(cd "$(dirname "$0")/scripts" && pwd)"

# Variables de entorno heredadas para subprocesos
export VM_DIR="${VM_DIR:-$HOME/vm_storage}"
export TMPDIR="${TMPDIR:-$HOME/tmp}"
mkdir -p "$TMPDIR"

# ==============================================================================
# 5. PROCESAMIENTO DE ARGUMENTOS DE LÍNEA DE COMANDOS
# ==============================================================================
DRY_RUN=0
for arg in "$@"; do
    case "$arg" in
        --dry-run) DRY_RUN=1 ;;
        --skip-vm-create) SKIP_VM_CREATE=1 ;;
        --help|-h)
            echo "Uso: $0 [--dry-run] [--skip-vm-create] [--help]"
            echo "  --dry-run          Modo simulación: muestra las fases sin ejecutarlas"
            echo "  --skip-vm-create   Reutiliza VMs existentes (arranque rápido)"
            echo "  SKIP_WAIT=1        Omitir esperas de ping/SSH (entorno de pruebas)"
            echo "  SKIP_VM_CREATE=1   Equivalente al parámetro --skip-vm-create"
            echo "  RECREATE_VMS=1     Borrar discos existentes y reinstalar todo por PXE"
            echo "  SSH_TIMEOUT=N      Establecer tiempo límite de espera por nodo (default: 600s)"
            exit 0
            ;;
    esac
done

# ==============================================================================
# 6. FUNCIONES DE APOYO PARA LAS FASES DE DESPLIEGUE
# ==============================================================================

# Ejecuta una fase determinada (o la simula si está activo --dry-run)
# Uso: run_phase <script> <descripción> [argumentos...]
run_phase() {
    local script="$1"
    local desc="$2"
    shift 2
    if [[ "$DRY_RUN" -eq 1 ]]; then
        info "[DRY-RUN] Se ejecutaría: bash \"$SCRIPTS_DIR/$script\" $*"
    else
        bash "$SCRIPTS_DIR/$script" "$@"
    fi
}

# Realiza un sondeo periódico de SSH hasta que el nodo responda
wait_for_ssh() {
    local host="$1"
    local timeout="$2"
    local elapsed=0
    local interval=10

    info "Esperando respuesta SSH en $host (Límite: ${timeout}s)..."
    while ! ssh -o ConnectTimeout=5 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes \
              "root@$host" true 2>/dev/null; do
        elapsed=$((elapsed + interval))
        if [[ "$elapsed" -ge "$timeout" ]]; then
            warn "Timeout alcanzado esperando SSH en $host tras ${timeout}s"
            return 1
        fi
        sleep "$interval"
    done
    success "Nodo $host responde correctamente a SSH (${elapsed}s)"
    return 0
}

# Controla la espera de todas las VMs de la infraestructura
wait_for_all_nodes() {
    if [[ "$SKIP_WAIT" -eq 1 ]]; then
        warn "SKIP_WAIT=1 configurado: se omite la espera por SSH de los nodos"
        return 0
    fi
    if [[ "$DRY_RUN" -eq 1 ]]; then
        info "[DRY-RUN] Esperaría conexión SSH en: ${WAIT_NODES[*]}"
        return 0
    fi

    local failed=0
    for node in "${WAIT_NODES[@]}"; do
        if ! wait_for_ssh "$node" "$SSH_TIMEOUT"; then
            failed=$((failed + 1))
        fi
    done

    if [[ "$failed" -gt 0 ]]; then
        warn "$failed nodo(s) no respondieron a SSH dentro del tiempo límite."
        warn "Continuando despliegue de todos modos (algunas fases posteriores podrían fallar)..."
    else
        success "Todos los nodos de la infraestructura responden por SSH"
    fi
}

# Ejecuta un script cronometrando su duración
# Uso: timed_phase <nombre_fase> <script> [argumentos...]
timed_phase() {
    local phase_name="$1"
    local script="$2"
    shift 2
    local start_time
    start_time=$(date +%s)

    info "Iniciando $phase_name..."
    run_phase "$script" "$phase_name" "$@"

    local end_time
    end_time=$(date +%s)
    local duration=$((end_time - start_time))
    success "$phase_name completada exitosamente en ${duration}s"
}

# ==============================================================================
# 7. COMPROBACIONES PREVIAS (PRE-FLIGHT CHECKS)
# ==============================================================================
preflight_checks() {
    info "Ejecutando comprobaciones del sistema local..."
    local errors=0

    # 1. Comprobar comandos requeridos de libvirt
    if ! command -v virsh &>/dev/null; then
        error "virsh no instalado. Ejecute: apt install libvirt-daemon-system"
        errors=$((errors + 1))
    else
        success "Comando virsh disponible"
    fi

    if ! command -v virt-install &>/dev/null; then
        error "virt-install no instalado. Ejecute: apt install virtinst"
        errors=$((errors + 1))
    else
        success "Comando virt-install disponible"
    fi

    # 2. Comprobar la conexión con el hipervisor local
    if virsh -c qemu:///system version &>/dev/null; then
        success "Conexión a libvirt establecida a través de qemu:///system"
    elif virsh -c qemu:///session version &>/dev/null; then
        success "Conexión a libvirt establecida a través de qemu:///session"
    else
        error "No se pudo conectar a libvirt. Verifique el estado de libvirtd y grupos del usuario."
        errors=$((errors + 1))
    fi

    # 3. Comprobar soporte KVM para virtualización asistida por hardware
    if [ -e /dev/kvm ] && [ -w /dev/kvm ]; then
        success "Soporte KVM activo (rendimiento nativo)"
    else
        warn "/dev/kvm no disponible. Las VMs usarán emulación QEMU por software (despliegue más lento)."
        warn "Para intentar habilitar KVM: sudo modprobe kvm_intel o kvm_amd"
    fi

    # 4. Comprobar almacenamiento libre en la ruta de las VMs
    local vm_dir="${VM_DIR:-$HOME/vm_storage}"
    mkdir -p "$vm_dir"
    local free_gb
    free_gb=$(df --output=avail -BG "$vm_dir" 2>/dev/null | tail -1 | tr -d ' G')
    if [[ "$free_gb" -lt 30 ]]; then
        warn "Solo quedan ${free_gb}GB libres en $vm_dir (Se recomienda un mínimo de >=30GB)"
    else
        success "Espacio en disco suficiente en $vm_dir: ${free_gb}GB libres"
    fi

    # 5. Comprobar la memoria RAM física del host
    local total_ram_mb
    total_ram_mb=$(awk '/MemTotal/ {printf "%d", $2/1024}' /proc/meminfo)
    if [[ "$total_ram_mb" -lt 16000 ]]; then
        warn "RAM física total detectada: ${total_ram_mb}MB (Recomendado: >=16GB para evitar problemas de OOM)"
    else
        success "Memoria RAM física suficiente: ${total_ram_mb}MB"
    fi

    # 6. Comprobar espacio del directorio raíz de instalación
    local root_free
    root_free=$(df --output=avail -BM / 2>/dev/null | tail -1 | tr -d ' M')
    if [[ "$root_free" -lt 500 ]]; then
        warn "Poco espacio en raíz (/): ${root_free}MB libres. Redirigiendo directorios temporales."
    fi

    if [[ "$errors" -gt 0 ]]; then
        error "Comprobaciones del sistema fallidas ($errors error/es). Cancelando despliegue."
        exit 1
    fi
    success "Comprobaciones del sistema superadas con éxito"
}

# ==============================================================================
# 8. BOOTSTRAP: CONFIGURACIÓN BÁSICA DE RUTAS Y NAT EN EL ROUTER
# ==============================================================================
# Habilita el enrutamiento L3 y NAT temporal en ufw-router para que los nodos clientes
# puedan conectarse a internet para descargar paquetes durante el PXE y aprovisionamiento.
bootstrap_router() {
    local host_key="${HOST_KEY_FILE:-$HOME/.ssh/id_ed25519_gar}"
    if [ ! -f "${host_key}" ]; then
        host_key="$HOME/.ssh/id_ed25519"
    fi
    info "Inicializando enrutamiento y NAT básico en ufw-router..."
    ssh -i "${host_key}" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes root@192.168.10.1 "
        ufw disable || true
        sysctl -w net.ipv4.ip_forward=1
        echo 'net.ipv4.ip_forward=1' >> /etc/sysctl.conf
        WAN_IF=\$(ip route show default 2>/dev/null | awk '{print \$5; exit}' || echo 'ens5')
        iptables -t nat -F POSTROUTING || true
        iptables -t nat -A POSTROUTING -s 192.168.10.0/24 -o \${WAN_IF} -j MASQUERADE
        iptables -t nat -A POSTROUTING -s 192.168.20.0/24 -o \${WAN_IF} -j MASQUERADE
    " || warn "No se pudo habilitar enrutamiento temporal. Los nodos podrían no disponer de red externa."
}

# ==============================================================================
# 9. FLUJO PRINCIPAL DE EJECUCIÓN (MAIN)
# ==============================================================================
DEPLOY_START=$(date +%s)

echo -e "${GREEN}=========================================================${NC}"
echo -e "${GREEN}  Despliegue de Infraestructura CMS (Fake Enterprise)    ${NC}"
echo -e "${GREEN}=========================================================${NC}"

if [[ "$DRY_RUN" -eq 1 ]]; then
    warn "MODO SIMULACIÓN (DRY-RUN): Los comandos no se ejecutarán realmente"
fi

# Validar estado del sistema antes de comenzar
preflight_checks

# --- VM PROVISIONING PHASE (PHASE 00) ---
if [[ "$SKIP_VM_CREATE" -eq 1 ]]; then
    info "SKIP_VM_CREATE=1 activo → Levantando VMs existentes sin reinstalar sistema operativo"
    timed_phase "Phase 00: Booting existing VMs" "start_all_vms.sh"
    if [[ "$DRY_RUN" -ne 1 ]]; then
        wait_for_ssh "$IP_JUMPSTART_INT" "$SSH_TIMEOUT"
    fi
else
    INIT_EXTRA=()
    [[ "$RECREATE_VMS" -eq 1 ]] && INIT_EXTRA+=(--recreate)

    # --- Phase 00a: Network Creation and Jumpstart Node Installation ---
    timed_phase "Phase 00a: Network Creation and Jumpstart Node Installation" "00_init_vms.sh" --jumpstart-only "${INIT_EXTRA[@]}"

    # --- Esperar a que el servidor Jumpstart esté disponible ---
    if [[ "$DRY_RUN" -ne 1 ]]; then
        wait_for_ssh "$IP_JUMPSTART_INT" "$SSH_TIMEOUT"
    fi

    # --- Phase 01: Cobbler Provisioning Server (Baremetal) ---
    timed_phase "Phase 01: Cobbler (PXE/DHCP/DNS Services)" "00_setup_cobbler.sh"
    timed_phase "Phase 01.5: Registering Client Nodes in Cobbler" "add_cobbler_nodes.sh"

    # --- Phase 00b: Client Nodes Creation (Unattended PXE Installation) ---
    timed_phase "Phase 00b: Client Nodes Creation (Unattended PXE Installation)" "00_init_vms.sh" --nodes-only "${INIT_EXTRA[@]}"

    # --- Espera de conectividad general ---
    if [[ "$DRY_RUN" -ne 1 ]]; then
        wait_for_all_nodes
        bootstrap_router
    fi
fi

if [[ "$SKIP_VM_CREATE" -eq 1 ]] && [[ "$DRY_RUN" -ne 1 ]]; then
    wait_for_all_nodes
    bootstrap_router
fi

# --- SSH key and CA certificate repair if applicable ---
if [[ -f "$SCRIPTS_DIR/08_repair_ssh_puppet.sh" ]]; then
    timed_phase "Phase 01.8: SSH and Puppet CA Repair" "08_repair_ssh_puppet.sh"
fi

# ==============================================================================
# LOGICAL DEPENDENCY PHASE ORDERING
# ==============================================================================

# --- Phase 02: Puppet (Configuration Management) ---
# Install agents and link certificates against the central Puppet Master
timed_phase "Phase 02: Puppet (Configuration Management)" "01_setup_puppet.sh"

# --- Phase 03: DRBD (HA Block Storage) ---
# Must be configured BEFORE Kubernetes and MariaDB so the data directory (/mnt/data/mariadb)
# is already mounted on the replicated device. This way, MariaDB initializes its database directly
# on high-availability storage, avoiding K3s downtime and backup/restore operations.
timed_phase "Phase 03: DRBD (High Availability Storage)" "06_setup_drbd.sh"

# --- Phase 04: Kubernetes (HA Clustering & Database) ---
# Configura K3s e inicializa el clúster. Levanta MariaDB en StatefulSet usando el PV del volumen DRBD.
timed_phase "Phase 04: Kubernetes (HA Clustering and Database)" "03_setup_kubernetes.sh"

# --- Phase 05: Nginx and WordPress (Load Balancer and Frontends) ---
# Levanta el balanceador de carga Nginx e instala Apache/WordPress. Se ejecuta tras Kubernetes,
# asegurando que los frontales de WordPress puedan conectar y autenticar contra MariaDB ya operativa.
timed_phase "Phase 05: Nginx (Load Balancing) and WordPress (CMS)" "02_setup_nginx.sh"

# --- Phase 06: Prometheus + Grafana (Comprehensive Monitoring) ---
# Configura el scraping de métricas en Prometheus e dashboards de Grafana para todos los servicios
# ya levantados en las fases anteriores (LB, CMS, MariaDB, nodos).
timed_phase "Phase 06: Prometheus + Grafana (Monitoring)" "04_setup_monitoring.sh"

# --- Phase 07: UFW (Security and Nodal Firewalling) ---
# Se ejecuta al final de todo el aprovisionamiento. Aplica las reglas del cortafuegos nodal e interno,
# cerrando puertos no utilizados una vez que el flujo de conexiones del clúster está totalmente asentado.
timed_phase "Phase 07: UFW (Security and Nodal Firewalling)" "05_setup_ufw.sh"

# --- Phase 08: Internal CA (PKI with step-ca) ---
# Deploys a private Certificate Authority on the Jumpstart node, issues TLS certificates
# for Nginx LB, Grafana, and K3s API servers, and distributes the root CA to all nodes.
if [[ -f "$SCRIPTS_DIR/09_setup_internal_ca.sh" ]]; then
    timed_phase "Phase 08: Internal CA (TLS Certificate Authority)" "09_setup_internal_ca.sh"
fi

# ==============================================================================
# 10. DEPLOYMENT SUMMARY
# ==============================================================================
DEPLOY_END=$(date +%s)
DEPLOY_DURATION=$((DEPLOY_END - DEPLOY_START))
DEPLOY_MINS=$((DEPLOY_DURATION / 60))
DEPLOY_SECS=$((DEPLOY_DURATION % 60))

echo -e "${GREEN}=========================================================${NC}"
echo -e "${GREEN}  Deployment completed successfully.                     ${NC}"
echo -e "${GREEN}  Total elapsed time: ${DEPLOY_MINS}m ${DEPLOY_SECS}s${NC}"
echo -e "${GREEN}=========================================================${NC}"