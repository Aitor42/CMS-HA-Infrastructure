#!/usr/bin/env bash
# cms-keepalived-metrics.sh [vip] — publish VIP ownership as node-exporter
# textfile metrics.
#
# The VIP comes from the first argument or CMS_VIP (set by
# /etc/default/cms-keepalived-metrics from the same Puppet parameter that feeds
# keepalived.conf). There is deliberately no built-in default: a wrong address
# would publish a permanent "nobody is MASTER" instead of surfacing the broken
# configuration.
#
# Runs from a systemd timer on both load balancers. The VRRP state is derived from whether
# the floating VIP is actually present on this host, which is what matters for
# routing: exactly one balancer must hold it. The series is always published so
# the KeepalivedState alert can tell "nobody is MASTER" (0) from "VRRP split"
# (more than one).
set -uo pipefail

VIP="${1:-${CMS_VIP:-}}"
if [[ -z "${VIP}" ]]; then
  echo "cms-keepalived-metrics: no VIP given; set CMS_VIP in /etc/default/cms-keepalived-metrics" >&2
  exit 1
fi
OUT_DIR="${CMS_TEXTFILE_DIR:-/var/lib/prometheus/node-exporter}"
OUT="${OUT_DIR}/cms_keepalived.prom"
TMP="${OUT}.$$"

mkdir -p "${OUT_DIR}" 2>/dev/null || exit 0

state="backup"
present=0
if command -v ip >/dev/null 2>&1 && ip -4 addr show 2>/dev/null | grep -qw "${VIP}"; then
  state="master"
  present=1
fi

{
  echo "# HELP cms_keepalived_state VRRP state of this balancer, as a 1-hot label set."
  echo "# TYPE cms_keepalived_state gauge"
  echo "cms_keepalived_state{state=\"master\"} $([ "${state}" = master ] && echo 1 || echo 0)"
  echo "cms_keepalived_state{state=\"backup\"} $([ "${state}" = backup ] && echo 1 || echo 0)"
  echo "# HELP cms_keepalived_vip_present Floating VIP is held by this node (1) or not (0)."
  echo "# TYPE cms_keepalived_vip_present gauge"
  echo "cms_keepalived_vip_present ${present}"
} > "${TMP}" && mv -f "${TMP}" "${OUT}"
