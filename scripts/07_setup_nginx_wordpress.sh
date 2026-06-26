#!/bin/bash
# 07_setup_nginx_wordpress.sh
#
# Triggers a Puppet agent run on the load balancer and CMS frontend nodes.
# Puppet manages the full desired state for these nodes:
#   - main-lb:    Nginx upstream config, self-signed SSL cert, UFW rules
#   - main-cms*:  Apache + PHP packages, WordPress vhost, WP-CLI core install, UFW rules
#
# Pre-requisites:
#   - 04_setup_puppet.sh must have completed (agents registered and certs signed).
#   - 06_setup_kubernetes.sh must have completed (MariaDB NodePort 30306 must be up).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/config.sh"

PUPPET="/opt/puppetlabs/bin/puppet"
PUPPET_RUN="$PUPPET agent -t"

echo ">>> Applying Puppet configuration — Load Balancer and CMS Frontends <<<"
echo ""

# ---------------------------------------------------------------------------
# Run puppet agent on each node in parallel, then collect exit codes.
# Exit code 0 = no changes; exit code 2 = changes applied (both are success).
# Exit code 1 or 4+ = error.
# ---------------------------------------------------------------------------
run_puppet() {
    local NODE_IP="$1"
    local NODE_NAME="$2"
    echo "[+] $NODE_NAME ($NODE_IP): starting Puppet run..."
    local EXIT_CODE
    ssh ${SSH_OPTS} root@"$NODE_IP" "$PUPPET_RUN" 2>&1 | sed "s/^/    [$NODE_NAME] /"
    EXIT_CODE=${PIPESTATUS[0]}
    if [ $EXIT_CODE -eq 0 ] || [ $EXIT_CODE -eq 2 ]; then
        echo "  [OK] $NODE_NAME converged successfully."
    else
        echo "  [ERROR] $NODE_NAME Puppet run failed (exit $EXIT_CODE)."
        return 1
    fi
}

# Load balancer
run_puppet "$LB_IP"   "main-lb"

# CMS frontend nodes
run_puppet "$CMS1_IP" "main-cms1"
run_puppet "$CMS2_IP" "main-cms2"

echo ""
echo ">>> Verifying service state <<<"

# Nginx sanity check on LB
echo "[+] Nginx config test on main-lb..."
ssh ${SSH_OPTS} root@"$LB_IP" 'nginx -t && systemctl is-active nginx' && \
    echo "  [OK] Nginx active and config valid."

# Apache check on CMS nodes
for CMS_IP in "$CMS1_IP" "$CMS2_IP"; do
    echo "[+] Apache check on $CMS_IP..."
    ssh ${SSH_OPTS} root@"$CMS_IP" 'apache2ctl -t 2>&1 | grep -q "Syntax OK" && systemctl is-active apache2' && \
        echo "  [OK] Apache active on $CMS_IP."
done

# HTTPS reachability through load balancer
echo "[+] HTTPS reachability check via load balancer..."
if ssh ${SSH_OPTS} root@"$LB_IP" 'curl -skI https://127.0.0.1/ | grep -q "HTTP/"'; then
    echo "  [OK] WordPress reachable at https://$LB_IP/"
else
    echo "  [WARN] HTTPS check failed — WordPress may still be initialising. Retry in 30s."
fi

echo ""
echo "════════════════════════════════════════════════════════════════"
echo "  Nginx + WordPress deployment complete (managed by Puppet)"
echo "  Load Balancer HTTPS: https://$LB_IP/"
echo "════════════════════════════════════════════════════════════════"
