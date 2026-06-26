#!/bin/bash
# 01_setup_cobbler.sh
#
#
# Descripción:
#   Instala y configura Cobbler en el nodo Jumpstart (192.168.10.10) para realizar
#   el aprovisionamiento automático y desatendido de Ubuntu 24.04 LTS (Noble Numbat)
#   en todos los nodos clientes mediante arranque por red PXE.
#
# Ajustes clave del entorno:
#   - Autenticación segura mediante SSH key (sin contraseñas tradicionales).
#   - Repositorio oficial OpenSUSE Build Service (OBS) para Cobbler v3.3.
#   - Soporte de fallback automático del repositorio (24.04 Noble a 22.04 Jammy).
#   - Habilitación del repositorio 'universe' necesario para dependencias de Python.
#   - Preservación de salida de red WAN en Jumpstart durante el aprovisionamiento.
#   - Publicación de la lista de claves autorizadas (host + jumpstart) en el pub de Cobbler.

set -euo pipefail

# Cargar configuraciones centrales del proyecto
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATES_DIR="${SCRIPT_DIR}/../templates"
source "${SCRIPT_DIR}/config.sh"

echo ">>> Iniciando despliegue de Cobbler (Baremetal) <<<"

# Buscar una imagen ISO válida de Ubuntu 24.04 Server en el host
ISO_SOURCE=""
for candidate in "$VM_DIR/ubuntu-24.04.4-live-server-amd64.iso" "$VM_DIR/ubuntu-24.04-live-server-amd64.iso" "$VM_DIR/ubuntu-24.04.2-live-server-amd64.iso"; do
    if [ -f "$candidate" ] && [ -s "$candidate" ]; then
        ISO_SOURCE="$candidate"
        break
    fi
done
if [ -z "$ISO_SOURCE" ]; then
    candidate=$(find "$VM_DIR" -name "*ubuntu-24.04*.iso" -size +1G | head -n 1 2>/dev/null) || true
    if [ -n "$candidate" ]; then
        ISO_SOURCE="$candidate"
    fi
fi
if [ -z "$ISO_SOURCE" ]; then
    echo "[ERROR] No se localizó ninguna ISO válida de Ubuntu 24.04 Noble en $VM_DIR"
    exit 1
fi
echo "[+] ISO origen para Cobbler: $ISO_SOURCE"

# Asegurar disponibilidad de claves SSH del hipervisor
if [ ! -f "${HOST_KEY_FILE}" ]; then
    echo "[+] Generando clave SSH del host en ${HOST_KEY_FILE}..."
    ssh-keygen -t ed25519 -N "" -f "${HOST_KEY_FILE}"
fi
HOST_PUBKEY="$(cat "${HOST_KEY_FILE}.pub")"

# ─── 1. VERIFICAR ACCESO SSH POR CLAVE AL JUMPSTART ───────────────────────────
echo "[+] Verificando acceso SSH con admin@${JUMPSTART_IP}..."
if ! ssh -i "${HOST_KEY_FILE}" ${SSH_OPTS} "admin@${JUMPSTART_IP}" true 2>/dev/null; then
    echo "[ERROR] Acceso SSH sin clave no disponible para admin@${JUMPSTART_IP}."
    echo "        Asegúrese de haber ejecutado 00_init_vms.sh previamente."
    exit 1
fi
echo "  [OK] Acceso SSH verificado para admin@${JUMPSTART_IP}"

# ─── 2. AUTORIZAR ACCESO SSH DE ROOT EN JUMPSTART ─────────────────────────────
echo "[+] Propagando claves SSH al usuario root de Jumpstart..."
ssh -i "${HOST_KEY_FILE}" ${SSH_OPTS} "admin@${JUMPSTART_IP}" 'bash -s' << 'ELEVATE_EOF'
set -e
sudo mkdir -p /root/.ssh
sudo cp /home/admin/.ssh/authorized_keys /root/.ssh/authorized_keys
sudo chown -R root:root /root/.ssh
sudo chmod 700 /root/.ssh
sudo chmod 600 /root/.ssh/authorized_keys
ELEVATE_EOF

if ! ssh -i "${HOST_KEY_FILE}" ${SSH_OPTS} "root@${JUMPSTART_IP}" true 2>/dev/null; then
    echo "[ERROR] Falló la elevación o el acceso directo por SSH como root al Jumpstart."
    exit 1
