#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# tests/integration/run.sh — end-to-end integration test on a real KVM host.
#
# This is the entry point of the "Integration (KVM lab)" workflow: it deploys
# the whole lab, verifies it and runs the DRBD failover chaos exercise. It has
# to run on a hypervisor with libvirt access, because the orchestrator manages
# the VMs directly and SSHes into them: CI runs it on the self-hosted `kvm`
# runner for the full topology, and on a GitHub-hosted runner where a smaller
# topology has already been provisioned (SKIP_VM_CREATE=1).
#
# Usage:
#   tests/integration/run.sh [--resume] [--skip-deploy] [--skip-chaos] [--with-traffic] [--with-drills] [--dry-run]
#
# Environment:
#   CONFIG           configuration file             (default: config.yaml)
#   STATE_FILE       deployment checkpoint          (default: deploy.state)
#   ARTIFACTS_DIR    where logs/state are collected (default: integration-artifacts)
#   TIMINGS_FILE     per-phase timing report         (default: $ARTIFACTS_DIR/timings.json)
#   SKIP_VM_CREATE   1 = the lab's networks and VMs are already provisioned, so
#                     deploy runs with --skip-vm-create (Step 3 of the manual).
#                     Set by workflows that provision (Step 1/2) beforehand.
#                     It also starts the deploy at 03-repair-ssh, because the
#                     provisioning steps already ran Cobbler and registration.
#   TRAFFIC_DURATION seconds of k6 load with --with-traffic (default: 45)
#   TRAFFIC_VUS      k6 virtual users with --with-traffic    (default: 3)
# ---------------------------------------------------------------------------
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR"

CONFIG="${CONFIG:-config.yaml}"
STATE_FILE="${STATE_FILE:-deploy.state}"
ARTIFACTS_DIR="${ARTIFACTS_DIR:-integration-artifacts}"
TIMINGS_FILE="${TIMINGS_FILE:-$ARTIFACTS_DIR/timings.json}"
SKIP_VM_CREATE="${SKIP_VM_CREATE:-0}"
TRAFFIC_DURATION="${TRAFFIC_DURATION:-45}"
TRAFFIC_VUS="${TRAFFIC_VUS:-3}"

RESUME=0
SKIP_DEPLOY=0
SKIP_CHAOS=0
WITH_TRAFFIC=0
WITH_DRILLS=0
DRY_RUN=0

usage() {
  sed -n '2,27p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
  case "$1" in
    --resume)       RESUME=1 ;;
    --skip-deploy)  SKIP_DEPLOY=1 ;;
    --skip-chaos)   SKIP_CHAOS=1 ;;
    --with-traffic) WITH_TRAFFIC=1 ;;
    --with-drills)  WITH_DRILLS=1 ;;
    --dry-run)      DRY_RUN=1 ;;
    -h|--help)      usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

