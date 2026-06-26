#!/bin/bash
# 06_setup_kubernetes.sh
#
#
# Descripción:
#   Instala un clúster K3s altamente disponible compuesto por 2 nodos maestros (con etcd embebido)
#   y 2 nodos agentes/workers. Seguidamente, realiza el despliegue de MariaDB utilizando almacenamiento
#   local persistente mapeado al directorio replicado por DRBD, e inicializa el esquema SQL de WordPress.
#
# Ajustes clave del entorno:
#   - Tiempos de latencia y timeouts del motor etcd ajustados para evitar falsos positivos de caída
#     en entornos con recursos de CPU/disco ajustados (laboratorio).
#   - Lógica de reintentos robusta durante el despliegue de recursos de Kubernetes para prevenir fallos
#     temporales de indisponibilidad del API Server durante su arranque.

set -euo pipefail

# Cargar la configuración global
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/config.sh"

echo ">>> Iniciando despliegue de Kubernetes (K3s) para el Clúster HA <<<"

# Ruta local de manifiestos YAML de Kubernetes
K8S_MANIFESTS_DIR="${SCRIPT_DIR}/../kubernetes"

# ==============================================================================
# PASO 1: INSTALAR PRIMER MAESTRO (INICIALIZADOR DEL CLÚSTER K3s)
# ==============================================================================
echo ""
echo "[+] Inicializando Nodo Master 1 ($MASTER1_IP) en modo cluster-init..."
ssh ${SSH_OPTS} root@$MASTER1_IP << EOF
    set -euo pipefail

    # Omitir instalación si el clúster ya responde localmente
    if systemctl is-active --quiet k3s 2>/dev/null && kubectl get nodes &>/dev/null; then
        echo "[OK] K3s ya está activo y respondiendo en Master 1. Omitiendo instalación."
    else
        echo "[+] Ejecutando instalador de K3s Server..."
        # Habilitar etcd con almacenamiento interno redundante
        # Aumentar timeouts de etcd para evitar división del clúster por latencia de disco (etcd-arg)
        curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="server \
            --cluster-init \
            --node-ip ${MASTER1_IP} \
            --tls-san ${MASTER1_IP} \
            --tls-san ${MASTER2_IP} \
            --write-kubeconfig-mode 644 \
            --etcd-arg=heartbeat-interval=1000 \
            --etcd-arg=election-timeout=10000" sh -

        echo "[+] Habilitando servicio systemd de K3s..."
        systemctl daemon-reload
        systemctl enable k3s
        systemctl start k3s --no-block
    fi

    # Sonda para esperar que la API local esté disponible
    echo "[+] Esperando a que el API Server esté listo..."
    K3S_READY=false
    for i in \$(seq 1 30); do
        if kubectl get nodes &>/dev/null; then
            echo "[OK] API Server de K3s responde consultas."
            K3S_READY=true
            break
        fi
        echo "[*] Sonda \$i/30: reintentando en 10 segundos..."
        sleep 10
    done
    
    if [ "\$K3S_READY" = "false" ]; then
      echo '✗ ERROR: K3s no arrancó tras 5 minutos de espera.'
      systemctl status k3s || true
      journalctl -u k3s -n 50 || true
      exit 1
    fi

    echo "[+] Estado inicial de nodos:"
    kubectl get nodes

    echo "[+] Etiquetando Master 1 como nodo de almacenamiento DRBD primario..."
    kubectl label node internal-master1 drbd-status=primary --overwrite || true
EOF

# ==============================================================================
# PASO 2: EXTRAER EL TOKEN DE UNIÓN DEL CLÚSTER (K3S TOKEN)
# ==============================================================================
echo ""
echo "[+] Obteniendo Token de unión desde Master 1..."
K3S_TOKEN=$(ssh ${SSH_OPTS} root@$MASTER1_IP "cat /var/lib/rancher/k3s/server/node-token")
echo "[OK] Token recuperado con éxito."

# ==============================================================================
# PASO 3: INSTALAR SEGUNDO MAESTRO (ALTA DISPONIBILIDAD DEL CONTROL PLANE)
# ==============================================================================
echo ""
echo "[+] Uniendo Nodo Master 2 ($MASTER2_IP) al clúster K3s..."
ssh ${SSH_OPTS} root@$MASTER2_IP << EOF
    set -euo pipefail

    if systemctl is-active --quiet k3s 2>/dev/null && kubectl get nodes &>/dev/null; then
        echo "[OK] K3s ya está activo y respondiendo en Master 2. Omitiendo unión."
    else
        echo "[+] Ejecutando instalador en modo Server apuntando a Master 1..."
        curl -sfL https://get.k3s.io | K3S_URL=https://${MASTER1_IP}:6443 \
            K3S_TOKEN=${K3S_TOKEN} \
            INSTALL_K3S_EXEC="server --node-ip ${MASTER2_IP} \
            --etcd-arg=heartbeat-interval=1000 \
            --etcd-arg=election-timeout=10000" sh -

        echo "[+] Habilitando servicio systemd..."
        systemctl daemon-reload
        systemctl enable k3s
        systemctl start k3s --no-block
    fi

    echo "[+] Esperando a que el Master 2 se integre al clúster..."
    K3S_READY=false
    for i in \$(seq 1 30); do
        if kubectl get nodes | grep -q "${MASTER2_IP}"; then
            echo "[OK] Master 2 integrado en el plano de control."
            K3S_READY=true
            break
        fi
        echo "[*] Sonda \$i/30: reintentando en 10 segundos..."
        sleep 10
    done
    
    if [ "\$K3S_READY" = "false" ]; then
      echo '✗ ERROR: Master 2 no se unió en el tiempo establecido.'
      systemctl status k3s || true
      journalctl -u k3s -n 50 || true
      exit 1
    fi
