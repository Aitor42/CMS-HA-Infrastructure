#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# DEPRECATED (v1, reference only). Superseded by the Go orchestrator (`cms-ha`)
# and by Ansible/Puppet. Kept for historical reference and NOT maintained: new
# work must target the v2 path (see scripts/README.md).
# ---------------------------------------------------------------------------
# Re-run the idempotent WordPress database/user initialization Job.
# Uses the existing mariadb-secret in Kubernetes; no MariaDB prompt or
# plaintext password argument is needed.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
source "${SCRIPT_DIR}/../config.sh"

MASTER_IP="${MASTER_IP:-${MASTER1_IP}}"
LOCAL_JOB="${PROJECT_DIR}/kubernetes/jobs/init-db-job.yaml"
REMOTE_JOB="/tmp/init-wordpress-db.yaml"

if [[ ! -f "${LOCAL_JOB}" ]]; then
  echo "[ERROR] No se encuentra ${LOCAL_JOB}" >&2
  exit 1
fi

echo "[+] Comprobando MariaDB y el Secret existente en ${MASTER_IP}..."
ssh ${SSH_OPTS} "root@${MASTER_IP}" \
  "kubectl get secret mariadb-secret -n cms >/dev/null && kubectl wait --for=condition=ready pod -l app=mariadb -n cms --timeout=300s"

echo "[+] Copiando el Job de inicialización..."
scp ${SSH_OPTS} "${LOCAL_JOB}" "root@${MASTER_IP}:${REMOTE_JOB}"

echo "[+] Ejecutando la inicialización idempotente de la base de datos..."
ssh ${SSH_OPTS} "root@${MASTER_IP}" bash -s -- "${REMOTE_JOB}" <<'REMOTE'
set -euo pipefail
job_file="$1"
trap 'rm -f "$job_file"' EXIT
kubectl delete job init-wordpress-db -n cms --ignore-not-found=true
kubectl apply -f "$job_file"
kubectl wait --for=condition=complete job/init-wordpress-db -n cms --timeout=300s
kubectl logs -n cms job/init-wordpress-db
REMOTE

echo "[OK] Base de datos y usuario de WordPress inicializados."
