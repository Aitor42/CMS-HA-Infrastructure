#!/bin/bash
# 03_repair_ssh_puppet.sh
#
#
# Descripción:
#   Restaura y verifica el acceso SSH sin contraseña desde el nodo Jumpstart hacia
#   todos los nodos clientes, y repara el estado de la Autoridad de Certificación (CA)
#   de Puppet Server en Jumpstart. Esto es crucial cuando se reanuda un despliegue
#   o se recrean VMs y los certificados previos quedan obsoletos o desincronizados.
#
# Requisitos:
#   La clave SSH pública del hipervisor (~/.ssh/id_ed25519_gar) debe estar inyectada
#   en los nodos autorizados (esto ocurre por PXE en Cobbler o via 00_init_vms.sh).

set -uo pipefail

# Cargar la configuración centralizada
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/config.sh"

JUMPSTART_USER="${JUMPSTART_USER:-admin}"
PUPPET_SERVER_FQDN="${PUPPET_SERVER_FQDN:-jumpstart.internal.local}"

# Opciones SSH comunes para comprobación y automatización sin interacción
SSH_COMMON_OPTS=(
    -F /dev/null
    -o StrictHostKeyChecking=no
    -o UserKnownHostsFile=/dev/null
    -o ConnectTimeout=8
)
SSH_BATCH_OPTS=("${SSH_COMMON_OPTS[@]}" -o BatchMode=yes -i "${HOST_KEY_FILE}")
SSH_KEY_OPTS=("${SSH_COMMON_OPTS[@]}" -o BatchMode=yes -i "${HOST_KEY_FILE}")

# Valida que la clave privada requerida exista en el hipervisor local
require_host_key() {
    if [ ! -f "${HOST_KEY_FILE}" ]; then
        error "Clave SSH del hipervisor no hallada en: ${HOST_KEY_FILE}"
        error "Genérela ejecutando: ssh-keygen -t ed25519 -N '' -f ${HOST_KEY_FILE}"
        return 1
    fi
    return 0
}

# Devuelve la lista de nodos en formato NOMBRE|IP|FQDN
node_entries() {
    cat <<'NODES'
ufw-router|192.168.10.1|ufw-router.internal.local
internal-monitor|192.168.10.20|internal-monitor.internal.local
internal-master1|192.168.10.11|internal-master1.internal.local
internal-master2|192.168.10.12|internal-master2.internal.local
internal-worker1|192.168.10.13|internal-worker1.internal.local
internal-worker2|192.168.10.14|internal-worker2.internal.local
internal-storage|192.168.10.15|internal-storage.internal.local
main-lb|192.168.20.100|main-lb.main.local
main-cms1|192.168.20.101|main-cms1.main.local
main-cms2|192.168.20.102|main-cms2.main.local
NODES
    local i ip_last
    for i in $(seq 1 "$NUM_HOTDESKS"); do
        ip_last=$((200 + i))
        printf 'main-hotdesk%s|192.168.20.%s|main-hotdesk%s.main.local\n' "$i" "$ip_last" "$i"
    done
}

# Asegura el acceso como root en Jumpstart
bootstrap_jumpstart_root() {
    info "Comprobando acceso SSH de root a jumpstart (${JUMPSTART_IP})..."
    
    # 1. Intentar acceso directo mediante clave SSH
    if ssh "${SSH_KEY_OPTS[@]}" "root@${JUMPSTART_IP}" true 2>/dev/null; then
        success "Acceso root por SSH a jumpstart operativo"
        return 0
    fi

    # 2. Si falla, intentar acceder como 'admin' y copiar la clave a root
    require_host_key || return 1
    if ssh "${SSH_KEY_OPTS[@]}" "${JUMPSTART_USER}@${JUMPSTART_IP}" true 2>/dev/null; then
        info "Acceso admin válido. Copiando claves al home de root via sudo..."
        ssh "${SSH_KEY_OPTS[@]}" "${JUMPSTART_USER}@${JUMPSTART_IP}" 'bash -s' << 'ELEVATE'
sudo mkdir -p /root/.ssh
sudo cp /home/admin/.ssh/authorized_keys /root/.ssh/authorized_keys
sudo chown -R root:root /root/.ssh
sudo chmod 700 /root/.ssh
sudo chmod 600 /root/.ssh/authorized_keys
ELEVATE
        ssh "${SSH_KEY_OPTS[@]}" "root@${JUMPSTART_IP}" true
        return $?
    fi

    error "Incapaz de establecer conexión SSH con admin o root en ${JUMPSTART_IP}."
    error "Asegúrese de haber inyectado la clave pública mediante 00_init_vms.sh."
    return 1
}