EOF

# ==============================================================================
# PASO 4: CONFIGURACIÓN DE NODOS AGENTES (WORKERS)
# ==============================================================================
echo ""
echo "[+] Configurando nodos agentes (Workers)..."
for WORKER_IP in "$WORKER1_IP" "$WORKER2_IP"; do
    echo ""
    echo "[+] Conectando e instalando K3s Agent en $WORKER_IP..."
    ssh ${SSH_OPTS} root@$WORKER_IP << EOF
        set -euo pipefail

        if systemctl is-active --quiet k3s-agent 2>/dev/null; then
            echo "[OK] Agente K3s ya activo en $WORKER_IP. Omitiendo instalación."
        else
            echo "[+] Ejecutando instalador en modo Agent..."
            curl -sfL https://get.k3s.io | K3S_URL=https://${MASTER1_IP}:6443 \
                K3S_TOKEN=${K3S_TOKEN} \
                INSTALL_K3S_EXEC="agent --node-ip ${WORKER_IP}" sh -

            echo "[+] Activando servicio systemd..."
            systemctl daemon-reload
            systemctl enable k3s-agent
            systemctl start k3s-agent --no-block
        fi

        echo "[+] Validando levantamiento del Agente..."
        AGENT_READY=false
        for i in \$(seq 1 15); do
            if systemctl is-active --quiet k3s-agent; then
                echo "[OK] Daemon k3s-agent en ejecución."
                AGENT_READY=true
                break
            fi
            sleep 10
        done
        
        if [ "\$AGENT_READY" = "false" ]; then
            echo "✗ ERROR: El Agente K3s falló al iniciar en $WORKER_IP"
            systemctl status k3s-agent || true
            exit 1
        fi
EOF
done

# ==============================================================================
# PASO 5: VERIFICAR CONSISTENCIA Y ESTADO 'READY' DE NODOS
# ==============================================================================
echo ""
echo "[+] Comprobando estado general de salud del clúster..."
ssh ${SSH_OPTS} root@$MASTER1_IP << 'EOF'
    echo "[+] Listado de nodos registrados:"
    kubectl get nodes -o wide || true
    echo ""
    
    echo "[+] Esperando a que todos los nodos reporten estado 'Ready'..."
    NODES_READY=false
    for i in $(seq 1 30); do
        if NODES_OUT=$(kubectl get nodes --no-headers 2>/dev/null); then
            NOT_READY=$(echo "$NODES_OUT" | grep -v " Ready" | wc -l)
            TOTAL_NODES=$(echo "$NODES_OUT" | wc -l)
            if [ "$NOT_READY" -eq 0 ] && [ "$TOTAL_NODES" -ge 4 ]; then
                echo "[OK] Todos los nodos ($TOTAL_NODES) se encuentran operativos."
                NODES_READY=true
                break
            fi
            echo "[*] Sonda $i/30: $NOT_READY nodo(s) no listos de $TOTAL_NODES detectados. Esperando..."
        else
            echo "[*] Sonda $i/30: Conexión con el API Server caída temporalmente. Reintentando..."
        fi
        sleep 10
    done
    
    if [ "$NODES_READY" = "false" ]; then
        echo "✗ ERROR: No todos los nodos alcanzaron estado Ready en 5 minutos."
        kubectl get nodes -o wide || true
        exit 1
    fi
    kubectl get nodes -o wide || true
EOF

# ==============================================================================
# PASO 6: APLICAR MANIFESTOS DE KUBERNETES Y DEPLOY DE MARIADB
# ==============================================================================
echo ""
echo "[+] Validando estado del clúster antes de desplegar base de datos..."
NOT_READY=$(ssh ${SSH_OPTS} root@$MASTER1_IP "kubectl get nodes --no-headers | grep -v ' Ready' | wc -l" 2>/dev/null || echo "999")
if [ "$NOT_READY" -ne 0 ]; then
  echo "✗ ERROR: Hay $NOT_READY nodo(s) inactivo/s. Se cancela el despliegue de MariaDB."
  ssh ${SSH_OPTS} root@$MASTER1_IP "kubectl get nodes" || true
  exit 1
