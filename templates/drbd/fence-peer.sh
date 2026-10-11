#!/bin/bash
# fence-peer.sh
#
# STONITH helper for the DRBD cms_data resource.
#
# Powers off the failed peer node at the hypervisor so that two nodes can never
# write the same replicated volume (split-brain). It exits 0 only when the peer
# domain is *positively confirmed* to be shut off; any other outcome is a
# failure and the caller must not promote.
#
# Usage:  fence-peer.sh [peer-domain]
# Config: /etc/drbd-fence.conf
#           FENCE_COMMAND        shell snippet that powers the peer off
#           FENCE_STATE_COMMAND  shell snippet that prints the peer's state
#           PEER_DOMAIN          default domain when no argument is given
#         In both snippets the literal {domain} is replaced with the peer's
#         libvirt domain name.
#
# Exit codes: 0 fenced (or already off) · 1 could not confirm off · 2 no
#             domain · 3 fencing not configured.

set -uo pipefail

CONF="/etc/drbd-fence.conf"
# shellcheck disable=SC1090
[ -r "$CONF" ] && . "$CONF"

DOMAIN="${1:-${PEER_DOMAIN:-}}"
FENCE_TIMEOUT="${FENCE_TIMEOUT:-20}"

log() { echo "[FENCE $(date '+%F %T')] $*"; }

# is_off: true when the hypervisor reports the domain as stopped or crashed.
is_off() {
  local state
  state="$(eval "${FENCE_STATE_COMMAND//"{domain}"/$DOMAIN}" 2>/dev/null || true)"
  echo "$state" | grep -qiE 'shut off|crash'
}

if [ -z "$DOMAIN" ]; then
  log "ERROR: no peer domain specified (set PEER_DOMAIN in $CONF or pass it as an argument)"
  exit 2
fi

if [ -z "${FENCE_COMMAND:-}" ] || [ -z "${FENCE_STATE_COMMAND:-}" ]; then
  log "ERROR: fencing is not configured (FENCE_COMMAND/FENCE_STATE_COMMAND empty in $CONF)"
  exit 3
fi

if is_off; then
  log "Peer domain '$DOMAIN' is already off; nothing to fence"
  exit 0
fi

log "Fencing peer domain '$DOMAIN'..."
if ! timeout "$FENCE_TIMEOUT" bash -c "${FENCE_COMMAND//"{domain}"/$DOMAIN}"; then
  log "WARNING: fence command returned non-zero; verifying state anyway"
fi

# Confirm the domain is actually off before declaring success.
for _ in 1 2 3 4 5 6 7 8 9 10; do
  if is_off; then
    log "Peer domain '$DOMAIN' confirmed OFF"
    exit 0
  fi
  sleep 1
done

log "ERROR: could not confirm '$DOMAIN' is off after fencing"
exit 1
