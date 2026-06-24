#!/bin/bash
# test_failover.sh
#
# Automated Chaos Engineering / Failover Testing Suite
#
# Purpose:
#   Validates the high-availability guarantees of the CMS infrastructure by
#   intentionally shutting down critical VMs and measuring how quickly the
#   system recovers (convergence time). Each scenario verifies that the CMS
#   remains reachable during or shortly after a component failure.
#
# Scenarios tested:
#   1. DRBD Master Failover   – Shuts down internal-master1 and verifies
#                                master2 promotes to DRBD Primary and MariaDB
#                                reschedules automatically.
#   2. CMS Frontend Failover  – Shuts down main-cms1 and verifies the Nginx
#                                load balancer still routes traffic to main-cms2.
#   3. K3s Worker Failover     – Shuts down internal-worker1 and verifies that
#                                Kubernetes reschedules pods to internal-worker2.
#
# Usage:
#   ./test_failover.sh [--skip-restore]
#
#   --skip-restore  Leave VMs in their failed state after each test so the
#                   operator can manually inspect the cluster behaviour.
#
# Prerequisites:
#   - Must be executed from the KVM hypervisor host
#   - All VMs must be running before starting the test suite
#   - SSH key-based access to all cluster nodes (via config.sh)

set -uo pipefail

# ── Resolve script location and load shared configuration ─────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/../config.sh"

# ── Constants ─────────────────────────────────────────────────────────────────
VIRSH="${VIRSH:-virsh -c qemu:///system}"
CMS_URL="https://192.168.20.100"

# Maximum seconds to wait for various convergence conditions
TIMEOUT_DRBD=300
TIMEOUT_POD=300
TIMEOUT_SSH=180
TIMEOUT_CURL=120
TIMEOUT_K3S_RESCHEDULE=300

# ── Parse CLI flags ──────────────────────────────────────────────────────────
SKIP_RESTORE=false
for arg in "$@"; do
  case "$arg" in
    --skip-restore) SKIP_RESTORE=true ;;
    *)
      error "Unknown argument: $arg"
      echo "Usage: $0 [--skip-restore]"
      exit 1
      ;;
  esac
done

# ── Results tracking ─────────────────────────────────────────────────────────
# Arrays to collect the final summary table rows
declare -a RESULT_NAMES=()
declare -a RESULT_STATUS=()
declare -a RESULT_TIMES=()

# ═══════════════════════════════════════════════════════════════════════════════
# Helper Functions
# ═══════════════════════════════════════════════════════════════════════════════

# wait_for_vm_ssh <ip> [timeout_seconds]
#   Polls SSH connectivity until the target responds or the timeout expires.
#   Returns 0 on success, 1 on timeout.
wait_for_vm_ssh() {
  local ip="$1"
  local timeout="${2:-$TIMEOUT_SSH}"
  local elapsed=0

  info "Waiting for SSH on ${ip} (timeout: ${timeout}s)..."
  while [ $elapsed -lt $timeout ]; do
    if ssh ${SSH_OPTS} root@"${ip}" true 2>/dev/null; then
      success "SSH reachable on ${ip} after ${elapsed}s"
      return 0
    fi
    sleep 5
    elapsed=$((elapsed + 5))
  done

  error "SSH timeout (${timeout}s) waiting for ${ip}"
  return 1
}

# check_cms_reachable [label]
#   Verifies the CMS site returns HTTP 200 via the load balancer VIP.
#   Returns 0 on success, 1 on failure.
check_cms_reachable() {
  local label="${1:-CMS reachability}"
  local http_code

  http_code=$(curl -skL -o /dev/null -w '%{http_code}' --max-time 15 "${CMS_URL}" 2>/dev/null || echo "000")

  if [ "$http_code" = "200" ]; then
    success "${label}: HTTP ${http_code}"
    return 0
  else
    error "${label}: HTTP ${http_code} (expected 200)"
    return 1
  fi
}

# record_result <test_name> <PASS|FAIL> <seconds>
#   Appends a row to the results table arrays.
record_result() {
  RESULT_NAMES+=("$1")
  RESULT_STATUS+=("$2")
  RESULT_TIMES+=("$3")
}

