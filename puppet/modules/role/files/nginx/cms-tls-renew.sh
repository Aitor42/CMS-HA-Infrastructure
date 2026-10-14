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
CA_URL="https://ca.internal.local:8443"
ROOT_CA=/usr/local/share/ca-certificates/cms_root_ca.crt
CA_NAME="CMS Local CA"
# Ask the CA for a renewal once fewer than this remains; the CA enforces the
# actual minimum against the provisioner's configured durations.
RENEW_WITHIN="720h"

log() { echo "cms-tls-renew: $*"; }

if ! command -v step >/dev/null 2>&1; then
  log "step CLI not installed; skipping"
  exit 0
fi

if [ ! -f "$CERT" ] || [ ! -f "$KEY" ]; then
  log "certificate not present; run the PKI phase to issue the initial certificate"
  exit 0
fi

if ! openssl x509 -in "$CERT" -noout -issuer 2>/dev/null | grep -q "$CA_NAME"; then
  log "certificate was not issued by '$CA_NAME' (self-signed bootstrap); run the PKI phase"
  exit 0
fi

out="$(step ca renew --expires-in "$RENEW_WITHIN" \
  --exec "systemctl reload nginx" \
  --ca-url "$CA_URL" --root "$ROOT_CA" \
  "$CERT" "$KEY" 2>&1)" && status=0 || status=$?
echo "$out"

if [ "$status" -ne 0 ]; then
  if echo "$out" | grep -qi "not renewed"; then
    log "certificate not due for renewal yet"
    exit 0
  fi
  log "renewal failed"
  exit "$status"
fi

log "certificate renewed and nginx reloaded"