# Configura las llaves SSH locales y ficheros de configuración del cliente SSH en Jumpstart
configure_jumpstart_ssh() {
    info "Configurando claves internas y config de SSH en jumpstart..."
    ssh "${SSH_KEY_OPTS[@]}" "root@${JUMPSTART_IP}" bash -s -- "$NUM_HOTDESKS" << 'REMOTE'
set -euo pipefail
NUM_HOTDESKS="$1"

# Asegurar disponibilidad de openssh-client
if ! command -v ssh >/dev/null 2>&1; then
    timeout 120 apt-get update -qq
    DEBIAN_FRONTEND=noninteractive timeout 120 apt-get install -y openssh-client
fi

# Configurar SSH tanto para root como para el usuario admin
for user_home in /root /home/admin; do
    user_name="$(basename "$user_home")"
    [ "$user_home" = "/root" ] && user_name="root"
    install -d -m 700 "$user_home/.ssh"
    if [ ! -f "$user_home/.ssh/id_ed25519" ]; then
        ssh-keygen -t ed25519 -N "" -f "$user_home/.ssh/id_ed25519"
    fi
    
    # Crear configuración SSH para ignorar validaciones de hostkeys en la red interna
    cat > "$user_home/.ssh/config" << 'SSHCONF'
Host 192.168.10.* 192.168.20.* *.internal.local *.main.local
    User root
    IdentityFile ~/.ssh/id_ed25519
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null
    ConnectTimeout 8
    BatchMode yes
SSHCONF
    chmod 600 "$user_home/.ssh/config"
    if [ "$user_name" = "admin" ]; then
        chown -R admin:admin "$user_home/.ssh"
    else
        chown -R root:root "$user_home/.ssh"
    fi
done

# Guardar base de datos de nodos localmente para simplificar scripts de orquestación remotos
cat >/root/gar_nodes.tsv <<'NODES'
ufw-router	192.168.10.1	ufw-router.internal.local
internal-monitor	192.168.10.20	internal-monitor.internal.local
internal-master1	192.168.10.11	internal-master1.internal.local
internal-master2	192.168.10.12	internal-master2.internal.local
internal-worker1	192.168.10.13	internal-worker1.internal.local
internal-worker2	192.168.10.14	internal-worker2.internal.local
internal-storage	192.168.10.15	internal-storage.internal.local
main-lb	192.168.20.100	main-lb.main.local
main-cms1	192.168.20.101	main-cms1.main.local
main-cms2	192.168.20.102	main-cms2.main.local
NODES
for i in $(seq 1 "$NUM_HOTDESKS"); do
    ip_last=$((200 + i))
    printf 'main-hotdesk%s\t192.168.20.%s\tmain-hotdesk%s.main.local\n' "$i" "$ip_last" "$i" >> /root/gar_nodes.tsv
done
REMOTE
}

# Distribuye la clave SSH pública del Jumpstart a root en todos los nodos clientes
distribute_jumpstart_key() {
    info "Distribuyendo clave pública de Jumpstart a todos los nodos clientes..."
    require_host_key || return 1

    local jumpstart_pubkey
    jumpstart_pubkey=$(ssh "${SSH_KEY_OPTS[@]}" "root@${JUMPSTART_IP}" "cat /root/.ssh/id_ed25519.pub" 2>/dev/null)
    if [ -z "$jumpstart_pubkey" ]; then
        error "No se pudo recuperar la clave pública de root@${JUMPSTART_IP}"
        return 1
    fi

    local failed=0
    while IFS='|' read -r name ip fqdn; do
        [ -n "$name" ] || continue
        echo "[+] Distribuyendo en: ${name} (${ip})"

        # Si el nodo responde por SSH, inyectar la clave y configurar la resolución local en /etc/hosts
        if ssh "${SSH_KEY_OPTS[@]}" -n "root@${ip}" true 2>/dev/null; then
            ssh "${SSH_KEY_OPTS[@]}" -n "root@${ip}" "set -e
                install -d -m 700 /root/.ssh /home/admin/.ssh 2>/dev/null || true
                grep -qxF '$jumpstart_pubkey' /root/.ssh/authorized_keys 2>/dev/null || echo '$jumpstart_pubkey' >> /root/.ssh/authorized_keys
                grep -qxF '$jumpstart_pubkey' /home/admin/.ssh/authorized_keys 2>/dev/null || echo '$jumpstart_pubkey' >> /home/admin/.ssh/authorized_keys
                chmod 600 /root/.ssh/authorized_keys /home/admin/.ssh/authorized_keys 2>/dev/null || true
                chown -R root:root /root/.ssh 2>/dev/null || true
                chown -R admin:admin /home/admin/.ssh 2>/dev/null || true
                grep -q '$fqdn' /etc/hosts || echo '$ip $fqdn $name' >> /etc/hosts" \
            || failed=$((failed + 1))
        else
            echo "[WARN] Nodo ${name} (${ip}) no responde a SSH (Omitiendo por ahora; el OS podría estar instalándose)"
        fi
    done < <(node_entries)

    return "$failed"
}