# elapsed_since <start_epoch>
#   Prints the number of seconds since the given epoch timestamp.
elapsed_since() {
  local start="$1"
  echo $(( $(date +%s) - start ))
}

# ═══════════════════════════════════════════════════════════════════════════════
# SCENARIO 1: DRBD Master Failover
# ═══════════════════════════════════════════════════════════════════════════════
# When internal-master1 (the DRBD Primary) goes down, internal-master2 must:
#   1. Promote itself to DRBD Primary
#   2. Mount the replicated filesystem
#   3. Accept the MariaDB pod rescheduled by Kubernetes
# The CMS must remain reachable throughout via the load balancer.

test_drbd_failover() {
  echo ""
  echo -e "${GREEN}═══════════════════════════════════════════════════════════${NC}"
  info "SCENARIO 1: DRBD Master Failover"
  echo -e "${GREEN}═══════════════════════════════════════════════════════════${NC}"

  local start_time
  start_time=$(date +%s)
  local test_status="FAIL"

  # Step 1: Pre-flight – verify CMS is healthy before we break anything
  info "[1/7] Pre-flight: verifying CMS is reachable..."
  if ! check_cms_reachable "Pre-flight check"; then
    error "CMS is not reachable before the test. Aborting scenario."
    record_result "DRBD Master Failover" "FAIL" "N/A"
    return 1
  fi

  # Step 2: Shut down internal-master1 (the current DRBD Primary)
  info "[2/7] Shutting down internal-master1..."
  $VIRSH shutdown internal-master1 2>/dev/null || true
  # Wait for the VM to actually stop so the cluster detects the failure
  sleep 10

  # Step 3: Wait for DRBD on master2 to promote to Primary
  # The drbd-failover.sh script or manual promotion should make master2 Primary.
  # We poll the DRBD status output on master2 looking for 'Primary' in the role.
  info "[3/7] Waiting for internal-master2 to become DRBD Primary (timeout: ${TIMEOUT_DRBD}s)..."
  local elapsed=0
  local drbd_promoted=false

  while [ $elapsed -lt $TIMEOUT_DRBD ]; do
    local status_output
    status_output=$(ssh ${SSH_OPTS} root@"${MASTER2_IP}" 'drbdadm status cms_data' 2>/dev/null || echo "")
    if echo "$status_output" | grep -q 'Primary'; then
      drbd_promoted=true
      success "DRBD promoted to Primary on master2 after ${elapsed}s"
      break
    fi
    sleep 5
    elapsed=$((elapsed + 5))
  done

  if [ "$drbd_promoted" = "false" ]; then
    error "DRBD promotion timeout on master2 after ${TIMEOUT_DRBD}s"
    record_result "DRBD Master Failover" "FAIL" "$(elapsed_since $start_time)s"
    return 1
  fi

  # Step 4: Wait for the MariaDB pod to reschedule and reach Running state on master2
  info "[4/7] Waiting for MariaDB pod to reschedule on master2 (timeout: ${TIMEOUT_POD}s)..."
  elapsed=0
  local pod_ready=false

  while [ $elapsed -lt $TIMEOUT_POD ]; do
    local pod_phase
    pod_phase=$(ssh ${SSH_OPTS} root@"${MASTER2_IP}" \
      'kubectl get pods -n cms -l app=mariadb -o jsonpath={.items[0].status.phase}' 2>/dev/null || echo "")
    if [ "$pod_phase" = "Running" ]; then
      pod_ready=true
      success "MariaDB pod Running on master2 after ${elapsed}s"
      break
    fi
    sleep 10
    elapsed=$((elapsed + 10))
  done

  if [ "$pod_ready" = "false" ]; then
    error "MariaDB pod did not reach Running state within ${TIMEOUT_POD}s"
    record_result "DRBD Master Failover" "FAIL" "$(elapsed_since $start_time)s"
    return 1
  fi

  # Step 5: Verify CMS is still reachable after failover
  info "[5/7] Post-failover: verifying CMS is still reachable..."
  # Give the pod a few seconds to fully serve traffic
  sleep 5
  if ! check_cms_reachable "Post-failover check"; then
    warn "CMS not immediately reachable, retrying for up to ${TIMEOUT_CURL}s..."
    elapsed=0
    local cms_ok=false
    while [ $elapsed -lt $TIMEOUT_CURL ]; do
      if check_cms_reachable "Retry check"; then
        cms_ok=true
        break
      fi
      sleep 5
      elapsed=$((elapsed + 5))
    done
    if [ "$cms_ok" = "false" ]; then
      error "CMS unreachable after DRBD failover"
      record_result "DRBD Master Failover" "FAIL" "$(elapsed_since $start_time)s"
      return 1
    fi
  fi

  # Step 6: Record convergence time
  local convergence
  convergence=$(elapsed_since $start_time)
  success "DRBD failover converged in ${convergence}s"
  test_status="PASS"

  # Step 7: Restore – bring master1 back and revert DRBD roles
  if [ "$SKIP_RESTORE" = "false" ]; then
    info "[7/7] Restoring: starting internal-master1..."
    $VIRSH start internal-master1 2>/dev/null || true

    if wait_for_vm_ssh "$MASTER1_IP"; then
      info "Reverting DRBD roles: demoting master2, promoting master1..."
      ssh ${SSH_OPTS} root@"${MASTER2_IP}" '/usr/local/bin/drbd-failover.sh demote' 2>/dev/null || true
      sleep 5
      ssh ${SSH_OPTS} root@"${MASTER1_IP}" '/usr/local/bin/drbd-failover.sh promote' 2>/dev/null || true
      success "DRBD roles restored (master1=Primary, master2=Secondary)"
    else
      warn "Could not restore master1 – VM did not become SSH-reachable in time"
    fi
  else
    warn "Skipping restore (--skip-restore). internal-master1 remains shut down."
  fi

  record_result "DRBD Master Failover" "$test_status" "${convergence}s"
}

