#!/bin/bash
# drbd-watchdog.sh
#
# Automated DRBD Failover Watchdog Daemon.
# Continuously monitors DRBD replication and peer master node health.
# Automatically promotes the local node to Primary and reschedules MariaDB
# when the peer master goes down, with anti-split-brain quorum verification.
#
# Architecture & Split-Brain Prevention:
#   1. Quorum/Gateway check: Never promote if the default gateway is unreachable
#      (prevents failover when local node is isolated from the network).
#   2. Peer ping check: Never promote if the peer host is alive and responding
#      to ICMP (prevents false positives during transient DRBD link flaps).
#   3. Strike threshold: Requires N consecutive failures (default: 3 checks = 15s)
#      before triggering failover to absorb network jitter.
#   4. Active Primary maintenance: Automatically mounts /mnt/data/mariadb,
#      ensures the Kubernetes drbd-status=primary label is active on the Primary,
#      and ensures MariaDB pod runs on the active storage node.
#   5. Passive Secondary maintenance: Ensures /mnt/data/mariadb is unmounted and
#      drbd-status label is cleared from Secondary nodes.

set -uo pipefail

RESOURCE="cms_data"
MOUNT_POINT="/mnt/data/mariadb"
DRBD_DEVICE="/dev/drbd0"
CHECK_INTERVAL="${CHECK_INTERVAL:-5}"
FAIL_THRESHOLD="${FAIL_THRESHOLD:-3}"
FAILOVER_SCRIPT="/usr/local/bin/drbd-failover.sh"
export KUBECONFIG="${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}"

GATEWAY="${GATEWAY:-$(ip route show default 2>/dev/null | awk '{print $3}' | head -n1 || echo "192.168.10.1")}"
STRIKE_COUNT=0

log() {
  echo "[DRBD-WATCHDOG $(date '+%Y-%m-%d %H:%M:%S')] $*"
}

get_peer_ip() {
  local peer=""
  for ip in $(grep -oP "address \K[0-9.]+" /etc/drbd.d/cms_data.res 2>/dev/null); do
    if ! ip -4 addr show | grep -q "$ip"; then
      peer="$ip"
      break
    fi
  done
  if [ -z "$peer" ]; then
    if [[ "$(hostname)" == *"master1"* ]]; then
      peer="192.168.10.12"
    else
      peer="192.168.10.11"
    fi
  fi
  echo "$peer"
}

log "Starting DRBD Failover Watchdog (Resource: $RESOURCE, Interval: ${CHECK_INTERVAL}s, Threshold: $FAIL_THRESHOLD strikes)"
PEER_IP=$(get_peer_ip)
log "Discovered Peer Master IP: $PEER_IP, Default Gateway: $GATEWAY"

