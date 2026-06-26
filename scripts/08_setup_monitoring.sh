#!/bin/bash
# 08_setup_monitoring.sh
#
# Triggers a Puppet agent run on internal-monitor and all cluster nodes.
# Puppet manages the full desired state for monitoring:
#   - internal-monitor: Prometheus + prometheus.yml, Grafana + provisioning files, UFW
#   - All other nodes:  prometheus-node-exporter (via role::base)
#
# Pre-requisites:
#   - 04_setup_puppet.sh must have completed (agents registered and certs signed).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/config.sh"

PUPPET="/opt/puppetlabs/bin/puppet"
PUPPET_RUN="$PUPPET agent -t"

echo ">>> Applying Puppet configuration — Monitoring Stack <<<"
echo ""

run_puppet() {
    local NODE_IP="$1"
    local NODE_NAME="$2"
    echo "[+] $NODE_NAME ($NODE_IP): starting Puppet run..."
    local EXIT_CODE
    ssh ${SSH_OPTS} root@"$NODE_IP" "$PUPPET_RUN" 2>&1 | sed "s/^/    [$NODE_NAME] /"
    EXIT_CODE=${PIPESTATUS[0]}
    if [ $EXIT_CODE -eq 0 ] || [ $EXIT_CODE -eq 2 ]; then
        echo "  [OK] $NODE_NAME converged."
    else
        echo "  [WARN] $NODE_NAME Puppet run returned exit $EXIT_CODE."
    fi
}

# Apply monitoring role on the observability node
run_puppet "$MONITOR_IP" "internal-monitor"

# Apply base role (node_exporter) on all other nodes in parallel
echo ""
echo "[+] Triggering node_exporter convergence on cluster nodes..."

declare -A NODE_PIDS
ALL_NODES=(
    "${MASTER1_IP}:internal-master1"
    "${MASTER2_IP}:internal-master2"
    "${WORKER1_IP}:internal-worker1"
    "${WORKER2_IP}:internal-worker2"
    "${STORAGE_IP}:internal-storage"
    "${LB_IP}:main-lb"
    "${CMS1_IP}:main-cms1"
    "${CMS2_IP}:main-cms2"
)

for ENTRY in "${ALL_NODES[@]}"; do
    NODE_IP="${ENTRY%%:*}"
    NODE_NAME="${ENTRY##*:}"
    if ssh ${SSH_OPTS} -o ConnectTimeout=5 root@"$NODE_IP" true 2>/dev/null; then
        ssh ${SSH_OPTS} root@"$NODE_IP" "$PUPPET_RUN" >/dev/null 2>&1 &
        NODE_PIDS[$NODE_NAME]=$!
    else
        echo "  [SKIP] $NODE_NAME ($NODE_IP) not reachable."
    fi
done

# Wait for all parallel runs to finish
echo "[+] Waiting for parallel Puppet runs to complete..."
for NODE_NAME in "${!NODE_PIDS[@]}"; do
    wait "${NODE_PIDS[$NODE_NAME]}" && echo "  [OK]  $NODE_NAME" || echo "  [WARN] $NODE_NAME returned non-zero"
done

echo ""
echo ">>> Verifying monitoring services <<<"

# Prometheus
echo "[+] Checking Prometheus..."
ssh ${SSH_OPTS} root@"$MONITOR_IP" 'systemctl is-active prometheus' && \
    echo "  [OK] Prometheus active."

# Grafana
echo "[+] Checking Grafana..."
ssh ${SSH_OPTS} root@"$MONITOR_IP" 'systemctl is-active grafana-server' && \
    echo "  [OK] Grafana active."

# Prometheus API self-check
echo "[+] Prometheus API reachability..."
if ssh ${SSH_OPTS} root@"$MONITOR_IP" \
    'curl -sf http://localhost:9090/-/ready | grep -q "Prometheus"' 2>/dev/null || \
    ssh ${SSH_OPTS} root@"$MONITOR_IP" \
    'curl -sf http://localhost:9090/api/v1/status/buildinfo' >/dev/null 2>&1; then
    echo "  [OK] Prometheus API responding."
else
    echo "  [WARN] Prometheus API not yet ready — may need ~30s to start."
fi

echo ""
echo "════════════════════════════════════════════════════════════════"
echo "  Monitoring stack deployment complete (managed by Puppet)"
echo "  Prometheus:  http://$MONITOR_IP:9090"
echo "  Grafana:     http://$MONITOR_IP:3000  (admin / admin)"
echo "════════════════════════════════════════════════════════════════"