log()  { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
fail() { printf '\n\033[1;31mFAILED: %s\033[0m\n' "$*" >&2; }

mkdir -p "$ARTIFACTS_DIR"

# ── Preflight ──────────────────────────────────────────────────────────────
log "Preflight: KVM, libvirt, container runtime and configuration"
for tool in virsh docker; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    fail "$tool is required on the hypervisor"
    exit 1
  fi
done
if [ ! -e /dev/kvm ]; then
  fail "/dev/kvm is not available (nested virtualisation disabled?)"
  exit 1
fi
if [ ! -f "$CONFIG" ]; then
  fail "configuration file $CONFIG not found"
  exit 1
fi
if ! virsh version >/dev/null 2>&1; then
  fail "cannot talk to libvirt (is libvirtd running and \$LIBVIRT_DEFAULT_URI set?)"
  exit 1
fi
echo "  ✔ virsh, docker, /dev/kvm and $CONFIG are available"

log "Building cms-ha"
make build

EXIT_CODE=0

# ── Deploy ─────────────────────────────────────────────────────────────────
if [ "$SKIP_DEPLOY" -eq 0 ]; then
  DEPLOY_ARGS=()
  if [ "$RESUME" -eq 1 ] && [ -f "$STATE_FILE" ]; then
    log "Resuming deployment from the checkpoint ($STATE_FILE)"
    DEPLOY_ARGS+=(--resume)
  else
    log "Running the full deployment"
  fi
  if [ "$DRY_RUN" -eq 1 ]; then
    DEPLOY_ARGS+=(--dry-run)
  fi
  if [ "$SKIP_VM_CREATE" = "1" ]; then
    # The VM/networks phases were already run by the provisioning step, so take
    # the manual's Step 3 path instead of revalidating them. Cobbler and the
    # node registration also already ran, so start at 03-repair-ssh: re-running
    # them would rebuild the 3.9 GB mirror and re-register every system for no
    # benefit (measured: 1m26s on the hosted runner).
    log "Deploying into an already provisioned lab (--skip-vm-create)"
    DEPLOY_ARGS+=(--skip-vm-create)
    if [ "$RESUME" -eq 0 ]; then
      DEPLOY_ARGS+=(--from-phase 03-repair-ssh)
    fi
  fi

  DEPLOY_ARGS+=(--timings-file "$TIMINGS_FILE")

  if ./cms-ha -c "$CONFIG" deploy --state-file "$STATE_FILE" "${DEPLOY_ARGS[@]}"; then
    log "Deployment succeeded"
  else
    fail "deployment failed"
    EXIT_CODE=1
  fi
else
  log "Skipping deployment (--skip-deploy)"
fi

# Always collect the checkpoint: it names the phase that failed and lets a rerun
# resume instead of starting over.
if [ -f "$STATE_FILE" ]; then
  cp -f "$STATE_FILE" "$ARTIFACTS_DIR/" 2>/dev/null || true
fi

# ── Verify ─────────────────────────────────────────────────────────────────
if [ "$EXIT_CODE" -eq 0 ] && [ "$DRY_RUN" -eq 0 ]; then
  log "Verifying the infrastructure"
  if ./cms-ha -c "$CONFIG" verify 2>&1 | tee "$ARTIFACTS_DIR/verify.log"; then
    log "Verification passed"
  else
    fail "verification failed"
    EXIT_CODE=1
  fi
fi

# ── Traffic: k6 load profile ───────────────────────────────────────────────
# Optional: the verify step above already proves the request path with a short
# burst, so the load profile is opt-in (workflows enable it). k6 exits non-zero
# when its thresholds are crossed, which makes the step a real gate.
if [ "$EXIT_CODE" -eq 0 ] && [ "$WITH_TRAFFIC" -eq 1 ] && [ "$DRY_RUN" -eq 0 ]; then
  log "Generating k6 load against the CMS (${TRAFFIC_DURATION}s, ${TRAFFIC_VUS} VUs)"
  if ./cms-ha -c "$CONFIG" traffic --external \
      --duration "$TRAFFIC_DURATION" --concurrency "$TRAFFIC_VUS" \
      2>&1 | tee "$ARTIFACTS_DIR/traffic.log"; then
    log "Traffic test passed"
  else
    fail "traffic test failed"
    EXIT_CODE=1
  fi
fi

# ── Drills: backup/restore, TLS renewal, alert hygiene ─────────────────────
# These paths have their own timers/CronJobs, so a green deploy never proves
# them. Each drill runs the production job/script and gates on its result.
if [ "$EXIT_CODE" -eq 0 ] && [ "$WITH_DRILLS" -eq 1 ] && [ "$DRY_RUN" -eq 0 ]; then
  log "Running the operational drills (backup/restore, TLS renewal, alert hygiene)"
  if ./cms-ha -c "$CONFIG" test drills 2>&1 | tee "$ARTIFACTS_DIR/drills.log"; then
    log "Operational drills passed"
  else
    fail "operational drills failed"
    EXIT_CODE=1
  fi
fi

# ── Chaos: DRBD failover ───────────────────────────────────────────────────
if [ "$EXIT_CODE" -eq 0 ] && [ "$SKIP_CHAOS" -eq 0 ] && [ "$DRY_RUN" -eq 0 ]; then
  log "Running the DRBD failover chaos exercise"
  # stderr carries the FAIL verdict (logging.Error), so it must be captured too.
  if ./cms-ha -c "$CONFIG" test failover 2>&1 | tee "$ARTIFACTS_DIR/failover.log"; then
    log "Failover exercise passed"
  else
    fail "failover exercise failed"
    EXIT_CODE=1
  fi
fi

if [ "$EXIT_CODE" -eq 0 ]; then
  log "Integration test PASSED"
else
  fail "Integration test FAILED (see $ARTIFACTS_DIR)"
fi
exit "$EXIT_CODE"