# Valida las conexiones directas sin clave desde Jumpstart
verify_jumpstart_ssh() {
    info "Verificando SSH sin contraseña directo desde Jumpstart hacia los nodos..."
    ssh "${SSH_KEY_OPTS[@]}" "root@${JUMPSTART_IP}" bash -s << 'REMOTE'
set -uo pipefail
SSH_OPTS=(-F /dev/null -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=8 -o BatchMode=yes)
failed=0
ok=0
while IFS=$'\t' read -r name ip fqdn; do
    [ -n "$name" ] || continue
    for user in root admin; do
        if ssh "${SSH_OPTS[@]}" -n "${user}@${ip}" true 2>/dev/null; then
            echo "[OK] Conectado: ${user}@${name} (${ip})"
            ((ok++)) || true
        else
            echo "[SKIP] Inaccesible: ${user}@${name} (${ip})"
        fi
    done
done < /root/gar_nodes.tsv
echo "  Total de túneles SSH válidos desde Jumpstart: $ok"
exit "$failed"
REMOTE
}

# Repara los certificados internos de CA y directorios de Puppet Server
repair_puppet_server_ca() {
    if ! ssh "${SSH_BATCH_OPTS[@]}" "root@${JUMPSTART_IP}" "[ -x /opt/puppetlabs/bin/puppetserver ] || [ -x /usr/bin/puppetserver ]" 2>/dev/null; then
        warn "Puppet Server no instalado en jumpstart (${JUMPSTART_IP}). Omitiendo reparación de CA."
        return 0
    fi
    
    info "Reparando estructura SSL y Autoridad de Certificación de Puppet Server..."
    ssh "${SSH_BATCH_OPTS[@]}" "root@${JUMPSTART_IP}" bash -s -- "$PUPPET_SERVER_FQDN" <<'REMOTE'
set -euo pipefail
PUPPET_SERVER_FQDN="$1"
PUPPET_BIN="/opt/puppetlabs/bin/puppet"
if [ -x /opt/puppetlabs/bin/puppetserver ]; then
    PUPPETSERVER_BIN="/opt/puppetlabs/bin/puppetserver"
else
    PUPPETSERVER_BIN="/usr/bin/puppetserver"
fi
PUPPET_CONF="/etc/puppetlabs/puppet/puppet.conf"
SSL_DIR="/etc/puppetlabs/puppet/ssl"

# Asegurar presencia de grupos y permisos
getent group puppet >/dev/null || groupadd -r puppet
getent passwd puppet >/dev/null || useradd -r -g puppet -d /opt/puppetlabs/server/data/puppetserver -s /usr/sbin/nologin -c "puppet server" puppet

mkdir -p /etc/puppetlabs/puppet /etc/puppetlabs/code /etc/puppet /opt/puppetlabs/server/data/puppetserver

# Corregir enlaces simbólicos de SSL e inicializar directorios
if [ -L "$SSL_DIR" ]; then
    target="$(readlink "$SSL_DIR" || true)"
    backup="/tmp/puppet_ssl_repair.$$"
    mkdir -p "$backup"
    if [ -n "$target" ] && [ -d "$target" ]; then
        cp -a "$target"/. "$backup"/ 2>/dev/null || true
    fi
    rm -f "$SSL_DIR"
    mkdir -p "$SSL_DIR"
    cp -a "$backup"/. "$SSL_DIR"/ 2>/dev/null || true
    rm -rf "$backup"
else
    mkdir -p "$SSL_DIR"
fi

rm -rf /etc/puppet/ssl
ln -s "$SSL_DIR" /etc/puppet/ssl
ln -sfn /etc/puppetlabs/puppet/puppet.conf /etc/puppet/puppet.conf
ln -sfn /etc/puppetlabs/puppet/autosign.conf /etc/puppet/autosign.conf

# Crear el puppet.conf si no existiera
if [ ! -f "$PUPPET_CONF" ]; then
    cat > "$PUPPET_CONF" <<PUPPET_CONF
[main]
certname = ${PUPPET_SERVER_FQDN}
server = ${PUPPET_SERVER_FQDN}
environment = production
runinterval = 30m
ssldir = ${SSL_DIR}

[server]
vardir = /opt/puppetlabs/server/data/puppetserver
logdir = /var/log/puppetlabs/puppetserver
rundir = /var/run/puppetlabs/puppetserver
pidfile = /var/run/puppetlabs/puppetserver/puppetserver.pid
codedir = /etc/puppetlabs/code
dns_alt_names = jumpstart,jumpstart.internal.local,puppet,puppet.internal.local
PUPPET_CONF
fi

# Ajustar configuraciones SSL y FQDN en el archivo ini de Puppet usando Python
python3 - "$PUPPET_CONF" "$SSL_DIR" "$PUPPET_SERVER_FQDN" <<'PY'
import sys
from pathlib import Path

path = Path(sys.argv[1])
ssl_dir = sys.argv[2]
certname = sys.argv[3]
lines = path.read_text().splitlines()
out = []
section = None
seen_main = False
main_has_ssl = False
main_has_certname = False
main_has_server = False

def flush_main_keys():
    global main_has_ssl, main_has_certname, main_has_server
    if not main_has_certname:
        out.append(f"certname = {certname}")
    if not main_has_server:
        out.append(f"server = {certname}")
    if not main_has_ssl:
        out.append(f"ssldir = {ssl_dir}")

for line in lines:
    stripped = line.strip()
    if stripped.startswith("[") and stripped.endswith("]"):
        if section == "main":
            flush_main_keys()
        section = stripped[1:-1].strip()
        if section == "main":
            seen_main = True
            main_has_ssl = main_has_certname = main_has_server = False
        out.append(line)
        continue
    if section == "main":
        key = stripped.split("=", 1)[0].strip() if "=" in stripped else ""
        if key == "ssldir":
            if not main_has_ssl:
                out.append(f"ssldir = {ssl_dir}")
                main_has_ssl = True
            continue
        if key == "certname":
            if not main_has_certname:
                out.append(f"certname = {certname}")
                main_has_certname = True
            continue
        if key == "server":
            if not main_has_server:
                out.append(f"server = {certname}")
                main_has_server = True
            continue
    out.append(line)

if section == "main":
    flush_main_keys()
if not seen_main:
    out.extend(["", "[main]", f"certname = {certname}", f"server = {certname}", f"ssldir = {ssl_dir}"])

path.write_text("\n".join(out).rstrip() + "\n")
PY

grep -q "jumpstart.internal.local" /etc/hosts || echo "192.168.10.10 jumpstart.internal.local jumpstart puppet" >> /etc/hosts
cat > /etc/puppetlabs/puppet/autosign.conf <<'AUTOSIGN'
*.internal.local
*.main.local
AUTOSIGN

chown -R puppet:puppet /etc/puppetlabs/puppet /etc/puppetlabs/code /opt/puppetlabs/server/data/puppetserver
chmod 755 /etc/puppetlabs/puppet
chmod -R u+rwX,g+rX,o-rwx "$SSL_DIR"

if command -v systemctl >/dev/null && systemctl list-unit-files puppetserver.service >/dev/null 2>&1; then
    systemctl restart puppetserver
    systemctl enable puppetserver >/dev/null 2>&1 || true
fi

# Inicializar CA si no está configurada y volcar estado
if ! "$PUPPETSERVER_BIN" ca list --all >/tmp/puppet_ca_list.out 2>/tmp/puppet_ca_list.err; then
    echo "[WARN] puppetserver ca list falló. Inicializando CA..."
    "$PUPPETSERVER_BIN" ca setup || true
    systemctl restart puppetserver || true
    "$PUPPETSERVER_BIN" ca list --all >/tmp/puppet_ca_list.out
fi

cat /tmp/puppet_ca_list.out
REMOTE
}

