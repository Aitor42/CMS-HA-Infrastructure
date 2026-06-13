#!/bin/bash
# 00_init_vms.sh
#
# Creates all virtual networks and VMs for the CMS infrastructure.
# Provisions the Jumpstart node via cloud-init autoinstall, then provisions
# all client nodes (router, masters, workers, storage, LB, CMS frontends, hotdesks)
# via PXE boot using the Jumpstart Cobbler server.
#
# NOTA PARA EL SERVIDOR GAR:
#   - El disco raíz (/) suele estar lleno, por lo que las VMs se crean en /home ($VM_DIR).
#   - Al ejecutarse sin sudo, se intenta conectar a qemu:///system (mediante sudo sin contraseña)
#     o se cae en qemu:///session (donde no se soportan interfaces tipo bridge).
#   - Si /dev/kvm no tiene permisos, se intenta corregir o se cae en emulación QEMU.
#   - Se redirige TMPDIR a /home/$USER/tmp para evitar fallos de espacio en /tmp.

set -euo pipefail

# Load global configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATES_DIR="${SCRIPT_DIR}/../templates"
source "${SCRIPT_DIR}/config.sh"

# ==============================================================================
# 1. PARÁMETROS DE RECURSOS PARA VIRTUALIZACIÓN (MEMORIA Y vCPUS)
# ==============================================================================
# Validar límite de puestos hot-desk admisibles
if [[ $NUM_HOTDESKS -gt 8 ]]; then
    echo 'ERROR: El número máximo de puestos hot-desk soportado es 8.'
    exit 1
fi

# NOTA: Durante la fase de instalación por red (PXE), el instalador de Ubuntu (Subiquity)
# requiere un mínimo de 3-4 GB de RAM para no fallar por falta de memoria (Squashfs OOM).
# Una vez finalizado el despliegue, la memoria de las VMs se reduce a su tamaño objetivo
# de producción ejecutando el script 'shrink_vm_ram.sh'.
RAM_JUMPSTART=3072
VCPU_JUMPSTART=2
RAM_ROUTER=3072
VCPU_ROUTER=1
RAM_MONITOR=3072
VCPU_MONITOR=1
RAM_MASTER=4096
VCPU_MASTER=1
RAM_WORKER=4096
VCPU_WORKER=1
RAM_STORAGE=3072
VCPU_STORAGE=1
RAM_LB=3072
VCPU_LB=1
RAM_CMS=3072
VCPU_CMS=1
RAM_HOTDESK=3072
VCPU_HOTDESK=1

# Configuración de directorio temporal seguro
export TMPDIR="${TMPDIR:-$HOME/tmp}"
mkdir -p "$TMPDIR"

# ==============================================================================
# 2. SELECCIÓN DE URI DE CONEXIÓN A LIBVIRT
# ==============================================================================
# qemu:///system (con privilegios) -> permite crear bridges y segmentación real.
# qemu:///session (usuario sin privilegios) -> limitado a red privada de usuario.
if [ -n "${VIRSH_URI:-}" ]; then
    # URI explícita definida por el usuario
    true
elif virsh -c qemu:///system version &>/dev/null; then
    VIRSH_URI="qemu:///system"
elif sudo -n virsh -c qemu:///system version &>/dev/null; then
    VIRSH_URI="qemu:///system"
else
    VIRSH_URI="qemu:///session"
    echo "[WARN] Usando qemu:///session: las interfaces en modo bridge de red podrían fallar."
fi
export LIBVIRT_DEFAULT_URI="$VIRSH_URI"

# Determinar si requerimos anteponer 'sudo' para ejecutar comandos de virsh/virt-install
SUDO_CMD=""
if [[ "$VIRSH_URI" == "qemu:///system" ]] && [[ "$EUID" -ne 0 ]]; then
    if ! virsh -c "$VIRSH_URI" version &>/dev/null; then
        SUDO_CMD="sudo"
    fi
fi

# ==============================================================================
# 3. DETECCIÓN Y HABILITACIÓN DE SOPORTE ACELERACIÓN HARDWARE (KVM)
# ==============================================================================
if [ -e /dev/kvm ] && [ -w /dev/kvm ]; then
    VIRT_TYPE="kvm"
    echo "[+] Soporte KVM disponible: virtualización nativa activa"
