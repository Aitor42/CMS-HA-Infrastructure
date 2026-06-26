#!/bin/bash
# 04_setup_puppet.sh
#
# Installs and configures Puppet Server 8.x on the Jumpstart node (Puppet Master)
# and installs Puppet agents on all infrastructure nodes.
#
# Key responsibilities:
#   - Installs Puppet Server on jumpstart with reduced JVM heap (512 MB).
#   - Configures autosign for *.internal.local and *.main.local domains.
#   - Installs puppet-agent on every node and triggers initial cert exchange.
#   - Syncs all manifests and modules from the local puppet/ directory to the
#     Puppet codedir on jumpstart (production environment).
#   - After sync, triggers an initial `puppet agent -t` on all nodes so they
#     converge to their declared desired state immediately.

set -euo pipefail

# Cargar configuración global
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATES_DIR="${SCRIPT_DIR}/../templates"
source "${SCRIPT_DIR}/config.sh"

echo ">>> Iniciando despliegue de Puppet (Configuration Management) <<<"

PUPPET_MASTER_IP="$JUMPSTART_IP"
PUPPET_SERVER_FQDN="jumpstart.internal.local"

# Mapear los nombres DNS con las direcciones IP para los agentes
declare -A AGENT_NODES=(
    ["ufw-router"]="192.168.10.1"
    ["internal-monitor"]="192.168.10.20"
    ["internal-master1"]="192.168.10.11"
    ["internal-master2"]="192.168.10.12"
    ["internal-worker1"]="192.168.10.13"
    ["internal-worker2"]="192.168.10.14"
    ["internal-storage"]="192.168.10.15"
    ["main-lb"]="192.168.20.100"
    ["main-cms1"]="192.168.20.101"
    ["main-cms2"]="192.168.20.102"
)
# Agregar dinámicamente los puestos hot-desk al inventario de Puppet
for i in $(seq 1 "$NUM_HOTDESKS"); do
    AGENT_NODES["main-hotdesk${i}"]="192.168.20.$((200 + i))"
done

# ==============================================================================
# 1. INSTALAR Y CONFIGURAR PUPPET SERVER EN JUMPSTART
# ==============================================================================
echo "[+] Configuring Puppet Server on Jumpstart ($PUPPET_MASTER_IP)..."

# Upload template files to Jumpstart before the remote session
scp ${SSH_OPTS} "${TEMPLATES_DIR}/puppet/puppet-server.conf" \
    root@${PUPPET_MASTER_IP}:/tmp/tpl_puppet_server.conf
scp ${SSH_OPTS} "${TEMPLATES_DIR}/puppet/autosign.conf" \
    root@${PUPPET_MASTER_IP}:/tmp/tpl_autosign.conf

ssh ${SSH_OPTS} root@${PUPPET_MASTER_IP} <<REMOTE_EOF
set -euo pipefail

# Crear el grupo y usuario para el servicio puppet
echo "[+] Creando grupo y usuario puppet si no existen..."
getent group puppet >/dev/null || groupadd -r puppet
getent passwd puppet >/dev/null || useradd -r -g puppet -d /opt/puppetlabs/server/data/puppetserver -s /usr/sbin/nologin -c "puppet server" puppet

mkdir -p /usr/share/puppet/modules/

# Descargar e instalar repositorio de Puppet 8 oficial para Ubuntu Noble (24.04)
echo "[+] Instalando Puppet Server..."
wget -q https://apt.puppet.com/puppet8-release-noble.deb -O /tmp/puppet8-release-noble.deb
dpkg -i /tmp/puppet8-release-noble.deb || true
export DEBIAN_FRONTEND=noninteractive
apt-get update
# Solventar posibles dependencias rotas de antemano
apt-get install -o Dpkg::Options::="--force-confold" -o Dpkg::Options::="--force-confdef" -f -y
apt-get install -o Dpkg::Options::="--force-confold" -o Dpkg::Options::="--force-confdef" -y puppetserver