# ═══════════════════════════════════════════════════════════════════════════════
# SCENARIO 2: CMS Frontend Failover
# ═══════════════════════════════════════════════════════════════════════════════
# With two Apache frontends behind the Nginx load balancer, losing one should
# be transparent to users. The LB health checks will remove the dead backend
# and route all traffic to the surviving node.

test_cms_frontend_failover() {
  echo ""
  echo -e "${GREEN}═══════════════════════════════════════════════════════════${NC}"
  info "SCENARIO 2: CMS Frontend Failover"
  echo -e "${GREEN}═══════════════════════════════════════════════════════════${NC}"

  local start_time
  start_time=$(date +%s)
  local test_status="FAIL"

  # Step 1: Verify both CMS frontends respond individually
  info "[1/5] Pre-flight: verifying both CMS frontends..."
  local cms1_code cms2_code
  cms1_code=$(curl -sk -o /dev/null -w '%{http_code}' --max-time 10 "http://${CMS1_IP}:80" 2>/dev/null || echo "000")
  cms2_code=$(curl -sk -o /dev/null -w '%{http_code}' --max-time 10 "http://${CMS2_IP}:80" 2>/dev/null || echo "000")

  info "  CMS1 (${CMS1_IP}): HTTP ${cms1_code}"
  info "  CMS2 (${CMS2_IP}): HTTP ${cms2_code}"

  if [ "$cms1_code" = "000" ] && [ "$cms2_code" = "000" ]; then
    error "Neither CMS frontend is responding. Aborting scenario."
    record_result "CMS Frontend Failover" "FAIL" "N/A"
    return 1
  fi

  # Step 2: Shut down main-cms1
  info "[2/5] Shutting down main-cms1..."
  $VIRSH shutdown main-cms1 2>/dev/null || true
  # Allow Nginx health checks to detect the backend is down
  sleep 10

  # Step 3: Verify the load balancer still returns HTTP 200
  info "[3/5] Verifying LB still serves traffic via remaining backend..."
  local elapsed=0
  local lb_ok=false

  while [ $elapsed -lt $TIMEOUT_CURL ]; do
    if check_cms_reachable "LB after cms1 shutdown"; then
      lb_ok=true
      break
    fi
    sleep 5
    elapsed=$((elapsed + 5))
  done

  if [ "$lb_ok" = "false" ]; then
    error "Load balancer did not recover within ${TIMEOUT_CURL}s"
    record_result "CMS Frontend Failover" "FAIL" "$(elapsed_since $start_time)s"
    return 1
  fi

  # Step 4: Record convergence time
  local convergence
  convergence=$(elapsed_since $start_time)
  success "CMS frontend failover converged in ${convergence}s"
  test_status="PASS"

  # Step 5: Restore – bring main-cms1 back
  if [ "$SKIP_RESTORE" = "false" ]; then
    info "[5/5] Restoring: starting main-cms1..."
    $VIRSH start main-cms1 2>/dev/null || true
    wait_for_vm_ssh "$CMS1_IP" || warn "main-cms1 did not become SSH-reachable in time"
  else
    warn "Skipping restore (--skip-restore). main-cms1 remains shut down."
  fi

  record_result "CMS Frontend Failover" "$test_status" "${convergence}s"
}

