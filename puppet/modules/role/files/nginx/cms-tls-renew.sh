#!/usr/bin/env bash
#
# CMS-HA — load-balancer TLS renewal (step-ca).
#
# Managed by Puppet (role::loadbalancer). Runs on both load balancers from the
# `cms-tls-renew.timer` systemd timer.
#
# Initial issuance is performed by the Go PKI phase on the CA host, so no CA
# provisioner credential is ever stored here: `step ca renew` authenticates
# against the CA over mTLS using the certificate it is renewing.
set -euo pipefail

CERT=/etc/nginx/ssl/cms.crt
KEY=/etc/nginx/ssl/cms.key
# The PKI phase writes the CA endpoint it actually provisioned to this file.
# Keep the defaults for a balancer whose Puppet catalog ran before the PKI
# phase (or for a hand-run script), but never duplicate the configured values
# here: a different pki.domain/ca_port/ca_name used to leave renewal pointed at
# the old CA and the timer failing silently every hour.
PKI_ENV=/etc/cms-ha/pki.env
if [ -r "$PKI_ENV" ]; then
  # shellcheck disable=SC1090
  . "$PKI_ENV"
fi
CA_URL="${CMS_CA_URL:-https://ca.internal.local:8443}"
ROOT_CA="${CMS_ROOT_CA:-/usr/local/share/ca-certificates/cms_root_ca.crt}"
CA_NAME="${CMS_CA_NAME:-CMS Local CA}"
# Ask the CA for a renewal once fewer than this remains; the CA enforces the
# actual minimum against the provisioner's configured durations.
RENEW_WITHIN="720h"
# The e2e renewal drill sets RENEW_FORCE=1 to rotate the certificate on demand.
# step refuses an --expires-in value above the certificate lifetime ("must be
# within (lower than) the certificate validity period"), so forcing is the only
# deterministic way to exercise the CA path without waiting 60 days.
RENEW_ARGS=(--expires-in "$RENEW_WITHIN")
if [ "${RENEW_FORCE:-0}" = "1" ]; then
  RENEW_ARGS=(--force)
fi

log() { echo "cms-tls-renew: $*"; }

if ! command -v step >/dev/null 2>&1; then
  log "step CLI not installed; skipping"
  exit 0
fi

if [ ! -f "$CERT" ] || [ ! -f "$KEY" ]; then
  log "certificate not present; run the PKI phase to issue the initial certificate"
  exit 0
fi

if ! openssl x509 -in "$CERT" -noout -issuer 2>/dev/null | grep -Fq -- "$CA_NAME"; then
  log "certificate was not issued by '$CA_NAME' (self-signed bootstrap); run the PKI phase"
  exit 0
fi

out="$(step ca renew "${RENEW_ARGS[@]}" \
  --exec "systemctl reload nginx" \
  --ca-url "$CA_URL" --root "$ROOT_CA" \
  "$CERT" "$KEY" 2>&1)" && status=0 || status=$?
echo "$out"

# step exits 0 when the certificate is not due, so the "not renewed" check must
# come first: otherwise the timer would log a renewal that never happened.
if echo "$out" | grep -qi "not renewed"; then
  log "certificate not due for renewal yet"
  exit 0
fi

if [ "$status" -ne 0 ]; then
  log "renewal failed"
  exit "$status"
fi

log "certificate renewed and nginx reloaded"
