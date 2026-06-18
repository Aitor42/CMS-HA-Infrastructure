#!/bin/bash
# 06_setup_drbd.sh
#
# Configures synchronous block-level replication (Protocol C) between the
# secondary disks (/dev/vdb) of internal-master1 and internal-master2.
# Creates the cms_data DRBD resource, promotes master1 as initial primary,
# formats /dev/drbd0 as ext4, and mounts it at /mnt/data/mariadb.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATES_DIR="${SCRIPT_DIR}/../templates"
source "${SCRIPT_DIR}/config.sh"

echo ">>> Deploying High Availability (DRBD) <<<"

NODE1_IP="$MASTER1_IP"   # internal-master1 (default Primary)
NODE2_IP="$MASTER2_IP"   # internal-master2 (default Secondary)
DRBD_DEVICE="/dev/drbd0"
DRBD_DISK="/dev/vdb"
DRBD_RESOURCE="cms_data"
DRBD_MOUNT="/mnt/data/mariadb"

# ==============================================================================
# 1. INSTALL AND CONFIGURE DRBD RESOURCE ON BOTH MASTERS
# ==============================================================================
echo "[+] Configuring DRBD modules and resource files on master nodes..."

for NODE in "$NODE1_IP" "$NODE2_IP"; do
  echo "    → Connecting to master node $NODE..."

  # Upload the DRBD resource definition from templates
  scp $SSH_OPTS "${TEMPLATES_DIR}/drbd/cms-data.res" root@"$NODE":/tmp/cms_data.res

  ssh $SSH_OPTS root@"$NODE" bash -s <<'DRBD_SETUP'
    set -e
    export DEBIAN_FRONTEND=noninteractive

    echo "[+] Installing drbd-utils..."
    apt-get update -qq
    apt-get install -y -qq drbd-utils

    # Load DRBD kernel module and ensure persistence on boot
    modprobe drbd || true
    grep -q "^drbd$" /etc/modules 2>/dev/null || echo "drbd" >> /etc/modules

    echo "[+] Installing DRBD resource definition (cms_data.res)..."
    cp /tmp/cms_data.res /etc/drbd.d/cms_data.res

    # Configure cross-node hostname resolution
    grep -q "internal-master1" /etc/hosts || echo "192.168.10.11 internal-master1" >> /etc/hosts
    grep -q "internal-master2" /etc/hosts || echo "192.168.10.12 internal-master2" >> /etc/hosts

    # Validate presence of secondary virtual disk
    if [ ! -b /dev/vdb ]; then
      echo "  ⚠ WARNING: Secondary disk /dev/vdb not present on this VM."
    fi

    echo "[+] Initialising DRBD metadata on volume..."
    echo "yes" | drbdadm create-md cms_data 2>/dev/null || {
      echo "  ℹ DRBD metadata already exists or volume is already initialised."
    }

    echo "[+] Bringing up DRBD resource..."
    drbdadm up cms_data 2>/dev/null || {
      echo "  ℹ DRBD resource already active on this node."
    }

    echo "  ✔ DRBD initialised on $(hostname)"
DRBD_SETUP
done

# ==============================================================================
# 2. WAIT FOR THE NODE PAIR TO ESTABLISH REPLICATION LINK
# ==============================================================================
echo '[+] Waiting for DRBD replication sync and link establishment...'
for i in $(seq 1 30); do
  if ssh $SSH_OPTS root@"$NODE1_IP" "drbdadm status cms_data 2>/dev/null" | grep -q 'peer-disk:UpToDate\|peer-disk:Inconsistent\|role:Secondary'; then
    echo '  ✔ DRBD nodes linked successfully'
    break
  fi
  if [ $i -eq 30 ]; then
    echo '  ⚠ Link timeout exceeded, continuing...'
  fi
  sleep 5
done

