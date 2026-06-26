#!/bin/bash
# 10_setup_internal_ca.sh
#
# Phase 08: Internal Certificate Authority (PKI) with step-ca
#
# Deploys a lightweight private CA on the Jumpstart node using Smallstep's
# step-ca. Issues and distributes TLS certificates to infrastructure services:
#   - Nginx LB (HTTPS frontend at main-lb)
#   - Grafana dashboard (internal-monitor)
#   - K3s API server (internal-master1, internal-master2)
#
# The root CA certificate is distributed to all nodes so they trust
# certificates issued by this CA.
#
# Prerequisites:
#   - Jumpstart node must be reachable via SSH
#   - All target nodes must be reachable via SSH
#   - 04_setup_puppet.sh should have completed (for SSH key distribution)

set -euo pipefail

# Load global configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/config.sh"

# ── Configuration ─────────────────────────────────────────────────────────────
CA_HOST="$JUMPSTART_IP"
CA_PORT="8443"
CA_PROVISIONER_PASSWORD="StepCA-Pr0v1s10ner!"
CA_DIR="/etc/step-ca"
STEP_VERSION="${STEP_VERSION:-0.27.5}"
STEP_CA_VERSION="${STEP_CA_VERSION:-0.27.5}"
CA_DOMAIN="ca.internal.local"

# Services that need TLS certificates
declare -A CERT_TARGETS=(
    ["nginx-lb"]="${LB_IP}"
    ["grafana"]="${MONITOR_IP}"
    ["k3s-master1"]="${MASTER1_IP}"
    ["k3s-master2"]="${MASTER2_IP}"
)

# All nodes that need the root CA certificate for trust
TRUST_NODES=(
    "${ALL_INTERNAL_NODES[@]}"
    "${ALL_MAIN_NODES[@]}"
    "$ROUTER_IP"
)

echo ">>> Phase 08: Internal Certificate Authority (step-ca) <<<"
echo ""

# ==============================================================================
# 1. INSTALL step CLI AND step-ca ON JUMPSTART
# ==============================================================================
info "Installing step CLI and step-ca on jumpstart ($CA_HOST)..."

ssh ${SSH_OPTS} root@"$CA_HOST" bash -s <<'INSTALL_EOF'
set -euo pipefail

# Skip if already installed
if command -v step &>/dev/null && command -v step-ca &>/dev/null; then
    echo "  [OK] step and step-ca already installed"
    exit 0
fi

ARCH=$(dpkg --print-architecture 2>/dev/null || echo "amd64")

# Install step CLI
if ! command -v step &>/dev/null; then
    echo "  [+] Installing step CLI..."
    STEP_DEB="step-cli_STEP_VERSION_${ARCH}.deb"
    wget -q "https://dl.smallstep.com/cli/docs-cli-install/latest/step-cli_STEP_VERSION_${ARCH}.deb" \
        -O "/tmp/${STEP_DEB}" 2>/dev/null || \
    wget -q "https://github.com/smallstep/cli/releases/latest/download/step-cli_STEP_VERSION_${ARCH}.deb" \
        -O "/tmp/${STEP_DEB}" 2>/dev/null || true

    if [ -f "/tmp/${STEP_DEB}" ] && [ -s "/tmp/${STEP_DEB}" ]; then
        dpkg -i "/tmp/${STEP_DEB}" || apt-get install -f -y
        rm -f "/tmp/${STEP_DEB}"
    else
        echo "  [INFO] Binary download failed, trying apt..."
        apt-get update -qq && apt-get install -y -qq step-cli 2>/dev/null || true
    fi
fi

# Install step-ca
if ! command -v step-ca &>/dev/null; then
    echo "  [+] Installing step-ca..."
    STEP_CA_DEB="step-ca_STEP_CA_VERSION_${ARCH}.deb"
    wget -q "https://dl.smallstep.com/certificates/docs-ca-install/latest/step-ca_STEP_CA_VERSION_${ARCH}.deb" \
        -O "/tmp/${STEP_CA_DEB}" 2>/dev/null || \
    wget -q "https://github.com/smallstep/certificates/releases/latest/download/step-ca_STEP_CA_VERSION_${ARCH}.deb" \
        -O "/tmp/${STEP_CA_DEB}" 2>/dev/null || true

    if [ -f "/tmp/${STEP_CA_DEB}" ] && [ -s "/tmp/${STEP_CA_DEB}" ]; then
        dpkg -i "/tmp/${STEP_CA_DEB}" || apt-get install -f -y
        rm -f "/tmp/${STEP_CA_DEB}"
    else
        echo "  [INFO] Binary download failed, trying apt..."
        apt-get update -qq && apt-get install -y -qq step-ca 2>/dev/null || true
    fi