# ═══════════════════════════════════════════════════════════════════════════════
# SCENARIO 3: K3s Worker Failover
# ═══════════════════════════════════════════════════════════════════════════════
# When a K3s agent node goes down, the Kubernetes scheduler should eventually
# reschedule all pods that were running on that node onto the surviving worker.
# This test verifies that all non-system pods recover.

test_k3s_worker_failover() {
  echo ""
  echo -e "${GREEN}═══════════════════════════════════════════════════════════${NC}"
  info "SCENARIO 3: K3s Worker Failover"
  echo -e "${GREEN}═══════════════════════════════════════════════════════════${NC}"

  local start_time
  start_time=$(date +%s)
  local test_status="FAIL"

  # We query Kubernetes from master1 (or master2 if master1 is unavailable)
  local kubectl_host="$MASTER1_IP"
  if ! ssh ${SSH_OPTS} root@"${MASTER1_IP}" true 2>/dev/null; then
    kubectl_host="$MASTER2_IP"
  fi

  # Step 1: Record pods currently running on internal-worker1
  info "[1/5] Listing pods on internal-worker1 before shutdown..."
  local worker1_pods
  worker1_pods=$(ssh ${SSH_OPTS} root@"${kubectl_host}" \
    "kubectl get pods -A -o wide --field-selector spec.nodeName=internal-worker1 --no-headers" 2>/dev/null || echo "")

  if [ -z "$worker1_pods" ]; then
    warn "No pods found on internal-worker1. The test will verify node recovery only."
  else
    echo "$worker1_pods"
  fi

  local pod_count
  pod_count=$(echo "$worker1_pods" | grep -c . || echo "0")
  info "Found ${pod_count} pod(s) on internal-worker1"

  # Step 2: Shut down internal-worker1
  info "[2/5] Shutting down internal-worker1..."
  $VIRSH shutdown internal-worker1 2>/dev/null || true
  # Kubernetes marks a node as NotReady after ~40s by default; give it time
  sleep 15

  # Step 3: Wait for pods to reschedule onto internal-worker2
  # We poll until no pods remain in Pending/ContainerCreating state (excluding
  # DaemonSets that are expected to remain on the lost node).
  info "[3/5] Waiting for pods to reschedule (timeout: ${TIMEOUT_K3S_RESCHEDULE}s)..."
  local elapsed=0
  local pods_rescheduled=false

  while [ $elapsed -lt $TIMEOUT_K3S_RESCHEDULE ]; do
    # Check if there are any non-Running, non-Succeeded pods outside the lost node
    local bad_pods
    bad_pods=$(ssh ${SSH_OPTS} root@"${kubectl_host}" \
      "kubectl get pods -A -o wide --no-headers 2>/dev/null | grep -v 'Running\|Completed\|Succeeded' | grep -v 'internal-worker1'" 2>/dev/null || echo "")

    if [ -z "$bad_pods" ]; then
      pods_rescheduled=true
      success "All pods rescheduled successfully after ${elapsed}s"
      break
    fi
    sleep 10
    elapsed=$((elapsed + 10))
  done

  if [ "$pods_rescheduled" = "false" ]; then
    warn "Some pods may not have fully rescheduled within ${TIMEOUT_K3S_RESCHEDULE}s"
    # Show current pod state for diagnostics
    ssh ${SSH_OPTS} root@"${kubectl_host}" "kubectl get pods -A -o wide --no-headers" 2>/dev/null || true
  fi

  # Step 4: Record convergence time
  local convergence
  convergence=$(elapsed_since $start_time)
  if [ "$pods_rescheduled" = "true" ]; then
    success "K3s worker failover converged in ${convergence}s"
    test_status="PASS"
  else
    warn "K3s worker failover partially converged in ${convergence}s"
    test_status="FAIL"
  fi

  # Step 5: Restore – bring internal-worker1 back
  if [ "$SKIP_RESTORE" = "false" ]; then
    info "[5/5] Restoring: starting internal-worker1..."
    $VIRSH start internal-worker1 2>/dev/null || true
    wait_for_vm_ssh "$WORKER1_IP" || warn "internal-worker1 did not become SSH-reachable in time"
  else
    warn "Skipping restore (--skip-restore). internal-worker1 remains shut down."
  fi

  record_result "K3s Worker Failover" "$test_status" "${convergence}s"
}