else
    echo "[*] /dev/kvm no disponible. Intentando cargar módulo del kernel..."
    
    # Intentar forzar carga del driver de Intel
    $SUDO_CMD modprobe kvm_intel >/dev/null 2>&1 || true

    # Si el dispositivo se creó pero no tiene permisos para el usuario
    if [ -e /dev/kvm ]; then
        $SUDO_CMD chown root:kvm /dev/kvm >/dev/null 2>&1 || true
        $SUDO_CMD chmod 666 /dev/kvm >/dev/null 2>&1 || true
    fi

    # Verificar si logramos habilitar el acceso a KVM
    if [ -e /dev/kvm ] && [ -w /dev/kvm ]; then
        VIRT_TYPE="kvm"
        echo "[+] KVM habilitado correctamente: rendimiento nativo activo"
    else
        VIRT_TYPE="qemu"
        echo "[WARN] KVM inaccesible. Se usará emulación por software (desempeño significativamente más lento)."
    fi
fi

# ==============================================================================
# 4. COMPROBACIÓN DE COMANDOS DEL SISTEMA
# ==============================================================================
if ! command -v virsh &>/dev/null; then
    echo "[ERROR] 'virsh' no encontrado. Instale: apt install libvirt-daemon-system"
    exit 1
fi

if ! command -v virt-install &>/dev/null; then
    echo "[ERROR] 'virt-install' no encontrado. Instale: apt install virtinst"
    exit 1
fi

if ! command -v xorrisofs &>/dev/null; then
    echo "[ERROR] 'xorrisofs' no encontrado. Instale: apt install xorriso"
    exit 1
fi

if ! $SUDO_CMD virsh -c "$VIRSH_URI" version &>/dev/null; then
    echo "[ERROR] No se pudo conectar a la API del hipervisor ($VIRSH_URI)."
    echo "        Asegúrese de que el daemon libvirtd está activo."
    exit 1
fi
echo "[+] Conexión activa con hipervisor: $VIRSH_URI (Sudo: ${SUDO_CMD:-ninguno})"

# ==============================================================================
# 5. FUNCIONES PARA ELIMINACIÓN DE RECURSOS PREVIOS
# ==============================================================================
cleanup_vm() {
    local nombre="$1"
    if $SUDO_CMD virsh dominfo "$nombre" &>/dev/null; then
        echo "  [-] Eliminando máquina virtual previa: $nombre"
        $SUDO_CMD virsh destroy "$nombre" 2>/dev/null || true
        # No usamos --remove-all-storage para evitar que elimine las imágenes ISO montadas.
        # En su lugar, borramos los discos virtuales asociados manualmente más adelante.
        $SUDO_CMD virsh undefine "$nombre" 2>/dev/null || true
    fi
    # Borrar ficheros de disco asociados a la VM
    rm -f "$VM_DIR/${nombre}.qcow2"
    rm -f "$VM_DIR/${nombre}-drbd.qcow2"
}

cleanup_net() {
    local nombre="$1"
    if $SUDO_CMD virsh net-info "$nombre" &>/dev/null; then
        echo "  [-] Eliminando red virtual existente: $nombre"
        $SUDO_CMD virsh net-destroy "$nombre" 2>/dev/null || true
        $SUDO_CMD virsh net-undefine "$nombre" 2>/dev/null || true
    fi
}

# ==============================================================================
# 6. PARSEO DE ARGUMENTOS DE EJECUCIÓN
# ==============================================================================
ALL_VMS=(
    ufw-router jumpstart
    internal-monitor internal-master1 internal-master2
    internal-worker1 internal-worker2 internal-storage
    main-lb main-cms1 main-cms2
)
for i in $(seq 1 "$NUM_HOTDESKS"); do
    ALL_VMS+=("main-hotdesk$i")
done

JUMPSTART_ONLY=0
NODES_ONLY=0
CLEANUP=0
RECREATE=0