while true; do
  # 1. Ensure DRBD module and resource are loaded
  drbdadm up "$RESOURCE" 2>/dev/null || true

  # 2. Query local role and connection state
  LOCAL_ROLE=$(drbdadm role "$RESOURCE" 2>/dev/null | cut -d'/' -f1 || echo "Unknown")
  CSTATE=$(drbdadm cstate "$RESOURCE" 2>/dev/null || echo "Unknown")

  # ──────────────────────────────────────────────────────────────────────────
  # CASE 1: Local node is PRIMARY
  # ──────────────────────────────────────────────────────────────────────────
  if [ "$LOCAL_ROLE" = "Primary" ]; then
    STRIKE_COUNT=0

    # Ensure storage volume is mounted
    mkdir -p "$MOUNT_POINT"
    if ! mountpoint -q "$MOUNT_POINT"; then
      log "Primary storage not mounted. Mounting $DRBD_DEVICE at $MOUNT_POINT..."
      mount "$DRBD_DEVICE" "$MOUNT_POINT" 2>/dev/null || true
    fi

    # Ensure Kubernetes label and pod placement are consistent
    if command -v kubectl &>/dev/null && [ -f "$KUBECONFIG" ] && kubectl get nodes --request-timeout=3s &>/dev/null; then
      LOCAL_NODE=$(kubectl get nodes --request-timeout=3s -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep "$(hostname)" | head -n 1 || hostname)
      CURRENT_LABEL=$(kubectl get node "$LOCAL_NODE" --request-timeout=3s -o jsonpath='{.metadata.labels.drbd-status}' 2>/dev/null || true)
      if [ "$CURRENT_LABEL" != "primary" ]; then
        log "Applying missing drbd-status=primary label to $LOCAL_NODE in Kubernetes..."
        kubectl label node "$LOCAL_NODE" drbd-status=primary --overwrite --request-timeout=3s 2>/dev/null || true
      fi

      # Verify MariaDB pod is scheduled on this node
      CURRENT_POD_NODE=$(kubectl get pod mariadb-0 -n cms --request-timeout=3s -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)
      if [ -n "$CURRENT_POD_NODE" ] && [ "$CURRENT_POD_NODE" != "$LOCAL_NODE" ]; then
        log "MariaDB pod currently scheduled on non-primary node ($CURRENT_POD_NODE). Rescheduling to $LOCAL_NODE..."
        kubectl delete pod mariadb-0 -n cms --request-timeout=3s 2>/dev/null || true
      fi
    fi

    sleep "$CHECK_INTERVAL"
    continue
  fi

  # ──────────────────────────────────────────────────────────────────────────
  # CASE 2: Local node is SECONDARY
  # ──────────────────────────────────────────────────────────────────────────
  if [ "$LOCAL_ROLE" = "Secondary" ]; then
    # Secondary cleanup: Ensure storage is NOT mounted locally
    if mountpoint -q "$MOUNT_POINT" 2>/dev/null; then
      log "Secondary node has volume mounted. Unmounting $MOUNT_POINT..."
      umount "$MOUNT_POINT" 2>/dev/null || true
    fi

    # Secondary cleanup: Ensure primary label is removed in Kubernetes
    if command -v kubectl &>/dev/null && [ -f "$KUBECONFIG" ] && kubectl get nodes --request-timeout=3s &>/dev/null; then
      LOCAL_NODE=$(kubectl get nodes --request-timeout=3s -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep "$(hostname)" | head -n 1 || hostname)
      CURRENT_LABEL=$(kubectl get node "$LOCAL_NODE" --request-timeout=3s -o jsonpath='{.metadata.labels.drbd-status}' 2>/dev/null || true)
      if [ "$CURRENT_LABEL" = "primary" ]; then
        log "Local node is Secondary but has primary label in Kubernetes. Removing label..."
        kubectl label node "$LOCAL_NODE" drbd-status- --request-timeout=3s 2>/dev/null || true
      fi
    fi

    # If connection state is healthy, reset counter and continue
    case "$CSTATE" in
      Connected|SyncSource|SyncTarget|Established|VerifyS|VerifyT)
        if [ $STRIKE_COUNT -gt 0 ]; then
          log "DRBD connection recovered to healthy state ($CSTATE). Resetting strike counter."
        fi
        STRIKE_COUNT=0
        sleep "$CHECK_INTERVAL"
        continue
        ;;
    esac

    # CSTATE is disconnected (Connecting, StandAlone, Unconnected, etc.)
    # Perform Anti-Split-Brain Quorum Verification:

    # Quorum Check 1: Can we reach the default gateway?
    if ! ping -c 1 -W 2 "$GATEWAY" &>/dev/null; then
      log "WARNING: DRBD disconnected ($CSTATE), but gateway $GATEWAY is unreachable. Local node is network-isolated; suppressing failover to prevent split-brain."
      STRIKE_COUNT=0
      sleep "$CHECK_INTERVAL"
      continue
    fi

    # Quorum Check 2: Can we reach the peer master?
    if [ -z "$PEER_IP" ]; then
      PEER_IP=$(get_peer_ip)
    fi

    if [ -n "$PEER_IP" ] && ping -c 1 -W 2 "$PEER_IP" &>/dev/null; then
      # Peer host is responsive to ICMP.
      # The peer machine is not dead; DRBD might be restarting or syncing.
      if [ $STRIKE_COUNT -gt 0 ]; then
        log "Peer $PEER_IP is reachable via ICMP. DRBD cstate is $CSTATE. Resetting strike counter."
      fi
      STRIKE_COUNT=0
      sleep "$CHECK_INTERVAL"
      continue
    fi

    # Peer is UNREACHABLE and Gateway is REACHABLE -> Primary is down!
    STRIKE_COUNT=$((STRIKE_COUNT + 1))
    log "ALERT: Peer $PEER_IP unreachable! DRBD cstate: $CSTATE. Strike $STRIKE_COUNT of $FAIL_THRESHOLD"

    if [ $STRIKE_COUNT -ge $FAIL_THRESHOLD ]; then
      log "CRITICAL: Peer $PEER_IP confirmed DEAD after $STRIKE_COUNT consecutive failed checks. Executing automatic failover promotion!"
      if [ -x "$FAILOVER_SCRIPT" ]; then
        "$FAILOVER_SCRIPT" promote || log "ERROR: Failover promotion script exited with error $?"
      else
        log "ERROR: Failover script $FAILOVER_SCRIPT not found or not executable!"
      fi
      STRIKE_COUNT=0
      NEW_ROLE=$(drbdadm role "$RESOURCE" 2>/dev/null | cut -d'/' -f1 || echo "Unknown")
      log "Failover promotion execution finished. New local role: $NEW_ROLE"
    fi

    sleep "$CHECK_INTERVAL"
    continue
  fi

  # ──────────────────────────────────────────────────────────────────────────
  # CASE 3: Role is unknown or uninitialized
  # ──────────────────────────────────────────────────────────────────────────
  log "DRBD resource in unexpected state: role=$LOCAL_ROLE, cstate=$CSTATE. Attempting drbdadm up..."
  drbdadm up "$RESOURCE" 2>/dev/null || true
  sleep "$CHECK_INTERVAL"
done
