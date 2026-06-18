#!/bin/bash
# drbd-failover.sh
#
# Manual DRBD failover utility for the high-availability cluster.
# Enables hot-standby promotion when the primary master node goes down.
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

    # Stop K3s to avoid mount point locking issues
    echo "  [+] Stopping K3s temporarily..."
    systemctl stop k3s || true

    # Promote device to Primary
    drbdadm primary "$RESOURCE"
    echo "  ✔ Node promoted to Primary"

    # Mount the replicated device
    mkdir -p "$MOUNT_POINT"
    if ! mountpoint -q "$MOUNT_POINT"; then
      mount "$DRBD_DEVICE" "$MOUNT_POINT"
      echo "  ✔ Filesystem mounted at $MOUNT_POINT"
    else
      echo "  ℹ $MOUNT_POINT was already mounted"
    fi

    # Label local node in Kubernetes as active primary and remove label from peer
    if command -v kubectl &>/dev/null; then
      echo "  [+] Updating availability labels in Kubernetes..."
      HOSTNAME_K8S=$(hostname)
      OTHER_NODE="internal-master2"
      if [[ "$HOSTNAME_K8S" == "internal-master2" ]]; then
        OTHER_NODE="internal-master1"
      fi
      kubectl label node "$HOSTNAME_K8S" drbd-status=primary --overwrite || true
      kubectl label node "$OTHER_NODE" drbd-status- 2>/dev/null || true
    fi

    # Start Kubernetes again
    echo "  [+] Restarting K3s..."
    systemctl start k3s || true

    echo "  ✔ Failover complete. This node is now the Primary."
    ;;

  demote)
    echo "[FAILOVER] Demoting local node to Secondary..."

    # Stop K3s to release open file descriptors on the volume
    echo "  [+] Stopping K3s..."
    systemctl stop k3s || true

    # Unmount the volume
    if mountpoint -q "$MOUNT_POINT" 2>/dev/null; then
      umount "$MOUNT_POINT"
      echo "  ✔ Volume unmounted from $MOUNT_POINT"
    fi

    # Remove node label from the cluster
    if command -v kubectl &>/dev/null; then
      echo "  [+] Removing Kubernetes labels..."
      kubectl label node $(hostname) drbd-status- 2>/dev/null || true
    fi

    # Degrade resource to Secondary
    drbdadm secondary "$RESOURCE"
    echo "  ✔ Resource degraded to Secondary"

    # Restart Kubernetes
    echo "  [+] Restarting K3s..."
    systemctl start k3s || true
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