for arg in "$@"; do
    case "$arg" in
        --cleanup) CLEANUP=1 ;;
        --recreate) RECREATE=1 ;;
        --jumpstart-only) JUMPSTART_ONLY=1 ;;
        --nodes-only) NODES_ONLY=1 ;;
    esac
done

# Por defecto, si no se definen filtros, se despliega toda la infraestructura
if [[ $JUMPSTART_ONLY -eq 0 ]] && [[ $NODES_ONLY -eq 0 ]] && [[ $CLEANUP -eq 0 ]] && [[ $RECREATE -eq 0 ]]; then
    JUMPSTART_ONLY=1
    NODES_ONLY=1
fi

# Modo de limpieza únicamente
if [[ $CLEANUP -eq 1 ]]; then
    echo ">>> Destruyendo y limpiando todas las VMs y Redes del proyecto <<<"
    for vm in "${ALL_VMS[@]}"; do
        cleanup_vm "$vm"
    done
    cleanup_net "internal"
    cleanup_net "main"
    echo ">>> Limpieza completada <<<"
    exit 0
fi

# Modo de recreación (eliminar previo antes de instalar)
if [[ $RECREATE -eq 1 ]]; then
    echo "[*] Limpiando recursos previos para recreación..."
    for vm in "${ALL_VMS[@]}"; do
        cleanup_vm "$vm"
    done
    cleanup_net "internal"
    cleanup_net "main"
fi

# ==============================================================================
# 7. INICIO DEL DESPLIEGUE DE INFRAESTRUCTURA BASE
# ==============================================================================
echo ">>> Iniciando creación de redes y máquinas virtuales <<<"
mkdir -p "$VM_DIR"

FREE_GB=$(df --output=avail -BG "$VM_DIR" 2>/dev/null | tail -1 | tr -d ' G')
echo "[+] Ruta de almacenamiento de discos: $VM_DIR (${FREE_GB} GB disponibles)"
echo "[+] Cantidad de puestos hot-desk: $NUM_HOTDESKS"
if [[ "$FREE_GB" -lt 30 ]]; then
    echo "[WARN] Espacio libre ajustado: ${FREE_GB} GB (Se recomiendan >=30 GB para evitar cuellos de botella)"
fi

# ==============================================================================
# 8. FUNCIÓN AUXILIAR PARA CREAR E INSTALAR UNA MÁQUINA VIRTUAL
# ==============================================================================
crear_vm() {
    local nombre="$1"
    local ram="$2"
    local vcpus="$3"
    local disco="$4"
    local mac="$5"
    local red="$6"
    shift 6
    local extra_args=("$@")

    echo "[+] Creando máquina virtual: $nombre (RAM: $ram MB, CPUs: $vcpus, Disco: $disco GB, Red: $red)..."
    cleanup_vm "$nombre"

    # La VM arranca configurada para bootear prioritariamente por PXE (red)
    # y como segundo dispositivo el disco duro principal.
    $SUDO_CMD virt-install \
        --connect "$VIRSH_URI" \
        --virt-type "$VIRT_TYPE" \
        --name="$nombre" \
        --ram="$ram" \
        --vcpus="$vcpus" \
        --disk "path=$VM_DIR/$nombre.qcow2,size=$disco,format=qcow2,bus=virtio" \
        --network "network=$red,mac=$mac,model=virtio" \
        --pxe \
        --boot hd,network \
        --os-variant=ubuntu24.04 \
        --noautoconsole \
        --wait=0 \
        "${extra_args[@]}"

    echo "  [OK] VM $nombre inicializada y registrada en libvirt"
}