fi

echo "  [OK] Installation complete"
INSTALL_EOF

success "step CLI and step-ca installed on jumpstart"

# ==============================================================================
# 2. INITIALIZE THE CERTIFICATE AUTHORITY
# ==============================================================================
info "Initializing internal CA on jumpstart..."

ssh ${SSH_OPTS} root@"$CA_HOST" bash -s -- "$CA_DIR" "$CA_PROVISIONER_PASSWORD" "$CA_DOMAIN" "$CA_PORT" <<'INIT_EOF'
set -euo pipefail
CA_DIR="$1"
CA_PASS="$2"
CA_DNS="$3"
CA_PORT="$4"
CA_LISTEN=":${CA_PORT}"

# Skip if CA is already initialised
if [ -f "${CA_DIR}/certs/root_ca.crt" ]; then
    echo "  [OK] CA already initialised at ${CA_DIR}"
    exit 0
fi

mkdir -p "${CA_DIR}"

# Write provisioner password to file
echo "$CA_PASS" > "${CA_DIR}/provisioner-password.txt"
chmod 600 "${CA_DIR}/provisioner-password.txt"

# Initialize the CA
echo "  [+] Running step ca init..."
step ca init \
    --name "CMS-HA Internal CA" \
    --dns "$CA_DNS" \
    --dns "$(hostname)" \
    --dns "192.168.10.10" \
    --address "$CA_LISTEN" \
    --provisioner "admin" \
    --password-file "${CA_DIR}/provisioner-password.txt" \
    --deployment-type standalone \
    --no-db 2>&1 | sed 's/^/    /'

# Move generated files to our CA directory if step placed them elsewhere
STEP_PATH=$(step path 2>/dev/null || echo "$HOME/.step")
if [ -d "${STEP_PATH}/certs" ] && [ "${STEP_PATH}" != "${CA_DIR}" ]; then
    cp -rn "${STEP_PATH}/certs" "${CA_DIR}/" 2>/dev/null || true
    cp -rn "${STEP_PATH}/secrets" "${CA_DIR}/" 2>/dev/null || true
    cp -rn "${STEP_PATH}/config" "${CA_DIR}/" 2>/dev/null || true
fi

# Add ACME provisioner for automated certificate renewal
step ca provisioner add acme --type ACME \
    --ca-config "${CA_DIR}/config/ca.json" 2>/dev/null || true

echo "  [OK] CA initialised successfully"
INIT_EOF

success "Internal CA initialised"

# ==============================================================================
# 3. CREATE AND ENABLE systemd SERVICE FOR step-ca
# ==============================================================================
info "Configuring step-ca systemd service..."

ssh ${SSH_OPTS} root@"$CA_HOST" bash -s -- "$CA_DIR" "$CA_PROVISIONER_PASSWORD" <<'SYSTEMD_EOF'
set -euo pipefail
CA_DIR="$1"
CA_PASS="$2"

STEP_PATH=$(step path 2>/dev/null || echo "$HOME/.step")
CA_CONFIG="${STEP_PATH}/config/ca.json"
[ -f "${CA_DIR}/config/ca.json" ] && CA_CONFIG="${CA_DIR}/config/ca.json"
PASS_FILE="${CA_DIR}/provisioner-password.txt"

cat > /etc/systemd/system/step-ca.service <<EOF
[Unit]
Description=Smallstep Certificate Authority (step-ca)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$(command -v step-ca) ${CA_CONFIG} --password-file ${PASS_FILE}
Restart=on-failure
RestartSec=10
User=root

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable step-ca
systemctl restart step-ca

# Wait for CA to become ready
for i in $(seq 1 15); do
    if curl -sk "https://localhost:${2:-8443}/health" 2>/dev/null | grep -q "ok"; then
        echo "  [OK] step-ca is running and healthy"
        exit 0
    fi
    sleep 2
done
echo "  [WARN] step-ca may not be fully ready yet (will retry on certificate issuance)"
SYSTEMD_EOF

success "step-ca systemd service configured"