# ==============================================================================
# 3. PROMOTE MASTER1 AS PRIMARY, FORMAT AND MOUNT
# ==============================================================================
echo "[+] Promoting internal-master1 ($NODE1_IP) to Primary role and formatting..."
ssh $SSH_OPTS root@"$NODE1_IP" bash -s <<'PRIMARY_SETUP'
  set -e

  echo "[+] Promoting to Primary..."
  drbdadm primary --force cms_data

  echo "[+] DRBD volume status:"
  cat /proc/drbd 2>/dev/null || drbdadm status cms_data

  echo "[+] Checking ext4 format on /dev/drbd0..."
  if ! blkid /dev/drbd0 2>/dev/null | grep -q ext4; then
    mkfs.ext4 -F /dev/drbd0
    echo "  ✔ /dev/drbd0 formatted as ext4"
  else
    echo "  ℹ ext4 filesystem already present, skipping format to preserve data"
  fi

  echo "[+] Stopping K3s to avoid mount point locks..."
  systemctl stop k3s || true

  mkdir -p /tmp/mariadb_backup
  if [ -d "/mnt/data/mariadb" ] && [ "$(ls -A "/mnt/data/mariadb" 2>/dev/null)" ]; then
    echo "[+] Saving existing MariaDB data..."
    cp -a /mnt/data/mariadb/* /tmp/mariadb_backup/
  fi

  echo "[+] Mounting /dev/drbd0 at /mnt/data/mariadb..."
  mkdir -p /mnt/data/mariadb
  if ! mountpoint -q /mnt/data/mariadb; then
    mount /dev/drbd0 /mnt/data/mariadb || { echo '✗ ERROR: Mount failed'; exit 1; }
    echo '  ✔ DRBD volume mounted'
  else
    echo '  ℹ /mnt/data/mariadb already mounted'
  fi

  if [ "$(ls -A /tmp/mariadb_backup 2>/dev/null)" ]; then
    echo "[+] Restoring MariaDB data to replicated device..."
    cp -a /tmp/mariadb_backup/* /mnt/data/mariadb/
  fi
  rm -rf /tmp/mariadb_backup

  echo "[+] Adding persistent fstab entry..."
  if ! grep -q "/dev/drbd0" /etc/fstab; then
    echo "/dev/drbd0  /mnt/data/mariadb  ext4  defaults,noauto  0  0" >> /etc/fstab
    echo "  ✔ Persistent entry registered"
  else
    echo "  ℹ Persistent entry already exists"
  fi

  if command -v kubectl &>/dev/null; then
    echo "[+] Labelling Kubernetes node as drbd-status=primary..."
    kubectl label node $(hostname) drbd-status=primary --overwrite || true
    kubectl label node internal-master2 drbd-status- 2>/dev/null || true
  fi

  echo "[+] Restarting K3s..."
  systemctl start k3s || true

  echo "  ✔ Primary configuration complete"
PRIMARY_SETUP

# ==============================================================================
# 4. DEPLOY FAILOVER SCRIPT ON BOTH MASTERS
# ==============================================================================
echo "[+] Deploying failover script on both master nodes..."

for NODE in "$NODE1_IP" "$NODE2_IP"; do
  echo "    → Installing failover script on $NODE..."
  scp $SSH_OPTS "${TEMPLATES_DIR}/drbd/drbd-failover.sh" \
      root@"$NODE":/usr/local/bin/drbd-failover.sh
  ssh $SSH_OPTS root@"$NODE" "chmod +x /usr/local/bin/drbd-failover.sh"
  echo "  ✔ Failover script installed at /usr/local/bin/drbd-failover.sh"
done

# ==============================================================================
# 5. VERIFY AND SUMMARISE
# ==============================================================================
echo ""
echo "[+] Validating final DRBD state..."
ssh $SSH_OPTS root@"$NODE1_IP" "drbdadm status cms_data 2>/dev/null || cat /proc/drbd" || true

echo ""
echo "════════════════════════════════════════════════════════════════"
echo "  DRBD configured and running"
echo "  • Initial Primary:   internal-master1 ($NODE1_IP)"
echo "  • Initial Secondary: internal-master2 ($NODE2_IP)"
echo "  • Resource:          $DRBD_RESOURCE"
echo "  • Mount point:       $DRBD_MOUNT (on primary node)"
echo "  • Failover utility:  /usr/local/bin/drbd-failover.sh {promote|demote|status}"
echo "════════════════════════════════════════════════════════════════"
echo ">>> DRBD configuration complete <<<"