# ==============================================================================
# 9. FASE 00A: DESPLIEGUE DEL SERVIDOR JUMPSTART
# ==============================================================================
if [[ $JUMPSTART_ONLY -eq 1 ]]; then
    # Asegurar que se dispone de una clave SSH para las VMs
    if [ ! -f "${HOST_KEY_FILE}" ]; then
        echo "[+] Generando clave privada SSH del host en ${HOST_KEY_FILE}..."
        ssh-keygen -t ed25519 -N "" -f "${HOST_KEY_FILE}"
    fi
    HOST_PUBKEY="$(cat "${HOST_KEY_FILE}.pub")"
    export HOST_KEY_FILE HOST_PUBKEY

    # ── Configure 'internal' Virtual Network (Provisioning, DB, Management) ──
    echo "[+] Defining virtual network 'internal'..."
    cp "${TEMPLATES_DIR}/libvirt/internal-net.xml" "$VM_DIR/internal-net.xml"

    if ! $SUDO_CMD virsh net-info internal &>/dev/null; then
        $SUDO_CMD virsh net-define "$VM_DIR/internal-net.xml"
        $SUDO_CMD virsh net-start internal
        $SUDO_CMD virsh net-autostart internal
        echo "  [OK] Red 'internal' creada y levantada"
    else
        echo "  [OK] Red 'internal' ya existía"
    fi

    # Asignar la IP IP_ROUTER_INT (.254 en el bridge del host) para permitir routing local
    if ! ip addr show dev virbr-int 2>/dev/null | grep -q "192.168.10.254/24"; then
        echo "[+] Asignando IP 192.168.10.254/24 al puente virbr-int del host..."
        if ip addr show dev virbr-int 2>/dev/null | grep -q "192.168.10.1/24"; then
            $SUDO_CMD ip addr del 192.168.10.1/24 dev virbr-int || true
        fi
        $SUDO_CMD ip addr add 192.168.10.254/24 dev virbr-int
        $SUDO_CMD ip link set dev virbr-int up
    else
        echo "  [OK] El puente virbr-int ya dispone de la IP 192.168.10.254/24"
    fi

    # ── Configure 'main' Virtual Network (Load Balancing and Client nodes) ──
    echo "[+] Defining virtual network 'main'..."
    cp "${TEMPLATES_DIR}/libvirt/main-net.xml" "$VM_DIR/main-net.xml"

    if ! $SUDO_CMD virsh net-info main &>/dev/null; then
        $SUDO_CMD virsh net-define "$VM_DIR/main-net.xml"
        $SUDO_CMD virsh net-start main
        $SUDO_CMD virsh net-autostart main
        echo "  [OK] Red 'main' creada y levantada"
    else
        echo "  [OK] Red 'main' ya existía"
    fi

    # Asignar IP .254 en el bridge virbr-main del host para posibilitar accesos locales
    if ! ip addr show dev virbr-main 2>/dev/null | grep -q "192.168.20.254/24"; then
        echo "[+] Asignando IP 192.168.20.254/24 al puente virbr-main del host..."
        if ip addr show dev virbr-main 2>/dev/null | grep -q "192.168.20.1/24"; then
            $SUDO_CMD ip addr del 192.168.20.1/24 dev virbr-main || true
        fi
        $SUDO_CMD ip addr add 192.168.20.254/24 dev virbr-main
        $SUDO_CMD ip link set dev virbr-main up
    else
        echo "  [OK] El puente virbr-main ya dispone de la IP 192.168.20.254/24"
    fi

    # ── Creación del Nodo Jumpstart (Instalación Desatendida Autoinstall) ──
    cleanup_vm "jumpstart"
    AUTOINSTALL_DIR="$VM_DIR/autoinstall"
    mkdir -p "$AUTOINSTALL_DIR"

    # Configurar interfaces de red del Jumpstart
    JUMPSTART_NET_ARGS=(
        --network network=internal,mac=52:54:00:10:00:01,model=virtio
        --network network=main,mac=52:54:00:10:02:0a,model=virtio
    )
    WAN_IFACE_CONFIG=""

    # Detectar si hay internet en el host mediante la red default de libvirt
    if ! $SUDO_CMD virsh net-info default &>/dev/null 2>&1; then
        echo "  [WARN] Red 'default' no disponible en libvirt. Jumpstart no dispondrá de WAN."
    else
        echo "  [+] Red 'default' (WAN) disponible. Vinculando interfaz ens5 (MAC 52:54:00:10:00:09) para acceso a internet."
        JUMPSTART_NET_ARGS+=(--network network=default,mac=52:54:00:10:00:09,model=virtio)
        
        # Obtener DNS del host para resolver nombres dentro de la VM
        HOST_DNS_SERVERS=$(resolvectl status 2>/dev/null \
            | grep 'DNS Servers:' \
            | awk '{for(i=3;i<=NF;i++) print $i}' \
            | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$') || true
        if [ -z "$HOST_DNS_SERVERS" ]; then
            HOST_DNS_SERVERS=$(grep -E '^nameserver' /etc/resolv.conf \
                | awk '{print $2}' | grep -v '127\.0\.0\.') || true
        fi
        [ -z "$HOST_DNS_SERVERS" ] && HOST_DNS_SERVERS="172.20.32.3 172.20.32.4"
        
        DNS_LIST=""
        for dns in ${HOST_DNS_SERVERS}; do
            DNS_LIST="${DNS_LIST}            - ${dns}
"
        done

        WAN_IFACE_CONFIG="      net2:
        match:
          macaddress: \"52:54:00:10:00:09\"
        dhcp4: true
        dhcp4-overrides:
          route-metric: 100
          use-dns: false
        nameservers:
          addresses:
${DNS_LIST}"
    fi

    # Generar un hash cifrado robusto para bloquear el login de usuario/contraseña
    RANDOM_PASS_HASH=$(python3 -c "
import crypt, os
salt = crypt.mksalt(crypt.METHOD_SHA512)
rand_pass = os.urandom(32).hex()
print(crypt.crypt(rand_pass, salt))
" 2>/dev/null) || RANDOM_PASS_HASH=$(openssl passwd -6 "$(head -c 16 /dev/urandom | base64 | head -c 24)" 2>/dev/null) || RANDOM_PASS_HASH='$6$GAR_RANDOM$zQ8.e3KlMUuKr2VVoNm4c5UwkHb9XsGdpT1qFaEhWy6LjnrCDivPt0OIZmBx7QgAsk'

    # Escribir la configuración de Cloud-Init para la instalación automática (Autoinstall)
    cat <<EOF_AUTOINSTALL > "$AUTOINSTALL_DIR/user-data"
#cloud-config
autoinstall:
  version: 1
  shutdown: poweroff
  locale: es_ES.UTF-8
  keyboard:
    layout: es
  identity:
    hostname: jumpstart
    username: admin
    password: "${RANDOM_PASS_HASH}"
  ssh:
    install-server: true
    allow-pw: false
    authorized-keys:
      - ${HOST_PUBKEY}
  network:
    version: 2
    ethernets:
      net0:
        match:
          macaddress: "52:54:00:10:00:01"
        addresses:
          - 192.168.10.10/24
        nameservers:
          addresses:
            - 192.168.10.10
        routes:
          - to: default
            via: 192.168.10.1
            metric: 200
      net1:
        match:
          macaddress: "52:54:00:10:02:0a"
        addresses:
          - 192.168.20.10/24
${WAN_IFACE_CONFIG}
  storage:
    layout:
      name: direct
  late-commands:
    # Permitir sudo al usuario admin sin contraseña
    - echo 'admin ALL=(ALL) NOPASSWD:ALL' > /target/etc/sudoers.d/admin
    - chmod 440 /target/etc/sudoers.d/admin
    # Bloquear las contraseñas de las cuentas admin y root para forzar SSH key obligatoria
    - curtin in-target --target=/target -- passwd -l admin
    - curtin in-target --target=/target -- passwd -l root
    # Copiar clave autorizada a root y admin
    - mkdir -p /target/root/.ssh /target/home/admin/.ssh
    - echo "${HOST_PUBKEY}" >> /target/root/.ssh/authorized_keys
    - echo "${HOST_PUBKEY}" >> /target/home/admin/.ssh/authorized_keys
    - chmod 700 /target/root/.ssh /target/home/admin/.ssh
    - chmod 600 /target/root/.ssh/authorized_keys /target/home/admin/.ssh/authorized_keys
    - curtin in-target --target=/target -- chown -R admin:admin /home/admin/.ssh
    # Endurecer SSHD: prohibir login interactivo por contraseña
    - sed -i 's/#\?PermitRootLogin.*/PermitRootLogin prohibit-password/' /target/etc/ssh/sshd_config
    - sed -i 's/#\?PasswordAuthentication.*/PasswordAuthentication no/' /target/etc/ssh/sshd_config
    - curtin in-target --target=/target -- systemctl enable ssh
EOF_AUTOINSTALL

    touch "$AUTOINSTALL_DIR/meta-data"

    echo "[+] Creando seed.iso de metadatos Nocloud..."
    xorrisofs -output "$VM_DIR/seed.iso" -volid cidata -joliet -rock "$AUTOINSTALL_DIR/"

    # Crear disco de almacenamiento principal
    rm -f "$VM_DIR/jumpstart.qcow2"
    qemu-img create -f qcow2 "$VM_DIR/jumpstart.qcow2" 30G

    # Buscar la imagen ISO del instalador oficial
    ISO_PATH=""
    for candidate in "$VM_DIR/ubuntu-24.04.4-live-server-amd64.iso" "$VM_DIR/ubuntu-24.04-live-server-amd64.iso" "$VM_DIR/ubuntu-24.04.2-live-server-amd64.iso"; do
        if [ -f "$candidate" ] && [ -s "$candidate" ]; then
            ISO_PATH="$candidate"
            break
        fi
    done
    if [ -z "$ISO_PATH" ]; then
        candidate=$(find "$VM_DIR" -name "*ubuntu-24.04*.iso" -size +1G | head -n 1 2>/dev/null) || true
        if [ -n "$candidate" ]; then
            ISO_PATH="$candidate"
        fi
    fi
    if [ -z "$ISO_PATH" ]; then
        echo "[ERROR] No se localizó ninguna ISO de Ubuntu 24.04 en $VM_DIR"
        exit 1
    fi
    echo "[+] ISO seleccionada para despliegue: $ISO_PATH"

    echo "[+] Iniciando instalación automatizada de la VM Jumpstart (Espere a que se apague)..."
    $SUDO_CMD virt-install \
        --connect "$VIRSH_URI" \
        --virt-type "$VIRT_TYPE" \
        --name=jumpstart \
        --ram="$RAM_JUMPSTART" \
        --vcpus="$VCPU_JUMPSTART" \
        --disk "path=$VM_DIR/jumpstart.qcow2,format=qcow2,bus=virtio" \
        --disk "path=${ISO_PATH},device=cdrom,bus=sata" \
        --disk "path=$VM_DIR/seed.iso,device=cdrom,bus=sata" \
        --location "${ISO_PATH},kernel=casper/vmlinuz,initrd=casper/initrd" \
        --extra-args "autoinstall ds=nocloud console=ttyS0,115200n8" \
        "${JUMPSTART_NET_ARGS[@]}" \
        --os-variant=ubuntu24.04 \
        --graphics none \
        --wait=-1 \
        --noreboot

    echo "[+] Instalación del Jumpstart completada satisfactoriamente"
    echo "[+] Iniciando VM Jumpstart..."
    $SUDO_CMD virsh start jumpstart || true
fi

# ==============================================================================
# 10. FASE 00B: DESPLIEGUE DE NODOS CLIENTES
# ==============================================================================
if [[ $NODES_ONLY -eq 1 ]]; then
    # ── Crear Router/Firewall (UFW-Router) ──
    echo "[+] Creando máquina virtual para Router/Firewall (ufw-router)..."
    cleanup_vm "ufw-router"
    ROUTER_WAN_NET="default"
    if ! $SUDO_CMD virsh net-info default &>/dev/null 2>&1; then
        warn "Red default ausente en libvirt. ufw-router utilizará 'main' como puente WAN alternativo."
        ROUTER_WAN_NET="main"
    fi
    $SUDO_CMD virt-install \
        --connect "$VIRSH_URI" \
        --virt-type "$VIRT_TYPE" \
        --name=ufw-router \
        --ram="$RAM_ROUTER" \
        --vcpus="$VCPU_ROUTER" \
        --disk "path=$VM_DIR/ufw-router.qcow2,size=5,format=qcow2,bus=virtio" \
        --network network=internal,mac=52:54:00:10:01:02,model=virtio \
        --network network=main,mac=52:54:00:10:02:02,model=virtio \
        --network "network=$ROUTER_WAN_NET,mac=52:54:00:10:00:02,model=virtio" \
        --pxe \
        --boot hd,network \
        --os-variant=ubuntu24.04 \
        --noautoconsole \
        --wait=0

    echo "  [OK] VM ufw-router registrada"

    # ── Crear Nodos de la Red Interna (Internal) ──
    echo "[+] Creando nodos clientes de la red interna (internal)..."

    # internal-monitor (Prometheus + Grafana)
    crear_vm "internal-monitor" "$RAM_MONITOR" "$VCPU_MONITOR" 4 "52:54:00:10:01:10" "internal"

    # Nodos maestros de Kubernetes (Requieren un segundo disco de 3GB para replicación DRBD)
    # internal-master1 (K3s Server + DRBD Primary)
    crear_vm "internal-master1" "$RAM_MASTER" "$VCPU_MASTER" 8 "52:54:00:10:01:11" "internal" \
        --disk "path=$VM_DIR/internal-master1-drbd.qcow2,size=3,format=qcow2,bus=virtio"

    # internal-master2 (K3s Server + DRBD Secondary)
    crear_vm "internal-master2" "$RAM_MASTER" "$VCPU_MASTER" 8 "52:54:00:10:01:12" "internal" \
        --disk "path=$VM_DIR/internal-master2-drbd.qcow2,size=3,format=qcow2,bus=virtio"

    # Nodos de computación Kubernetes (Workers)
    # internal-worker1 (K3s Agent)
    crear_vm "internal-worker1" "$RAM_WORKER" "$VCPU_WORKER" 8 "52:54:00:10:01:13" "internal"
    # internal-worker2 (K3s Agent)
    crear_vm "internal-worker2" "$RAM_WORKER" "$VCPU_WORKER" 8 "52:54:00:10:01:14" "internal"

    # internal-storage (Almacenamiento compartido NFS/backups)
    crear_vm "internal-storage" "$RAM_STORAGE" "$VCPU_STORAGE" 8 "52:54:00:10:01:15" "internal"

    # ── Crear Nodos de la Red Pública/Cliente (Main) ──
    echo "[+] Creando nodos clientes de la red exterior (main)..."

    # main-lb (Nginx Load Balancer)
    crear_vm "main-lb" "$RAM_LB" "$VCPU_LB" 4 "52:54:00:10:02:64" "main"

    # Servidores de Frontal CMS (Apache + PHP + WordPress)
    # main-cms1
    crear_vm "main-cms1" "$RAM_CMS" "$VCPU_CMS" 4 "52:54:00:10:02:65" "main"
    # main-cms2
    crear_vm "main-cms2" "$RAM_CMS" "$VCPU_CMS" 4 "52:54:00:10:02:66" "main"

    # Puestos de Trabajo Dinámicos (Hotdesks)
    # Se calculan las MACs a partir del octeto de IP secuencial (0xC9 = 201 en decimal)
    echo "[+] Creando $NUM_HOTDESKS puestos de trabajo hot-desk..."
    for i in $(seq 1 "$NUM_HOTDESKS"); do
        ip_last_octet=$((200 + i))
        hex_id=$(printf '%02x' "$ip_last_octet")
        crear_vm "main-hotdesk$i" "$RAM_HOTDESK" "$VCPU_HOTDESK" 3 "52:54:00:10:02:$hex_id" "main"
    done

    # ── Resumen de Recursos de Virtualización ──
    TOTAL_VMS=$($SUDO_CMD virsh list --all --name 2>/dev/null | grep -c . || echo "?")
    USED_GB=$(du -sh "$VM_DIR" 2>/dev/null | awk '{print $1}' || echo "?")
    echo ""
    echo ">>> Despliegue de Redes y Máquinas Virtuales completado <<<"
    echo "    Total VMs registradas:  $TOTAL_VMS"
    echo "    Espacio usado por discos: $USED_GB en $VM_DIR"
    echo "    Espacio libre restante:   $(df --output=avail -BG "$VM_DIR" 2>/dev/null | tail -1 | tr -d ' G') GB"
fi