fi
echo "  [OK] Acceso SSH verificado para root@${JUMPSTART_IP}"

# ─── 3. TRANSFERIR LA IMAGEN ISO AL JUMPSTART ─────────────────────────────────
echo "[+] Transfiriendo ISO de Ubuntu al disco virtual de Jumpstart..."
ssh -i "${HOST_KEY_FILE}" ${SSH_OPTS} "root@${JUMPSTART_IP}" "mkdir -p /var/lib/cobbler/isos"
scp -i "${HOST_KEY_FILE}" \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    "${ISO_SOURCE}" \
    "root@${JUMPSTART_IP}:/var/lib/cobbler/isos/ubuntu-24.04-live-server-amd64.iso"
echo "  [OK] Transferencia de ISO finalizada"

# ─── 4. DETECTAR SERVIDORES DNS ACTIVOS EN EL HOST ────────────────────────────
echo "[+] Obteniendo resolución DNS local del hipervisor..."
DNS_SERVERS=$(resolvectl status 2>/dev/null \
    | grep 'DNS Servers:' \
    | awk '{for(i=3;i<=NF;i++) print $i}' \
    | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$') || true
if [ -z "$DNS_SERVERS" ]; then
    DNS_SERVERS=$(grep -E '^nameserver' /etc/resolv.conf \
        | awk '{print $2}' | grep -v '127\.0\.0\.') || true
fi
[ -z "$DNS_SERVERS" ] && DNS_SERVERS="172.20.32.3\n8.8.8.8"

RESOLV_CONF_CONTENT=""
for dns in ${DNS_SERVERS}; do
    RESOLV_CONF_CONTENT="${RESOLV_CONF_CONTENT}nameserver ${dns}\n"
done
echo "  Servidores DNS detectados: $(echo $DNS_SERVERS | tr '\n' ' ')"

# ─── 5. INSTALL AND CONFIGURE COBBLER REMOTELY ON JUMPSTART ──────────────────
echo "[+] Running Cobbler installation and provisioning on Jumpstart..."

export HOST_PUBKEY RESOLV_CONF_CONTENT

# Upload template files to Jumpstart before the remote session
scp -i "${HOST_KEY_FILE}" ${SSH_OPTS} \
    "${TEMPLATES_DIR}/systemd/no-rate-limit.conf" \
    root@${JUMPSTART_IP}:/tmp/tpl_no_rate_limit.conf
scp -i "${HOST_KEY_FILE}" ${SSH_OPTS} \
    "${TEMPLATES_DIR}/systemd/isc-dhcp-timeout.conf" \
    root@${JUMPSTART_IP}:/tmp/tpl_isc_dhcp_timeout.conf
scp -i "${HOST_KEY_FILE}" ${SSH_OPTS} \
    "${TEMPLATES_DIR}/systemd/cobblerd-timeout.conf" \
    root@${JUMPSTART_IP}:/tmp/tpl_cobblerd_timeout.conf
scp -i "${HOST_KEY_FILE}" ${SSH_OPTS} \
    "${TEMPLATES_DIR}/cobbler/settings-patch.py" \
    root@${JUMPSTART_IP}:/tmp/tpl_settings_patch.py
scp -i "${HOST_KEY_FILE}" ${SSH_OPTS} \
    "${TEMPLATES_DIR}/cobbler/dhcp.template" \
    root@${JUMPSTART_IP}:/tmp/tpl_dhcp.template
scp -i "${HOST_KEY_FILE}" ${SSH_OPTS} \
    "${TEMPLATES_DIR}/cobbler/named.template" \
    root@${JUMPSTART_IP}:/tmp/tpl_named.template
scp -i "${HOST_KEY_FILE}" ${SSH_OPTS} \
    "${TEMPLATES_DIR}/cobbler/ubuntu-24.04-autoinstall.yaml" \
    root@${JUMPSTART_IP}:/tmp/tpl_autoinstall.yaml

ssh -i "${HOST_KEY_FILE}" ${SSH_OPTS} "root@${JUMPSTART_IP}" "HOST_PUBKEY='${HOST_PUBKEY}' RESOLV_CONF_CONTENT='${RESOLV_CONF_CONTENT}' bash" << 'REMOTE_EOF'
set -euo pipefail

# ── Fase 1: Repositorios de Paquetes ──
echo "[+] [1/14] Activando repositorio universe de Ubuntu..."
add-apt-repository -y universe 2>/dev/null || true
apt-get update -qq

echo "[+] [2/14] Añadiendo repositorio OpenSUSE Build Service para Cobbler..."
COBBLER_REPO_24="https://download.opensuse.org/repositories/systemsmanagement:/cobbler:/release33/xUbuntu_24.04"
COBBLER_REPO_22="https://download.opensuse.org/repositories/systemsmanagement:/cobbler:/release33/xUbuntu_22.04"

COBBLER_REPO_OK=0
# Probar repositorio nativo de 24.04 Noble
if curl -sf --max-time 15 "${COBBLER_REPO_24}/Release" >/dev/null 2>&1; then
    echo "  [+] Repositorio para 24.04 (Noble) disponible y activo"
    echo "deb [trusted=yes] ${COBBLER_REPO_24}/ /" > /etc/apt/sources.list.d/cobbler.list
    apt-get update -qq 2>/dev/null \
        && apt-cache show cobbler >/dev/null 2>&1 \
        && COBBLER_REPO_OK=1
fi

# Fallback al repositorio de 22.04 Jammy en caso de caída del servidor Noble
if [ "$COBBLER_REPO_OK" -eq 0 ]; then
    echo "  [WARN] Repositorio de Noble inaccesible. Configurando fallback a Jammy (22.04)..."
    echo "deb [trusted=yes] ${COBBLER_REPO_22}/ /" > /etc/apt/sources.list.d/cobbler.list
    apt-get update -qq 2>/dev/null || true
fi

# ── Fase 2: Instalación de Paquetes y Dependencias ──
echo "[+] [3/14] Instalando servicios de Cobbler, DHCP, TFTP, DNS y NFS..."
DEBIAN_FRONTEND=noninteractive apt-get install -y \
    cobbler apache2 isc-dhcp-server tftpd-hpa nfs-kernel-server \
    libapache2-mod-wsgi-py3 python3-yaml \
    bind9 bind9utils pxelinux ipxe \
    shim-signed grub-efi-amd64-signed syslinux-common \
    curl wget rsync 2>&1 \
|| {
    echo "  [WARN] Instalación inicial incompleta, forzando resolución de dependencias (--fix-broken)..."
    DEBIAN_FRONTEND=noninteractive apt-get install -y --fix-broken \
        cobbler apache2 isc-dhcp-server tftpd-hpa nfs-kernel-server \
        libapache2-mod-wsgi-py3 python3-yaml \
        bind9 bind9utils pxelinux ipxe syslinux-common \
        curl wget rsync 2>&1
}

# Cobbler requiere Django para funcionar. Asegurar su presencia en python3.
if ! python3 -c "import django" 2>/dev/null; then
    echo "  [+] Django no encontrado. Instalando mediante python3-pip..."
    DEBIAN_FRONTEND=noninteractive apt-get install -y python3-pip 2>/dev/null || true
    pip3 install "django>=3.2,<5" --break-system-packages 2>/dev/null \
        || pip3 install "django>=3.2,<5" 2>/dev/null || true
fi

# ── Fase 3: Configuración del DNS Resolv.conf ──
echo "[+] [4/14] Escribiendo resolución DNS para el servidor Jumpstart..."
printf "${RESOLV_CONF_CONTENT}" > /etc/resolv.conf
echo "  [OK] /etc/resolv.conf actualizado"

# ── Fase 4: Ajustes del Límite de Reinicios en Systemd ──
# Evita fallos de arranque múltiple por dependencias temporales en el inicio del sistema
echo "[+] [5/14] Applying systemd start-limit overrides..."
for svc in named.service isc-dhcp-server.service cobblerd.service; do
    svc_dir="/etc/systemd/system/${svc}.d"
    mkdir -p "$svc_dir"
    cp /tmp/tpl_no_rate_limit.conf "${svc_dir}/override.conf"
done
cat /tmp/tpl_isc_dhcp_timeout.conf >> /etc/systemd/system/isc-dhcp-server.service.d/override.conf
cat /tmp/tpl_cobblerd_timeout.conf >> /etc/systemd/system/cobblerd.service.d/override.conf
systemctl daemon-reload

# ── Fase 5: Interfaces del Servidor DHCP ──
echo "[+] [6/14] Asignando interfaces de escucha para isc-dhcp-server..."
INT_IF=$(ip -o link show 2>/dev/null | awk -F': ' '/52:54:00:10:00:01/{print $2}' | head -1)
[ -z "$INT_IF" ] && INT_IF="enp1s0"
MAIN_IF=$(ip -o link show 2>/dev/null | awk -F': ' '/52:54:00:10:02:0a/{print $2}' | head -1)
[ -z "$MAIN_IF" ] && MAIN_IF="enp2s0"
echo "  Interfaces detectadas: Interna (${INT_IF}) | Cliente/Main (${MAIN_IF})"

IFACES_LINE="INTERFACESv4=\"${INT_IF} ${MAIN_IF}\""
if grep -q 'INTERFACESv4=' /etc/default/isc-dhcp-server 2>/dev/null; then
    sed -i "s|INTERFACESv4=.*|${IFACES_LINE}|" /etc/default/isc-dhcp-server
else
    echo "${IFACES_LINE}" >> /etc/default/isc-dhcp-server
fi

echo "[+] [7/14] Modifying Cobbler settings (settings.yaml)..."
SETTINGS="/etc/cobbler/settings.yaml"
if [ ! -f "$SETTINGS" ]; then
    echo "[ERROR] settings.yaml not found."
    exit 1
fi
python3 /tmp/tpl_settings_patch.py

echo "[+] [8/14] Writing DHCP template..."
cp /tmp/tpl_dhcp.template /etc/cobbler/dhcp.template

echo "[+] [9/14] Writing BIND9 DNS named template..."
cp /tmp/tpl_named.template /etc/cobbler/named.template

mkdir -p /var/cache/bind/data
chown -R bind:bind /var/cache/bind

# ── Fase 9: Parámetros del Servidor TFTP ──
echo "[+] [10/14] Configurando parámetros de tftpd-hpa..."
if [ -f /etc/default/tftpd-hpa ]; then
    sed -i 's|^TFTP_DIRECTORY=.*|TFTP_DIRECTORY="/srv/tftpboot"|' /etc/default/tftpd-hpa
    sed -i 's|^TFTP_OPTIONS=.*|TFTP_OPTIONS="--secure --create"|' /etc/default/tftpd-hpa
fi

# ── Fase 9.5: Exportación por NFS para Instalación Remota ──
echo "[+] Configurando recurso compartido NFS del mirror de distribución..."
mkdir -p /var/www/cobbler/distro_mirror/ubuntu-24.04
NFS_EXPORT="/var/www/cobbler/distro_mirror/ubuntu-24.04 *(ro,async,no_root_squash,no_subtree_check)"
if ! grep -qF "/var/www/cobbler/distro_mirror/ubuntu-24.04" /etc/exports 2>/dev/null; then
    echo "${NFS_EXPORT}" >> /etc/exports
    echo "  [OK] Ruta NFS registrada en /etc/exports"
else
    echo "  [OK] Ruta NFS ya se encontraba configurada"
fi

# ── Fase 10: Inicialización y Activación de los Servicios ──
echo "[+] [11/14] Levantando y activando servicios en systemd..."
mkdir -p /srv/tftpboot
a2enmod wsgi 2>/dev/null || true
a2enconf cobbler 2>/dev/null || true

for svc in tftpd-hpa apache2 cobblerd nfs-kernel-server named; do
    systemctl restart "$svc" && systemctl enable "$svc" \
        && echo "  [OK] Servicio $svc arrancado correctamente" \
        || echo "  [WARN] Falló el arranque de $svc (Verifique systemctl status $svc)"
done

# Esperar a que el demonio central cobblerd exponga la API
echo "[+] Sonda de estado del demonio cobblerd..."
for i in $(seq 1 40); do
    if systemctl is-active --quiet cobblerd; then
        echo "  [OK] Servicio cobblerd responde activamente (Sonda $i)"
        break
    fi
    [ "$i" -eq 40 ] && {
        echo "[ERROR] El daemon de Cobbler falló al levantar. Logs recientes:"
        journalctl -u cobblerd --no-pager -n 40
        exit 1
    }
    sleep 3
done
sleep 2

# ── Fase 11: Publicar Claves Públicas SSH del Cluster ──
echo "[+] [12/14] Copiando claves SSH autorizadas para la provisión PXE..."
mkdir -p /var/www/cobbler/pub

# Generar llave de root de Jumpstart si no existiese
[ -f /root/.ssh/id_ed25519 ] || ssh-keygen -t ed25519 -N "" -f /root/.ssh/id_ed25519

# Publicar claves en la web interna para que los nodos las descarguen al instalar
{
    cat /root/.ssh/id_ed25519.pub
    echo "${HOST_PUBKEY}"
} | awk '!seen[$0]++' > /var/www/cobbler/pub/authorized_keys
chmod 644 /var/www/cobbler/pub/authorized_keys
echo "  [OK] Claves SSH autorizadas publicadas"

# ── Fase 12: Extraer e Importar la Imagen de Distribución ──
echo "[+] [13/14] Importando archivos de la ISO de Ubuntu 24.04 LTS en el mirror..."
ISO_FILE="/var/lib/cobbler/isos/ubuntu-24.04-live-server-amd64.iso"
MOUNT_POINT="/mnt/ubuntu-24.04"

[ -f "$ISO_FILE" ] || { echo "[ERROR] ISO no hallada en ruta: $ISO_FILE"; exit 1; }

# Limpiar posibles registros huérfanos anteriores
cobbler system list 2>/dev/null | awk '{print $1}' | while read -r sys; do
    [ -n "$sys" ] && cobbler system remove --name="$sys" &>/dev/null || true
done
cobbler profile remove --name="ubuntu-24.04-x86_64" 2>/dev/null || true
cobbler distro  remove --name="ubuntu-24.04-x86_64" 2>/dev/null || true

mkdir -p "$MOUNT_POINT"
mount -o loop "$ISO_FILE" "$MOUNT_POINT" 2>/dev/null || true

echo "  Copiando árbol de directorios de la distribución (rsync)..."
mkdir -p /var/www/cobbler/distro_mirror/ubuntu-24.04
rsync -a --progress "${MOUNT_POINT}/" /var/www/cobbler/distro_mirror/ubuntu-24.04/ 2>&1 | tail -5

umount "$MOUNT_POINT" 2>/dev/null || true

# Registrar la distribución en Cobbler
cobbler distro add \
    --name="ubuntu-24.04-x86_64" \
    --kernel="/var/www/cobbler/distro_mirror/ubuntu-24.04/casper/vmlinuz" \
    --initrd="/var/www/cobbler/distro_mirror/ubuntu-24.04/casper/initrd" \
    --breed=ubuntu
echo "  [OK] Distribución agregada a Cobbler"

echo "[+] Writing autoinstall template..."
mkdir -p /etc/cobbler/autoinstall_templates /var/lib/cobbler/templates
cp /tmp/tpl_autoinstall.yaml /etc/cobbler/autoinstall_templates/ubuntu-24.04-autoinstall.yaml

cp /etc/cobbler/autoinstall_templates/ubuntu-24.04-autoinstall.yaml \
   /var/lib/cobbler/templates/ubuntu-24.04-autoinstall.yaml

# Crear el perfil que asocia la ISO con la plantilla Autoinstall
cobbler profile add \
    --name="ubuntu-24.04-x86_64" \
    --distro="ubuntu-24.04-x86_64" \
    --autoinstall="ubuntu-24.04-autoinstall.yaml" \
    --autoinstall-meta="hostname=default"
echo "  [OK] Perfil de instalación generado"

# ── Fase 14: Sincronización y Configuración Final ──
echo "[+] [14/14] Generando cargadores de arranque TFTP y ejecutando sync..."
cobbler mkloaders 2>/dev/null || true
cobbler sync

echo ""
echo "══════════════════════════════════════════"
echo "  Estado de servicios Cobbler:"
for svc in cobblerd apache2 tftpd-hpa isc-dhcp-server named nfs-kernel-server; do
    printf "    %-20s %s\n" "$svc" "$(systemctl is-active $svc 2>/dev/null || echo 'inactivo')"
done
echo ""
echo "  Verificación de consistencia (cobbler check):"
cobbler check 2>&1 | head -20 || true
echo ""
echo "  [OK] Servidor de aprovisionamiento Cobbler listo en $(hostname)"
echo "  [OK] Claves publicadas en http://192.168.10.10/cblr/pub/authorized_keys"
echo "══════════════════════════════════════════"

REMOTE_EOF

echo ""
echo ">>> Finalizado despliegue de Cobbler <<<"
echo "[SIGUIENTE] Ejecute: bash scripts/02_register_cobbler_nodes.sh"
echo "            Luego encienda los nodos vacíos para iniciar instalación PXE."