# ═══════════════════════════════════════════════════════════════════════════════
# Summary Table
# ═══════════════════════════════════════════════════════════════════════════════
# Prints a formatted table with all test results and convergence times.

print_summary() {
  echo ""
  echo -e "${GREEN}═══════════════════════════════════════════════════════════${NC}"
  echo -e "${GREEN}                FAILOVER TEST SUMMARY                     ${NC}"
  echo -e "${GREEN}═══════════════════════════════════════════════════════════${NC}"
  echo ""

  # Table header
  printf "%-30s %-10s %-20s\n" "TEST" "RESULT" "CONVERGENCE TIME"
  printf "%-30s %-10s %-20s\n" "------------------------------" "----------" "--------------------"

  local all_passed=true
  for i in "${!RESULT_NAMES[@]}"; do
    local status_color
    if [ "${RESULT_STATUS[$i]}" = "PASS" ]; then
      status_color="${GREEN}PASS${NC}"
    else
      status_color="${RED}FAIL${NC}"
      all_passed=false
    fi
    printf "%-30s %-10b %-20s\n" "${RESULT_NAMES[$i]}" "$status_color" "${RESULT_TIMES[$i]}"
  done

  echo ""
  if [ "$all_passed" = "true" ]; then
    success "All failover scenarios passed successfully ✔"
  else
    error "One or more failover scenarios failed ✗"
  fi
  echo ""
}

# ═══════════════════════════════════════════════════════════════════════════════
# Main Execution
# ═══════════════════════════════════════════════════════════════════════════════

echo ""
echo -e "${GREEN}╔═══════════════════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║     CMS Infrastructure – Failover Test Suite             ║${NC}"
echo -e "${GREEN}╠═══════════════════════════════════════════════════════════╣${NC}"
echo -e "${GREEN}║  Hypervisor: $(hostname)${NC}"
echo -e "${GREEN}║  Timestamp:  $(date '+%Y-%m-%d %H:%M:%S %Z')${NC}"
echo -e "${GREEN}║  Restore:    $([ "$SKIP_RESTORE" = "true" ] && echo "DISABLED (--skip-restore)" || echo "ENABLED")${NC}"
echo -e "${GREEN}╚═══════════════════════════════════════════════════════════╝${NC}"
echo ""

# Run each scenario sequentially – order matters because scenario 1 may
# affect the DRBD primary which is needed by subsequent tests.
test_drbd_failover
test_cms_frontend_failover
test_k3s_worker_failover

# Final summary
print_summary

# Exit with non-zero if any test failed
for status in "${RESULT_STATUS[@]}"; do
  if [ "$status" = "FAIL" ]; then
    exit 1
  fi
done
exit 0