# Fuerza la sincronización de los agentes Puppet y firma los certificados en lote
run_puppet_agents() {
    if ! ssh "${SSH_BATCH_OPTS[@]}" "root@${JUMPSTART_IP}" "[ -x /opt/puppetlabs/bin/puppetserver ] || [ -x /usr/bin/puppetserver ]" 2>/dev/null; then
        warn "Puppet Server no instalado. Omitiendo ejecución de agentes."
        return 0
    fi
    info "Ejecutando puppet agent en todos los nodos y autofirmando certificados..."
    ssh "${SSH_BATCH_OPTS[@]}" "root@${JUMPSTART_IP}" bash -s -- "$PUPPET_SERVER_FQDN" <<'REMOTE'
set -uo pipefail
PUPPET_SERVER_FQDN="$1"
SSH_OPTS=(-F /dev/null -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=8 -o BatchMode=yes)
if [ -x /opt/puppetlabs/bin/puppetserver ]; then
    PUPPETSERVER_BIN="/opt/puppetlabs/bin/puppetserver"
else
    PUPPETSERVER_BIN="/usr/bin/puppetserver"
fi
failed=0

while IFS=$'\t' read -r name ip fqdn; do
    [ -n "$name" ] || continue
    echo "[+] Conectando con ${name} para inicializar agente Puppet..."
    
    # Escribir puppet.conf en el agente y forzar conexión inicial
    if ssh "${SSH_OPTS[@]}" -n "root@${ip}" "set -e; mkdir -p /etc/puppetlabs/puppet; grep -q '$PUPPET_SERVER_FQDN' /etc/hosts || echo '192.168.10.10 $PUPPET_SERVER_FQDN jumpstart puppet' >> /etc/hosts; cat > /etc/puppetlabs/puppet/puppet.conf <<EOF
[main]
certname = ${fqdn}
server = ${PUPPET_SERVER_FQDN}
environment = production
runinterval = 30m

[agent]
report = true
EOF
if [ -x /opt/puppetlabs/bin/puppet ]; then /opt/puppetlabs/bin/puppet agent -t --server ${PUPPET_SERVER_FQDN} --waitforcert 10 || true; else echo '[WARN] puppet-agent no instalado en el nodo'; fi" 2>&1; then
        true
    else
        echo "[WARN] No se pudo invocar el agente en ${name}"
        failed=$((failed + 1))
    fi
done </root/gar_nodes.tsv

# Firmar todos los certificados entrantes acumulados en el servidor
"$PUPPETSERVER_BIN" ca sign --all || true

# Ejecutar segunda pasada para validar el aprovisionamiento
while IFS=$'\t' read -r name ip fqdn; do
    [ -n "$name" ] || continue
    if ssh "${SSH_OPTS[@]}" -n "root@${ip}" "test -x /opt/puppetlabs/bin/puppet && /opt/puppetlabs/bin/puppet agent -t --server ${PUPPET_SERVER_FQDN}" 2>&1; then
        echo "[OK] ${name} conectado correctamente con Puppet Master"
    else
        echo "[FAIL] ${name} no pudo validar la conexión de certificados"
        failed=$((failed + 1))
    fi
done </root/gar_nodes.tsv

echo "[+] Estado final de la CA de Puppet:"
"$PUPPETSERVER_BIN" ca list --all || failed=$((failed + 1))
exit "$failed"
REMOTE
}

# ==============================================================================
# FLUJO DE REPARACIÓN
# ==============================================================================
main() {
    local rc=0
    bootstrap_jumpstart_root || rc=1
    if [ "$rc" -eq 0 ]; then
      if ! configure_jumpstart_ssh; then
        warn "La configuración de claves locales SSH en jumpstart ha fallado, continuando..."
      fi
    fi
    [ "$rc" -eq 0 ] && distribute_jumpstart_key || rc=1
    [ "$rc" -eq 0 ] && verify_jumpstart_ssh || rc=1
    [ "$rc" -eq 0 ] && repair_puppet_server_ca || rc=1
    [ "$rc" -eq 0 ] && run_puppet_agents || rc=1

    if [ "$rc" -eq 0 ]; then
        success "Reparación y emparejamiento SSH/Puppet completado con éxito"
    else
        error "Proceso finalizado con advertencias o fallos en nodos."
    fi
    exit "$rc"
}

main "$@"
