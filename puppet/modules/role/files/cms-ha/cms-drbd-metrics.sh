#!/usr/bin/env bash
# cms-drbd-metrics.sh — publish DRBD health as node-exporter textfile metrics.
#
# Runs from a systemd timer on the DRBD nodes (internal-master1/2). It writes an atomic
# .prom file that node_exporter's textfile collector exposes, so DRBD can be
# alerted on without deploying a dedicated DRBD exporter.
#
# On a node where the resource is not configured (e.g. internal-master3, or
# before the DRBD phase has run) it emits nothing, so the DRBD alerts only fire
# where DRBD is actually expected.
set -uo pipefail

OUT_DIR="${CMS_TEXTFILE_DIR:-/var/lib/prometheus/node-exporter}"
RESOURCE="${CMS_DRBD_RESOURCE:-cms_data}"
OUT="${OUT_DIR}/cms_drbd.prom"
TMP="${OUT}.$$"

remove_stale() { rm -f "${OUT}" "${TMP}"; }

mkdir -p "${OUT_DIR}" 2>/dev/null || exit 0

command -v drbdadm >/dev/null 2>&1 || { remove_stale; exit 0; }

status="$(drbdadm status "${RESOURCE}" 2>/dev/null)" || { remove_stale; exit 0; }
case "${status}" in
  ""|*"No currently configured DRBD"*) remove_stale; exit 0 ;;
esac

connected=0
primary=0
up_to_date=0
split_brain=0

case "${status}" in
  *"role:Primary"*) primary=1 ;;
esac
case "${status}" in
  *StandAlone*|*WFConnection*|*SplitBrain*|*"split-brain"*) split_brain=1 ;;
esac
case "${status}" in
  *"peer-disk:UpToDate"*|*"replication:Established"*|*Connected*) connected=1 ;;
esac
case "${status}" in
  *"disk:UpToDate"*) up_to_date=1 ;;
esac

{
  echo "# HELP cms_drbd_connected DRBD peer replication link is up (1) or not (0)."
  echo "# TYPE cms_drbd_connected gauge"
  echo "cms_drbd_connected ${connected}"
  echo "# HELP cms_drbd_primary Node holds the DRBD Primary role (1) or not (0)."
  echo "# TYPE cms_drbd_primary gauge"
  echo "cms_drbd_primary ${primary}"
  echo "# HELP cms_drbd_disk_up_to_date Local DRBD disk state is UpToDate (1) or not (0)."
  echo "# TYPE cms_drbd_disk_up_to_date gauge"
  echo "cms_drbd_disk_up_to_date ${up_to_date}"
  echo "# HELP cms_drbd_split_brain Resource is StandAlone/WFConnection (1) or healthy (0)."
  echo "# TYPE cms_drbd_split_brain gauge"
  echo "cms_drbd_split_brain ${split_brain}"
} > "${TMP}" && mv -f "${TMP}" "${OUT}"