fi
echo "[OK] Clúster sano para despliegue."

echo ""
echo "[+] Transfiriendo manifiestos YAML a Master 1..."
scp ${SSH_OPTS} -r "${K8S_MANIFESTS_DIR}" root@${MASTER1_IP}:/tmp/k8s-manifests

echo "[+] Aplicando manifiestos de MariaDB en el clúster..."
ssh ${SSH_OPTS} root@$MASTER1_IP << 'EOF'
    set -euo pipefail
    MANIFESTS="/tmp/k8s-manifests"

    # Lógica de reintentos ante colisiones en la API de Kubernetes durante el arranque
    apply_with_retry() {
        local file=$1
        local max_attempts=6
        local attempt=1
        while [ $attempt -le $max_attempts ]; do
            if kubectl apply -f "$file"; then
                return 0
            fi
            echo "[*] Reintento ($attempt/$max_attempts): Ocurrió un error aplicando $file. Esperando..."
            sleep 10
            attempt=$((attempt + 1))
        done
        echo "✗ ERROR: Fallo insalvable aplicando el recurso $file"
        return 1
    }

    # 6a. Crear espacio de nombres
    echo "[+] Creando namespace 'cms'..."
    apply_with_retry $MANIFESTS/namespace.yaml

    # 6b. Guardar credenciales seguras
    echo "[+] Creando secretos de MariaDB..."
    apply_with_retry $MANIFESTS/mariadb-secret.yaml

    # 6c. Configurar almacenamiento persistente (Volumen en local /mnt/data/mariadb)
    # Al utilizar DRBD, esta carpeta está físicamente montada sobre el dispositivo distribuido
    echo "[+] Creando volumen persistente y PVC para MariaDB..."
    apply_with_retry $MANIFESTS/mariadb-pv.yaml
    apply_with_retry $MANIFESTS/mariadb-pvc.yaml

    # 6d. Desplegar abstracción de red (Service)
    echo "[+] Creando servicios de red de MariaDB (ClusterIP + NodePort)..."
    apply_with_retry $MANIFESTS/mariadb-service.yaml

    # 6e. Desplegar aplicación con estado (StatefulSet)
    echo "[+] Desplegando pod StatefulSet de MariaDB..."
    apply_with_retry $MANIFESTS/mariadb-statefulset.yaml

    echo "[+] Esperando inicialización del contenedor de MariaDB..."
    for i in $(seq 1 24); do
        if kubectl get pods -l app=mariadb -n cms --no-headers 2>/dev/null | grep -q .; then
            break
        fi
        sleep 5
    done

    # Alerta en caso de problemas de scheduling (ej. afinidad de DRBD incorrecta)
    POD_STATUS=$(kubectl get pods -l app=mariadb -n cms --no-headers 2>/dev/null | awk '{print $3}' || true)
    if [ "$POD_STATUS" = "Pending" ]; then
        echo "[WARN] Pod de MariaDB retenido en estado Pending. Analizando causas..."
        kubectl describe pod -l app=mariadb -n cms | grep -A5 Events || true
    fi

    # Bloquear hasta que MariaDB pase la prueba de Readiness
    kubectl wait --for=condition=ready pod -l app=mariadb -n cms --timeout=300s
    echo "[OK] Contenedor de MariaDB respondiendo de manera correcta."
    kubectl get pods -n cms -o wide || true

    # 6f. Inicializar la base de datos WordPress (esquema SQL y tablas)
    echo "[+] Limpiando jobs de base de datos antiguos para evitar colisiones..."
    kubectl delete job init-wordpress-db -n cms --ignore-not-found=true 2>/dev/null || true
    sleep 5

    echo "[+] Lanzando Job de inicialización (init-wordpress-db)..."
    apply_with_retry $MANIFESTS/init-db-job.yaml

    echo "[+] Esperando finalización del Job de base de datos..."
    kubectl wait --for=condition=complete job/init-wordpress-db -n cms --timeout=180s

    echo "[OK] Base de datos y privilegios del CMS inicializados correctamente."

    # Mostrar inventario de recursos
    echo ""
    echo "[+] Inventario actual en namespace 'cms':"
    kubectl get all -n cms
    echo ""
    echo "[+] Servicios NodePort expuestos:"
    kubectl get svc -n cms -o wide

    # Limpieza de temporales
    rm -rf /tmp/k8s-manifests
EOF

echo ""
echo ">>> Finalizado despliegue de Kubernetes (K3s) <<<"
echo "[INFO] Topología HA: 2 maestros ($MASTER1_IP, $MASTER2_IP) y 2 workers ($WORKER1_IP, $WORKER2_IP)"
echo "[INFO] MariaDB expuesta en el puerto NodePort 30306"
