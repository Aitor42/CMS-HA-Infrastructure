#!/bin/bash
# drbd-boot.sh
#
# Boot-time role resolution for the replicated cms_data volume.
#
# A node that boots may still carry "Primary" in its DRBD metadata: a power
# cut, a hypervisor crash or the fence action never gave it the chance to
# demote itself. If the peer took over while this node was down, attaching as
# Primary is a split brain: after-sb-2pri (disconnect) leaves both sides
# disconnected with two writers, and the watchdog on each side would mount the
# volume and claim the drbd-status=primary label for itself.
#
# The node that just booted is the stale one by definition, so it stands down
# and reconnects; with after-sb-1pri (discard-secondary) the peer streams its
# state over this disk. Only then is a promotion attempted, and DRBD refuses it
# while the peer is Primary.
#
# PROMOTE=1 (internal-master1) promotes when it is safe; PROMOTE=0
# (internal-master2) only resolves the role.

set -uo pipefail

RESOURCE="${DRBD_RESOURCE:-cms_data}"
PROMOTE="${PROMOTE:-0}"
# A reachable peer answers the DRBD handshake within a couple of seconds; the
# budget only matters when the peer is down, where staying Primary is correct.
DEADLINE=$((SECONDS + ${ROLE_TIMEOUT:-30}))

log() { echo "[drbd-boot] $*"; }

drbdadm up all 2>/dev/null || true

STAND_DOWN=0
while [ "$SECONDS" -lt "$DEADLINE" ]; do
  role=$(drbdadm role "$RESOURCE" 2>/dev/null | cut -d/ -f1)
  cstate=$(drbdadm cstate "$RESOURCE" 2>/dev/null)

  # Not Primary: nothing to resolve (a promotion may still follow below).
  [ "$role" = "Primary" ] || break

  case "$cstate" in
    Established|Connected)
      # The peer is connected and, since only one side can hold Primary, it is
      # Secondary: this node is the single writer.
      break
      ;;
    StandAlone)
      log "both nodes report Primary (cstate=$cstate); standing down so the peer can resync this disk"
      drbdadm secondary "$RESOURCE" 2>/dev/null \
        || drbdadm secondary --force "$RESOURCE" 2>/dev/null \
        || log "could not demote $RESOURCE"
      drbdadm connect "$RESOURCE" 2>/dev/null || true
      STAND_DOWN=1
      break
      ;;
  esac
  sleep 2
done

if [ "$STAND_DOWN" = "1" ]; then
  # Let the reconnect finish before the promote attempt below: a promote that
  # races the handshake could win again and recreate the double writer.
  RECONNECT_DEADLINE=$((SECONDS + 30))
  while [ "$SECONDS" -lt "$RECONNECT_DEADLINE" ]; do
    case "$(drbdadm cstate "$RESOURCE" 2>/dev/null)" in
      Established|Connected) break ;;
    esac
    sleep 2
  done
fi

if [ "$PROMOTE" = "1" ]; then
  # Never promote out of StandAlone: that state means both sides claimed
  # Primary and the reconnect above has not settled, so a promote here could
  # recreate the double writer. Fails (and is ignored) while the peer is
  # Primary, which is exactly the behaviour wanted after a failover.
  if [ "$(drbdadm cstate "$RESOURCE" 2>/dev/null)" = "StandAlone" ]; then
    log "refusing to promote while cstate=StandAlone (peer may still be Primary)"
  else
    drbdadm primary "$RESOURCE" 2>/dev/null || true
  fi
fi

drbdadm status "$RESOURCE" 2>/dev/null || true
exit 0