# ==============================================================================
# 4. EXTRACT AND DISTRIBUTE ROOT CA CERTIFICATE
# ==============================================================================
info "Distributing root CA certificate to all nodes..."

# Fetch root CA cert from jumpstart
ROOT_CA_CERT=$(ssh ${SSH_OPTS} root@"$CA_HOST" "cat ${CA_DIR}/certs/root_ca.crt 2>/dev/null || cat \$(step path)/certs/root_ca.crt 2>/dev/null")

if [ -z "$ROOT_CA_CERT" ]; then
    error "Could not retrieve root CA certificate from jumpstart"
    exit 1
fi

FAILED_NODES=0
for NODE_IP in "${TRUST_NODES[@]}"; do
    if ! ssh ${SSH_OPTS} -o ConnectTimeout=5 root@"$NODE_IP" true 2>/dev/null; then
        warn "Node $NODE_IP not reachable — skipping CA trust distribution"
        FAILED_NODES=$((FAILED_NODES + 1))
        continue
    fi

    ssh ${SSH_OPTS} root@"$NODE_IP" bash -s <<TRUST_EOF
set -euo pipefail
mkdir -p /usr/local/share/ca-certificates/internal
cat > /usr/local/share/ca-certificates/internal/cms-ha-root-ca.crt <<'CERT'
${ROOT_CA_CERT}
CERT
update-ca-certificates --fresh 2>/dev/null || update-ca-certificates 2>/dev/null || true
TRUST_EOF

    echo "  [OK] Root CA trusted on $NODE_IP"
done

if [ "$FAILED_NODES" -gt 0 ]; then
    warn "$FAILED_NODES node(s) could not be reached for CA trust distribution"
else
    success "Root CA certificate distributed to all nodes"
fi

# ==============================================================================
# 5. ISSUE TLS CERTIFICATES FOR SERVICES
# ==============================================================================
info "Issuing TLS certificates for infrastructure services..."

CA_FINGERPRINT=$(ssh ${SSH_OPTS} root@"$CA_HOST" "step certificate fingerprint ${CA_DIR}/certs/root_ca.crt 2>/dev/null || step certificate fingerprint \$(step path)/certs/root_ca.crt 2>/dev/null")

for SERVICE in "${!CERT_TARGETS[@]}"; do
    TARGET_IP="${CERT_TARGETS[$SERVICE]}"
    info "  Issuing certificate for ${SERVICE} (${TARGET_IP})..."

    # Determine SANs based on service
    SANS="$TARGET_IP"
    case "$SERVICE" in
        nginx-lb)
            SANS="$TARGET_IP,main-lb,cms.fake-enterprise.com,192.168.20.100"
            ;;
        grafana)
            SANS="$TARGET_IP,internal-monitor,grafana.internal.local,192.168.10.20"
            ;;
        k3s-master1)
            SANS="$TARGET_IP,internal-master1,kubernetes.default,192.168.10.11"
            ;;
        k3s-master2)
            SANS="$TARGET_IP,internal-master2,192.168.10.12"
            ;;
    esac

    # Issue certificate from the CA on the jumpstart node
    ssh ${SSH_OPTS} root@"$CA_HOST" bash -s -- "$SERVICE" "$SANS" "$CA_PROVISIONER_PASSWORD" <<'ISSUE_EOF'
set -euo pipefail
SERVICE="$1"
SANS="$2"
CA_PASS="$3"

STEP_PATH=$(step path 2>/dev/null || echo "$HOME/.step")
CERT_DIR="/etc/step-ca/issued/${SERVICE}"
mkdir -p "${CERT_DIR}"

# Build SAN arguments
SAN_ARGS=""
IFS=',' read -ra SAN_LIST <<< "$SANS"
for san in "${SAN_LIST[@]}"; do
    SAN_ARGS="${SAN_ARGS} --san ${san}"
done

# Issue the certificate (valid for 720 hours = 30 days)
step ca certificate \
    "${SERVICE}" \
    "${CERT_DIR}/${SERVICE}.crt" \
    "${CERT_DIR}/${SERVICE}.key" \
    --provisioner "admin" \
    --provisioner-password-file "/etc/step-ca/provisioner-password.txt" \
    --not-after 720h \
    --force \
    ${SAN_ARGS} 2>&1 | sed 's/^/      /' || echo "  [WARN] Certificate issuance may have failed for ${SERVICE}"

