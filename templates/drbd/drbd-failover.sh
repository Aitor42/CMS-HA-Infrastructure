#!/bin/bash
# drbd-failover.sh
#
# DRBD failover utility for the high-availability cluster.
# The watchdog invokes it to promote the survivor once the peer has been
# fenced; promote, demote and status can also be run by hand.
#
# Usage: drbd-failover.sh {promote|demote|status}

set -euo pipefail

RESOURCE="cms_data"
MOUNT_POINT="/mnt/data/mariadb"
DRBD_DEVICE="/dev/drbd0"

case "${1:-status}" in
  promote)
    echo "[FAILOVER] Promoting local node to Primary..."

    # Verify the resource is defined and active in the kernel
    if ! drbdadm status "$RESOURCE" > /dev/null 2>&1; then
      echo "  ✗ ERROR: DRBD resource '$RESOURCE' is not active on this node"
      echo "  Run first: drbdadm up $RESOURCE"
      exit 1
    fi

    # No --force here. The caller has already confirmed the peer is fenced,
    # but the local disk must still be UpToDate for DRBD to promote it.
    # Forcing an Outdated/Inconsistent disk would publish stale data as the
    # new authority; failing is the safe outcome, and the watchdog retries.
    drbdadm primary "$RESOURCE"
    echo "  ✔ Node promoted to Primary"

    mkdir -p "$MOUNT_POINT"
    if ! mountpoint -q "$MOUNT_POINT"; then
      mount "$DRBD_DEVICE" "$MOUNT_POINT"
      echo "  ✔ Filesystem mounted at $MOUNT_POINT"
    else
      echo "  ℹ $MOUNT_POINT was already mounted"
    fi

    # Label local node as the active primary and clear stale labels from every
    # other master.
    export KUBECONFIG="${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}"
    if command -v kubectl &>/dev/null && [ -f "$KUBECONFIG" ]; then
      echo "  [+] Updating availability labels in Kubernetes..."
      LOCAL_NODE=$(kubectl get nodes -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep "$(hostname)" | head -n 1 || hostname)
      if [ -n "$LOCAL_NODE" ]; then
        kubectl label node "$LOCAL_NODE" drbd-status=primary --overwrite || true
        # Clear the stale label from every other master, not just one peer:
        # with three control-plane nodes a singular "peer" can miss the old
        # primary and leave a secondary matching the MariaDB node affinity.
        for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep "master" | grep -vx "$LOCAL_NODE" || true); do
          kubectl label node "$node" drbd-status- 2>/dev/null || true
        done
      fi

      # Trigger pod reschedule to the new Primary node.
      #
      # A pod that lived on the failed node stays in Terminating until that
      # node's kubelet confirms the deletion, and a dead kubelet never does.
      # A StatefulSet must see its old pod object disappear before it creates
      # the replacement, so a graceful delete here leaves the database down
      # until the failed node comes back. The peer was fenced off before this
      # script ran, so nothing is writing the volume and a forced delete is
      # safe: it lets the StatefulSet recreate the pod on the new Primary.
      echo "  [+] Ensuring MariaDB runs on new Primary..."
      POD_NODE=$(kubectl get pod mariadb-0 -n cms -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)
      if [ -n "$POD_NODE" ] && [ "$POD_NODE" != "$LOCAL_NODE" ]; then
        kubectl delete pod mariadb-0 -n cms --force --grace-period=0 --request-timeout=10s 2>/dev/null || true
      fi
      kubectl scale statefulset mariadb -n cms --replicas=1 2>/dev/null || true
    fi

    echo "  ✔ Failover complete. This node is now the Primary."
    ;;

  demote)
    echo "[FAILOVER] Demoting local node to Secondary..."

    # The watchdog treats a local Primary as the active writer and would scale
    # MariaDB back to one replica while this function is trying to unmount.
    # Pause it for the duration; the trap restarts it even if a step fails.
    WATCHDOG_WAS_ACTIVE=0
    if systemctl is-active --quiet drbd-failover-watchdog.service 2>/dev/null; then
      WATCHDOG_WAS_ACTIVE=1
      systemctl stop drbd-failover-watchdog.service 2>/dev/null || true
    fi
    restart_watchdog() {
      if [ "$WATCHDOG_WAS_ACTIVE" = "1" ]; then
        systemctl start drbd-failover-watchdog.service 2>/dev/null || true
      fi
    }
    trap restart_watchdog EXIT

    # Scale down MariaDB pod before unmounting to release storage locks
    if command -v kubectl &>/dev/null; then
      echo "  [+] Scaling down MariaDB pod..."
      kubectl scale statefulset mariadb -n cms --replicas=0 2>/dev/null || true
      # A fixed sleep is not enough: the default termination grace period is
      # 30s, and unmounting while mariadbd still has the volume open fails.
      for _ in $(seq 1 30); do
        if ! kubectl get pod mariadb-0 -n cms >/dev/null 2>&1; then
          break
        fi
        sleep 2
      done
      echo "  [+] Removing Kubernetes labels..."
      LOCAL_NODE=$(kubectl get nodes -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep "$(hostname)" | head -n 1 || hostname)
      kubectl label node "$LOCAL_NODE" drbd-status- 2>/dev/null || true
    fi

    if mountpoint -q "$MOUNT_POINT" 2>/dev/null; then
      umount "$MOUNT_POINT"
      echo "  ✔ Volume unmounted from $MOUNT_POINT"
    fi

    drbdadm secondary "$RESOURCE"
    echo "  ✔ Resource degraded to Secondary"
    ;;

  status)
    echo "=== DRBD Status ==="
    drbdadm status "$RESOURCE" 2>/dev/null || cat /proc/drbd 2>/dev/null
    echo ""
    echo "=== Mount Point ==="
    if mountpoint -q "$MOUNT_POINT" 2>/dev/null; then
      echo "$MOUNT_POINT is currently mounted"
      df -h "$MOUNT_POINT"
    else
      echo "$MOUNT_POINT is NOT mounted"
    fi
    ;;

  *)
    echo "Usage: $0 {promote|demote|status}"
    echo ""
    echo "  promote  - Promotes the node to primary and mounts the partition"
    echo "  demote   - Unmounts the volume and degrades the node to secondary"
    echo "  status   - Shows the volume and mount point status"
    exit 1
    ;;
esac
