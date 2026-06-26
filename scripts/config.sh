#!/bin/bash
# config.sh
#
# Centralised environment variables, IP addressing, database credentials,
# and utility functions for the CMS high-availability infrastructure.
#
# All variables can be overridden via external environment variables.
# Example: DB_PASS="AnotherPassword" bash scripts/07_setup_nginx_wordpress.sh
#
# This file is source-safe: it only defines and exports variables and
# functions without performing any direct execution.

# ── Paths and SSH Settings ────────────────────────────────────────────────────
# Storage directory for VM virtual disk images (.qcow2)
export VM_DIR="${VM_DIR:-$HOME/vm_storage}"

# SSH private key used to manage the cluster
export HOST_KEY_FILE="${HOST_KEY_FILE:-$HOME/.ssh/id_ed25519_gar}"

# Common SSH parameters for non-interactive connections to VMs
export SSH_OPTS="-i ${HOST_KEY_FILE} -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -o BatchMode=yes"

# ── Libvirt Connection URI ────────────────────────────────────────────────────
# KVM/QEMU hypervisor access mode (system by default)
export LIBVIRT_DEFAULT_URI="${LIBVIRT_DEFAULT_URI:-qemu:///system}"

# ── Internal Network Addressing (192.168.10.0/24) ─────────────────────────────
# Reserved IPs for control plane, databases, and monitoring
export ROUTER_IP="192.168.10.1"        # Router/firewall internal interface
export JUMPSTART_IP="192.168.10.10"    # Cobbler + Puppet Server
export MASTER1_IP="192.168.10.11"      # First K3s master node (K3s Server)
export MASTER2_IP="192.168.10.12"      # Second K3s master node (K3s Server)
export WORKER1_IP="192.168.10.13"      # First K3s worker node (K3s Agent)
export WORKER2_IP="192.168.10.14"      # Second K3s worker node (K3s Agent)
export STORAGE_IP="192.168.10.15"      # Centralised storage server
export MONITOR_IP="192.168.10.20"      # Prometheus + Grafana monitoring server

# ── Client Network Addressing (Main - 192.168.20.0/24) ───────────────────────
# User-facing frontend and load balancer IPs
export JUMPSTART_MAIN_IP="192.168.20.10" # Jumpstart secondary interface on main network
export LB_IP="192.168.20.100"            # Nginx load balancer
export CMS1_IP="192.168.20.101"          # WordPress frontend 1 (Apache)
export CMS2_IP="192.168.20.102"          # WordPress frontend 2 (Apache)

# ── Dynamic Hot-desk Workstation Configuration ────────────────────────────────
export NUM_HOTDESKS="${NUM_HOTDESKS:-3}" # Default number of hot-desks to deploy
export MAX_HOTDESKS=8                    # Maximum allowed by IP address range

# ── CMS Database Credentials and Parameters ──────────────────────────────────
export DB_PASS="${DB_PASS:-WpS3cur3P4ss!}" # WordPress password for MariaDB
export DB_NAME="${DB_NAME:-wordpress}"     # Database schema name
export DB_USER="${DB_USER:-wp_user}"       # WordPress connection user
export DB_HOST="${DB_HOST:-${MASTER1_IP}}" # Master node IP for MariaDB access
export DB_PORT="${DB_PORT:-30306}"         # NodePort exposed by the K3s cluster

# ── Monitoring Exporter Versions (Prometheus) ─────────────────────────────────
export NGINX_EXPORTER_VERSION="${NGINX_EXPORTER_VERSION:-1.4.1}"
export APACHE_EXPORTER_VERSION="${APACHE_EXPORTER_VERSION:-1.0.9}"
export MYSQLD_EXPORTER_VERSION="${MYSQLD_EXPORTER_VERSION:-0.16.0}"

# ── Grouped Node Lists (for iteration and deployment loops) ──────────────────
ALL_INTERNAL_NODES=("$JUMPSTART_IP" "$MASTER1_IP" "$MASTER2_IP" "$WORKER1_IP" "$WORKER2_IP" "$STORAGE_IP" "$MONITOR_IP")
ALL_MAIN_NODES=("$LB_IP" "$CMS1_IP" "$CMS2_IP")
ALL_NODES=("${ALL_INTERNAL_NODES[@]}" "${ALL_MAIN_NODES[@]}")

# ── Console Output Colour Configuration ──────────────────────────────────────
export RED='\033[0;31m'
export GREEN='\033[0;32m'
export YELLOW='\033[1;33m'
export BLUE='\033[0;34m'
export NC='\033[0m'

# ── Status Reporting Utility Functions ────────────────────────────────────────
info()    { echo -e "${BLUE}[INFO]${NC} $*"; }
success() { echo -e "${GREEN}[OK]${NC} $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC} $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*" >&2; }