# Enlazar las librerías de Puppet con el entorno JRuby embebido en el servidor
echo "[+] Vinculando librerías de Puppet para JRuby..."
mkdir -p /usr/lib/ruby/vendor_ruby
for item in /opt/puppetlabs/puppet/lib/ruby/vendor_ruby/*; do
    name=\$(basename "\$item")
    rm -rf "/usr/lib/ruby/vendor_ruby/\$name"
    ln -sf "\$item" "/usr/lib/ruby/vendor_ruby/\$name"
done

# Crear estructuras de carpetas y enlaces simbólicos de compatibilidad
echo "[+] Creando enlaces de configuración de Puppet..."
mkdir -p /etc/puppetlabs/puppet
rm -rf /etc/puppet/code
ln -sf /etc/puppetlabs/code /etc/puppet/code
ln -sf /etc/puppetlabs/puppet/puppet.conf /etc/puppet/puppet.conf
ln -sf /etc/puppetlabs/puppet/autosign.conf /etc/puppet/autosign.conf

if [ -L /etc/puppetlabs/puppet/ssl ]; then
    target=\$(readlink /etc/puppetlabs/puppet/ssl || true)
    backup="/tmp/puppet-ssl-backup.\$\$"
    mkdir -p "\$backup"
    if [ -n "\$target" ] && [ -d "\$target" ]; then
        cp -a "\$target"/. "\$backup"/ 2>/dev/null || true
    fi
    rm -f /etc/puppetlabs/puppet/ssl
    mkdir -p /etc/puppetlabs/puppet/ssl
    cp -a "\$backup"/. /etc/puppetlabs/puppet/ssl/ 2>/dev/null || true
    rm -rf "\$backup"
else
    mkdir -p /etc/puppetlabs/puppet/ssl
fi
rm -rf /etc/puppet/ssl
ln -s /etc/puppetlabs/puppet/ssl /etc/puppet/ssl

# Asegurar permisos correctos sobre el directorio de trabajo de Puppet
echo "[+] Configurando permisos de directorios de Puppet..."
mkdir -p /opt/puppetlabs/server/data/puppetserver /var/log/puppetlabs /var/run/puppetlabs
chown -R puppet:puppet /etc/puppetlabs
chown -R puppet:puppet /etc/puppet
chown -R puppet:puppet /opt/puppetlabs/server/data/puppetserver
chown -R puppet:puppet /var/log/puppetlabs
chown -R puppet:puppet /var/run/puppetlabs

# NOTA CRÍTICA DE LABORATORIO:
#   Por defecto Puppet Server reserva 2GB de memoria RAM. Reducimos este valor
#   a 512MB en el arranque del daemon para no desbordar los recursos físicos del host.
echo "[+] Configurando memoria de Puppet Server (Límite: 512m)..."
sed -i -re 's/(-Xms)[0-9a-zA-Z]+ (-Xmx)[0-9a-zA-Z]+/\1512m \2512m/' /etc/default/puppetserver

# Write Puppet Server main config from template
echo "[+] Deploying puppet.conf for server..."
cp /tmp/tpl_puppet_server.conf /etc/puppetlabs/puppet/puppet.conf

# Configure autosign for automatic cert signing
echo "[+] Configuring certificate autosign..."
cp /tmp/tpl_autosign.conf /etc/puppetlabs/puppet/autosign.conf
chmod 644 /etc/puppetlabs/puppet/autosign.conf

echo "[+] Añadiendo resolución local a /etc/hosts del servidor Puppet..."
grep -q "jumpstart.internal.local" /etc/hosts || echo "192.168.10.10 jumpstart.internal.local jumpstart puppet" >> /etc/hosts

# Habilitar e iniciar Puppet Server
echo "[+] Iniciando servicio Puppet Server (Verificación de arranque)..."
systemctl start puppetserver
systemctl enable puppetserver

puppetserver ca list --all >/dev/null || puppetserver ca setup || true
echo "[+] Puppet Server instalado y en ejecución."
REMOTE_EOF

# Sonda periódica para asegurar que el servicio web de Puppet está levantado antes de pasar a los agentes
echo "[+] Esperando a que Puppet Server inicie su API HTTP..."
for i in $(seq 1 60); do
  if ssh $SSH_OPTS root@"$PUPPET_MASTER_IP" "puppetserver ca list --all" &>/dev/null; then
    echo "  ✔ Puppet Server activo y respondiendo consultas"
    break
  fi
  if [ $i -eq 60 ]; then
    echo "  ✗ ERROR: Puppet Server no arrancó dentro del tiempo de cortesía (300s)"
    exit 1
  fi
  sleep 5
done

# Desplegar los manifiestos locales (.pp) y módulos al directorio de Puppet Master
echo "[+] Copiando manifiestos y módulos locales al codedir..."
ssh ${SSH_OPTS} root@${PUPPET_MASTER_IP} "mkdir -p /etc/puppetlabs/code/environments/production"
scp ${SSH_OPTS} -r "${SCRIPT_DIR}/../puppet/"* root@${PUPPET_MASTER_IP}:/etc/puppetlabs/code/environments/production/
ssh ${SSH_OPTS} root@${PUPPET_MASTER_IP} "chown -R puppet:puppet /etc/puppetlabs/code/environments/production/"

# ==============================================================================
# 2. INSTALAR Y ASOCIAR AGENTES DE PUPPET EN LOS NODOS CLIENTES
# ==============================================================================
echo "[+] Configurando agentes de Puppet en los nodos de la infraestructura..."

SUCCESSFUL_NODES=()

for NODE_NAME in "${!AGENT_NODES[@]}"; do
    NODE_IP="${AGENT_NODES[$NODE_NAME]}"

    # Definir FQDN en función de la subred del nodo
    if [[ "$NODE_IP" == 192.168.10.* ]]; then
        NODE_FQDN="${NODE_NAME}.internal.local"
    else
        NODE_FQDN="${NODE_NAME}.main.local"
    fi

    echo "[+] Instalando Puppet Agent en ${NODE_NAME} (${NODE_IP}) -> ${NODE_FQDN}..."

    # Omitir si la máquina está apagada o no dispone de SSH
    if ! ssh ${SSH_OPTS} -o ConnectTimeout=5 root@${NODE_IP} true 2>/dev/null; then
        echo "  [SKIP] ${NODE_NAME} no responde SSH. Se omitirá (ejecutar repair_ssh cuando el nodo esté listo)."
        continue
    fi
    echo "  [✓] Canal SSH establecido con root@${NODE_IP}"

    # Instalar agente, configurar config de Puppet Agent y solicitar certificado
    ssh ${SSH_OPTS} root@${NODE_IP} <<AGENT_EOF
set -euo pipefail

echo "[+] Instalando puppet-agent oficial..."
wget -q https://apt.puppet.com/puppet8-release-noble.deb -O /tmp/puppet8-release-noble.deb
dpkg -i /tmp/puppet8-release-noble.deb || true
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -o Dpkg::Options::="--force-confold" -o Dpkg::Options::="--force-confdef" -y puppet-agent

echo "[+] Deploying puppet.conf for agent..."
export NODE_FQDN
envsubst < /tmp/tpl_puppet_agent.conf > /etc/puppetlabs/puppet/puppet.conf

# Garantizar resolución local hacia el Puppet Master
grep -q "${PUPPET_SERVER_FQDN}" /etc/hosts || \
    echo "${PUPPET_MASTER_IP} ${PUPPET_SERVER_FQDN} jumpstart puppet" >> /etc/hosts

echo "[+] Habilitando y levantando servicio puppet..."
/opt/puppetlabs/bin/puppet resource service puppet ensure=running enable=true

# Ejecución inicial para registrar la clave SSL en el servidor
echo "[+] Solicitando certificado SSL del agente al servidor..."
/opt/puppetlabs/bin/puppet agent -t --server ${PUPPET_SERVER_FQDN} --waitforcert 10 || true

echo "[+] Agente de Puppet configurado."
AGENT_EOF

    echo "[+] Agente listo en ${NODE_NAME} (${NODE_IP})."
    SUCCESSFUL_NODES+=("$NODE_NAME")
done

# ==============================================================================
# 3. VERIFICAR Y CONFIRMAR FIRMA DE CERTIFICADOS SSL DE AGENTES
# ==============================================================================
echo "[+] Validando firma de certificados en Puppet Master..."
EXPECTED_AGENTS=()
for NODE_NAME in "${SUCCESSFUL_NODES[@]}"; do
    NODE_IP="${AGENT_NODES[$NODE_NAME]}"
    if [[ "$NODE_IP" == 192.168.10.* ]]; then
        EXPECTED_AGENTS+=("${NODE_NAME}.internal.local")
    else
        EXPECTED_AGENTS+=("${NODE_NAME}.main.local")
    fi
done

if [ ${#EXPECTED_AGENTS[@]} -eq 0 ]; then
    echo "[WARN] Ningún agente disponible para verificar certificados."
else
    # Escribir listado esperado en Jumpstart
    printf '%s\n' "${EXPECTED_AGENTS[@]}" > /tmp/gar_expected_puppet_agents.txt
    scp ${SSH_OPTS} /tmp/gar_expected_puppet_agents.txt root@${PUPPET_MASTER_IP}:/tmp/gar_expected_puppet_agents.txt

    ssh ${SSH_OPTS} root@${PUPPET_MASTER_IP} <<'VERIFY_EOF'
set -euo pipefail

MAX_WAIT=120
INTERVAL=10
ELAPSED=0
mapfile -t EXPECTED_AGENTS < /tmp/gar_expected_puppet_agents.txt

echo "[+] Esperando el intercambio automático de firmas..."
while [ $ELAPSED -lt $MAX_WAIT ]; do
    # Forzar la firma de cualquier clave pendiente
    puppetserver ca sign --all >/dev/null 2>&1 || true
    SIGNED=$(puppetserver ca list --all 2>/dev/null | grep -Ec "Signed|^\+" || true)
    echo "    Nodos firmados detectados: ${SIGNED} de ${#EXPECTED_AGENTS[@]} (Bucle de espera...)"

    if [ "$SIGNED" -ge "${#EXPECTED_AGENTS[@]}" ]; then
        echo "[+] Certificación SSL completada en todos los agentes activos."
        break
    fi

    sleep $INTERVAL
    ELAPSED=$((ELAPSED + INTERVAL))
done

echo "[+] Estado general del almacén de CA:"
puppetserver ca list --all
VERIFY_EOF

fi

# ==============================================================================
# 4. SYNC PUPPET CODE (manifests + modules + files) TO JUMPSTART CODEDIR
# ==============================================================================
echo ""
echo "[+] Syncing Puppet code to production codedir on Jumpstart..."

PUPPET_DIR="${SCRIPT_DIR}/../puppet"
CODEDIR="/etc/puppetlabs/code/environments/production"

# Create the Puppet fileserver module files directory structure on jumpstart
ssh ${SSH_OPTS} root@${PUPPET_MASTER_IP} \
    "mkdir -p ${CODEDIR}/modules/role/files/nginx \
              ${CODEDIR}/modules/role/files/apache \
              ${CODEDIR}/modules/role/files/prometheus \
              ${CODEDIR}/modules/role/files/grafana"

# Sync manifests (site.pp)
scp ${SSH_OPTS} "${PUPPET_DIR}/manifests/site.pp" \
    root@${PUPPET_MASTER_IP}:"${CODEDIR}/manifests/site.pp"

# Sync all role module manifests
scp ${SSH_OPTS} "${PUPPET_DIR}/modules/role/manifests/"*.pp \
    root@${PUPPET_MASTER_IP}:"${CODEDIR}/modules/role/manifests/"

# Sync Puppet fileserver static files (config files served to agents)
scp ${SSH_OPTS} "${PUPPET_DIR}/modules/role/files/nginx/cms-lb.conf" \
    root@${PUPPET_MASTER_IP}:"${CODEDIR}/modules/role/files/nginx/cms-lb.conf"
scp ${SSH_OPTS} "${PUPPET_DIR}/modules/role/files/apache/"* \
    root@${PUPPET_MASTER_IP}:"${CODEDIR}/modules/role/files/apache/"
scp ${SSH_OPTS} "${PUPPET_DIR}/modules/role/files/prometheus/prometheus.yml" \
    root@${PUPPET_MASTER_IP}:"${CODEDIR}/modules/role/files/prometheus/prometheus.yml"
scp ${SSH_OPTS} "${PUPPET_DIR}/modules/role/files/grafana/"* \
    root@${PUPPET_MASTER_IP}:"${CODEDIR}/modules/role/files/grafana/"

# Fix ownership so puppetserver can read the code
ssh ${SSH_OPTS} root@${PUPPET_MASTER_IP} \
    "chown -R puppet:puppet ${CODEDIR}/manifests ${CODEDIR}/modules && \
     chmod -R 644 ${CODEDIR}/manifests/*.pp ${CODEDIR}/modules/role/manifests/*.pp && \
     find ${CODEDIR}/modules/role/files -type f -exec chmod 644 {} \;"

echo "  [OK] Puppet code synced to ${CODEDIR}"

# ==============================================================================
# 5. INITIAL PUPPET RUN — Apply desired state on all converged nodes
# ==============================================================================
echo ""
echo "[+] Triggering initial Puppet convergence on all registered nodes..."
echo "    (This applies the full desired state declared in the manifests)"

PUPPET_BIN="/opt/puppetlabs/bin/puppet"

for NODE_NAME in "${SUCCESSFUL_NODES[@]}"; do
    NODE_IP="${AGENT_NODES[$NODE_NAME]}"
    if ssh ${SSH_OPTS} -o ConnectTimeout=5 root@"$NODE_IP" true 2>/dev/null; then
        echo "    → $NODE_NAME ($NODE_IP)"
        ssh ${SSH_OPTS} root@"$NODE_IP" "$PUPPET_BIN agent -t" 2>&1 | \
            tail -3 | sed 's/^/        /'
    fi
done

echo ""
echo ">>> Puppet deployment complete <<<"
echo "    Agents will re-apply their catalogue every 30 minutes automatically."
