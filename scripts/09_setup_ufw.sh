#!/bin/bash
# 09_setup_ufw.sh
#
# Triggers a Puppet agent run on all nodes (including the router).
# Puppet manages the full desired state for network security:
#   - ufw-router:   role::router  → ip_forward, UFW perimeter + forwarding rules
#   - All others:   role::base    → UFW default deny + per-role allow rules
#
# The NAT/DNAT iptables rules in /etc/ufw/before.rules are injected separately
# below, since they require detecting the WAN interface name at runtime and
# cannot be expressed as pure declarative Puppet state.
#
# Pre-requisites:
#   - 04_setup_puppet.sh must have completed (agents registered and certs signed).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATES_DIR="${SCRIPT_DIR}/../templates"
source "${SCRIPT_DIR}/config.sh"

PUPPET="/opt/puppetlabs/bin/puppet"
PUPPET_RUN="$PUPPET agent -t"

echo ">>> Applying Puppet configuration — Network Security (UFW) <<<"
echo ""

run_puppet() {
    local NODE_IP="$1"
    local NODE_NAME="$2"
    echo "[+] $NODE_NAME ($NODE_IP): applying Puppet..."
    local EXIT_CODE
    ssh ${SSH_OPTS} root@"$NODE_IP" "$PUPPET_RUN" 2>&1 | sed "s/^/    [$NODE_NAME] /"
    EXIT_CODE=${PIPESTATUS[0]}
    if [ $EXIT_CODE -eq 0 ] || [ $EXIT_CODE -eq 2 ]; then
        echo "  [OK] $NODE_NAME UFW state converged."
    else
        echo "  [WARN] $NODE_NAME returned exit $EXIT_CODE."
    fi
}

# ==============================================================================
# 1. ROUTER — Apply role::router (ip_forward + UFW perimeter rules)
# ==============================================================================
run_puppet "$ROUTER_IP" "ufw-router"

# ==============================================================================
# 2. INJECT NAT/DNAT RULES INTO ROUTER (runtime WAN interface detection)
# This cannot be done by Puppet because the WAN interface name is discovered
# dynamically from the MAC address at provisioning time.
# ==============================================================================
echo ""
echo "[+] Injecting NAT/DNAT rules on ufw-router..."

scp $SSH_OPTS "${TEMPLATES_DIR}/ufw/nat-rules" root@"$ROUTER_IP":/tmp/nat-rules-tpl

ssh $SSH_OPTS root@"$ROUTER_IP" bash -s <<'NAT_APPLY'
  set -e
  WAN_IF=$(ip -o link show | grep '52:54:00:10:00:02' | awk -F': ' '{print $2}')
  WAN_IF="${WAN_IF:-ens5}"
  export WAN_IF
  envsubst < /tmp/nat-rules-tpl > /tmp/nat_rules

  # Remove any existing NAT block to avoid duplicates
  sed -i '/^# NAT Rules/,/^COMMIT/{/^COMMIT/d; d}' /etc/ufw/before.rules
  sed -i '/^\*nat/,/^COMMIT/{/^COMMIT/d; d}' /etc/ufw/before.rules

  # Inject before the *filter section
  awk '/^\*filter/{while((getline line < "/tmp/nat_rules") > 0) print line} {print}' \
    /etc/ufw/before.rules > /tmp/before.rules.new
  mv /tmp/before.rules.new /etc/ufw/before.rules

  ufw reload
  echo "  [OK] NAT rules active (WAN: $WAN_IF)"
NAT_APPLY

# ==============================================================================
# 3. ALL OTHER NODES — Apply base UFW rules via Puppet in parallel
# ==============================================================================
echo ""
echo "[+] Applying UFW base rules on all cluster nodes (parallel)..."

declare -A NODE_PIDS
ALL_NODES=(
    "${MONITOR_IP}:internal-monitor"
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
        echo "  [SKIP] $NODE_NAME not reachable."
    fi
done

for NODE_NAME in "${!NODE_PIDS[@]}"; do
    wait "${NODE_PIDS[$NODE_NAME]}" && echo "  [OK]  $NODE_NAME" || \
        echo "  [WARN] $NODE_NAME returned non-zero (check manually)"
done

# ==============================================================================
# 4. VERIFY
# ==============================================================================
echo ""
echo ">>> Verifying UFW state <<<"

echo "[+] ufw-router:"
ssh $SSH_OPTS root@"$ROUTER_IP" 'ufw status | head -5'

echo "[+] IP Forwarding on router:"
ssh $SSH_OPTS root@"$ROUTER_IP" 'sysctl net.ipv4.ip_forward'

echo ""
echo "════════════════════════════════════════════════════════════════"
echo "  Network Security (UFW) deployment complete (managed by Puppet)"
echo "  Router:  UFW active with NAT/DNAT and inter-zone routing."
echo "  Nodes:   Least-privilege per-role UFW policies applied."
echo "════════════════════════════════════════════════════════════════"
