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
export KUBECONFIG="${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}"

storage_is_ready() {
  local source fstype options root record actual expected
  [ -b "$DRBD_DEVICE" ] || return 1
  record=$(findmnt -rn -M "$MOUNT_POINT" -o SOURCE,FSTYPE,OPTIONS,FSROOT) || return 1
  [[ "$record" != *$'\n'* ]] || return 1
  read -r source fstype options root <<< "$record"
  actual=$(readlink -f "$source") || return 1
  expected=$(readlink -f "$DRBD_DEVICE") || return 1
  [ -n "$expected" ] && [ "$actual" = "$expected" ] &&
    [ "$fstype" = "ext4" ] && [ "$root" = "/" ] && [[ ",$options," = *,rw,* ]]
}

withdraw_storage() {
  local node role label="" pod_node=""
  echo "  ✗ Promotion failed; withdrawing storage availability."
  if command -v kubectl &>/dev/null && [ -f "$KUBECONFIG" ]; then
    node=$(local_k8s_node) || true
    if [ -n "$node" ]; then
      label=$(kubectl get node "$node" --request-timeout=3s -o jsonpath='{.metadata.labels.drbd-status}' 2>/dev/null || true)
      pod_node=$(kubectl get pod mariadb-0 -n cms --request-timeout=3s -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)
      kubectl label node "$node" drbd-status- --request-timeout=3s || echo "ERROR: failed to remove primary label"
    fi
    role=$(drbdadm role "$RESOURCE" 2>/dev/null | cut -d/ -f1) || true
    # A rejected promotion on a passive node must not stop a healthy peer DB.
    if [ "$role" = "Primary" ] || [ "$label" = "primary" ] || { [ -n "$pod_node" ] && [ "$pod_node" = "$node" ]; }; then
      kubectl scale statefulset mariadb -n cms --replicas=0 --request-timeout=3s || echo "ERROR: failed to stop MariaDB"
    fi
  else
    echo "ERROR: Kubernetes unavailable; cannot withdraw labels or stop MariaDB"
  fi
}

# Resolve this host's Kubernetes node object. Node names are usually the VM's
# short hostname but may be the FQDN; compare the first label so that
# "internal-master1" never matches "internal-master10". Prints nothing when the
# host is not registered, and the callers skip label/pod maintenance.
local_k8s_node() {
  local short="${HOSTNAME%%.*}"
  kubectl get nodes --request-timeout=3s -o jsonpath='{.items[*].metadata.name}' 2>/dev/null \
    | tr ' ' '\n' \
    | awk -v h="$short" 'NF { n=$0; sub(/\..*/, "", n); if (n == h) { print $0; exit } }'
}

case "${1:-status}" in
  promote)
    trap 'if [ "$?" -ne 0 ]; then withdraw_storage; fi' EXIT
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

    if ! storage_is_ready; then
      # Clear stale availability before attempting recovery. Do not overmount
      # another device, a read-only volume or a bind-mounted subdirectory.
      withdraw_storage
      if ! mountpoint -q "$MOUNT_POINT" && [ -b "$DRBD_DEVICE" ]; then
        mkdir -p "$MOUNT_POINT"
        mount -t ext4 -o rw "$DRBD_DEVICE" "$MOUNT_POINT"
      fi
      if ! storage_is_ready; then
        echo "  ✗ ERROR: expected writable DRBD filesystem is not mounted"
        exit 1
      fi
    fi
    echo "  ✔ Verified DRBD filesystem mounted at $MOUNT_POINT"

    # Label local node as the active primary and clear stale labels from every
    # other master.
    export KUBECONFIG="${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}"
    if command -v kubectl &>/dev/null && [ -f "$KUBECONFIG" ]; then
      echo "  [+] Updating availability labels in Kubernetes..."
      LOCAL_NODE=$(local_k8s_node)
      if [ -z "$LOCAL_NODE" ]; then
        echo "  ! $(hostname) is not registered as a Kubernetes node yet; skipping label and pod reschedule."
      else
        kubectl label node "$LOCAL_NODE" drbd-status=primary --overwrite || true
        # Clear the stale label from every node that still advertises itself
        # as the primary, not just from nodes whose name contains "master":
        # a custom-named control-plane node would otherwise keep the label and
        # match the MariaDB node affinity alongside the new Primary.
        for node in $(kubectl get nodes -l drbd-status=primary -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -vx "$LOCAL_NODE" || true); do
          kubectl label node "$node" drbd-status- 2>/dev/null || true
        done

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
      LOCAL_NODE=$(local_k8s_node)
      if [ -n "$LOCAL_NODE" ]; then
        kubectl label node "$LOCAL_NODE" drbd-status- 2>/dev/null || true
      fi
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