chmod 644 "${CERT_DIR}/${SERVICE}.crt"
chmod 600 "${CERT_DIR}/${SERVICE}.key"
ISSUE_EOF

    # Copy certificate to target node
    if ssh ${SSH_OPTS} -o ConnectTimeout=5 root@"$TARGET_IP" true 2>/dev/null; then
        ssh ${SSH_OPTS} root@"$CA_HOST" "cat /etc/step-ca/issued/${SERVICE}/${SERVICE}.crt" | \
            ssh ${SSH_OPTS} root@"$TARGET_IP" "mkdir -p /etc/ssl/internal && cat > /etc/ssl/internal/${SERVICE}.crt"
        ssh ${SSH_OPTS} root@"$CA_HOST" "cat /etc/step-ca/issued/${SERVICE}/${SERVICE}.key" | \
            ssh ${SSH_OPTS} root@"$TARGET_IP" "mkdir -p /etc/ssl/internal && cat > /etc/ssl/internal/${SERVICE}.key && chmod 600 /etc/ssl/internal/${SERVICE}.key"
        success "  Certificate deployed to ${TARGET_IP}"
    else
        warn "  Could not deploy certificate to ${TARGET_IP} (node unreachable)"
    fi
done

# ==============================================================================
# 6. SETUP AUTOMATIC RENEWAL CRON JOB
# ==============================================================================
info "Configuring automatic certificate renewal..."

ssh ${SSH_OPTS} root@"$CA_HOST" bash -s <<'RENEWAL_EOF'
set -euo pipefail

# Create renewal script
cat > /usr/local/bin/renew-internal-certs.sh <<'SCRIPT'
#!/bin/bash
# Automatic certificate renewal for CMS HA infrastructure
# Runs via cron to renew certificates before they expire

set -uo pipefail
CERT_BASE="/etc/step-ca/issued"
LOG="/var/log/cert-renewal.log"

echo "$(date '+%Y-%m-%d %H:%M:%S') — Starting certificate renewal check" >> "$LOG"

for SERVICE_DIR in "${CERT_BASE}"/*/; do
    SERVICE=$(basename "$SERVICE_DIR")
    CERT="${SERVICE_DIR}/${SERVICE}.crt"
    KEY="${SERVICE_DIR}/${SERVICE}.key"

    if [ ! -f "$CERT" ]; then
        continue
    fi

    # Check if certificate expires within 7 days (168 hours)
    if step certificate needs-renewal "$CERT" --expires-in 168h 2>/dev/null; then
        echo "$(date '+%Y-%m-%d %H:%M:%S') — Renewing certificate for ${SERVICE}..." >> "$LOG"
        step ca renew "$CERT" "$KEY" --force 2>> "$LOG" || \
            echo "$(date '+%Y-%m-%d %H:%M:%S') — [WARN] Renewal failed for ${SERVICE}" >> "$LOG"
    fi
done

echo "$(date '+%Y-%m-%d %H:%M:%S') — Renewal check complete" >> "$LOG"
SCRIPT

chmod +x /usr/local/bin/renew-internal-certs.sh

# Schedule renewal check every 12 hours
CRON_LINE="0 */12 * * * /usr/local/bin/renew-internal-certs.sh"
(crontab -l 2>/dev/null | grep -v 'renew-internal-certs' ; echo "$CRON_LINE") | crontab -

echo "  [OK] Renewal cron job configured (every 12 hours)"
RENEWAL_EOF

success "Automatic certificate renewal configured"

# ==============================================================================
# 7. SUMMARY
# ==============================================================================
echo ""
echo "════════════════════════════════════════════════════════════════"
echo "  Internal CA deployment complete (managed by step-ca)"
echo ""
echo "  CA Server:      https://${CA_HOST}:${CA_PORT}"
echo "  Root CA:        ${CA_DIR}/certs/root_ca.crt"
echo "  Fingerprint:    ${CA_FINGERPRINT:-N/A}"
echo ""
echo "  Issued certificates:"
for SERVICE in "${!CERT_TARGETS[@]}"; do
    echo "    - ${SERVICE} → ${CERT_TARGETS[$SERVICE]}"
done
echo ""
echo "  Renewal: automatic via cron (every 12h)"
echo "  ACME endpoint: https://${CA_HOST}:${CA_PORT}/acme/acme/directory"
echo "════════════════════════════════════════════════════════════════"
