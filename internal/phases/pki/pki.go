package pki

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// Phase implements the PKI setup phase
type Phase struct {
	cfg  *config.Config
	pool *ssh.Pool
}

// NewPhase creates a new PKI Phase
func NewPhase(cfg *config.Config, pool *ssh.Pool) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

// Description returns the phase description
func (p *Phase) Description() string {
	return "Sets up Internal CA with step-ca"
}

// Name returns the phase name
func (p *Phase) Name() string {
	return "Internal CA Setup"
}

// Run runs the setup
func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()
	caPass := p.cfg.PKI.ProvisionerPassword
	if caPass == "" {
		return fmt.Errorf("pki.provisioner_password must be set in config.yaml so CA restarts use the same key password")
	}

	jumpIP := p.cfg.Nodes.Jumpstart.IP

	logging.Info("Installing step-cli and step-ca on Jumpstart...")
	// The release assets are pinned by pki.step_version / pki.step_ca_version.
	// Downloading whatever "latest" happens to point at made a fresh deploy
	// depend on that day's Smallstep release while the configuration claimed a
	// version, so a future release that changes the CLI surface (or the Debian
	// asset name) would break exactly the hosts that start from scratch.
	//
	// The asset name is not stable across releases: step-cli ships
	// step-cli_<v>-1_<arch>.deb while step-ca has published both
	// step-ca_<v>-1_<arch>.deb and step-ca_<v>_<arch>.deb, so every known shape
	// is tried and the version-independent alias is used only when the version
	// is left empty. A pin that cannot be downloaded is an error, not a reason
	// to install something else.
	installCmd := fmt.Sprintf(`set -e
if ! command -v step >/dev/null 2>&1 || ! command -v step-ca >/dev/null 2>&1; then
	ARCH=$(dpkg --print-architecture 2>/dev/null || echo "amd64")
	install_deb() {
		out="$1"; shift
		for url in "$@"; do
			if wget -q "$url" -O "$out"; then
				dpkg -i "$out" || apt-get install -f -y
				rm -f "$out"
				return 0
			fi
			echo "  no such release asset: $url" >&2
		done
		return 1
	}
	install_deb /tmp/step-cli.deb %s
	install_deb /tmp/step-ca.deb %s
fi`,
		quoteURLs(stepReleaseURLs("cli", "step-cli", p.cfg.PKI.StepVersion)),
		quoteURLs(stepReleaseURLs("certificates", "step-ca", p.cfg.PKI.StepCAVersion)))
	if _, _, code, err := p.pool.RunCommand(ctx, jumpIP, installCmd); err != nil || code != 0 {
		return fmt.Errorf("failed to install step-ca (exit %d): %v", code, err)
	}

	logging.Info("Initializing step-ca...")
	caPort := p.cfg.PKI.CAPort
	if caPort == 0 {
		caPort = 8443
	}
	caDomain := p.cfg.PKI.Domain
	if caDomain == "" {
		caDomain = "ca.internal.local"
	}

	// Prepare step directory and write password file securely with 0600 permissions
	if _, _, _, err := p.pool.RunCommand(ctx, jumpIP, "mkdir -p /root/.step && chmod 700 /root/.step"); err != nil {
		return fmt.Errorf("failed to prepare step directory: %w", err)
	}
	if err := p.pool.CopyContent(ctx, jumpIP, []byte(caPass+"\n"), "/root/.step/password.txt", 0600); err != nil {
		return fmt.Errorf("failed to upload CA password: %w", err)
	}

	initCmd := fmt.Sprintf(`export STEPPATH=/root/.step && \
[ -f /root/.step/certs/root_ca.crt ] || step ca init --name="%s" --dns="%s,%s" \
--address=":%d" --provisioner="admin" --password-file=/root/.step/password.txt --with-ca-url="https://%s:%d" --deployment-type=standalone`,
		p.cfg.PKICommonName(), caDomain, jumpIP, caPort, jumpIP, caPort)

	// Idempotency is guaranteed by the `[ -f ... ] ||` guard inside initCmd:
	// when the root CA already exists, `step ca init` is not executed and the
	// command exits 0. Any non-zero exit here is therefore a real failure.
	initOut, initErr, initCode, err := p.pool.RunCommand(ctx, jumpIP, initCmd)
	if initCode != 0 {
		return fmt.Errorf("failed to initialize step-ca (exit %d): %v %s%s", initCode, err, strings.TrimSpace(initOut), strings.TrimSpace(initErr))
	}

	logging.Info("Starting step-ca service...")
	serviceCmd := `cat << 'EOF' > /etc/systemd/system/step-ca.service
[Unit]
Description=step-ca
After=network.target

[Service]
ExecStart=/usr/bin/step-ca /root/.step/config/ca.json --password-file /root/.step/password.txt
Restart=on-failure
Environment="STEPPATH=/root/.step"

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload && systemctl enable --now step-ca`

	if _, _, code, err := p.pool.RunCommand(ctx, jumpIP, serviceCmd); err != nil || code != 0 {
		return fmt.Errorf("failed to install/enable step-ca systemd service (exit %d): %v", code, err)
	}

	logging.Info("Waiting for CA health endpoint...")
	err = retry.DoContext(ctx, retry.Config{MaxAttempts: 15, Interval: 2 * time.Second, Timeout: 60 * time.Second}, func(attemptCtx context.Context) error {
		_, _, _, err := p.pool.RunCommand(attemptCtx, jumpIP, fmt.Sprintf("curl -kf https://localhost:%d/health", caPort))
		return err
	})
	if err != nil {
		return fmt.Errorf("step-ca health check failed: %w", err)
	}

	// Raise the provisioner's x509 durations so the load-balancer certificate
	// can outlive the verify threshold. step-ca defaults to a 24h maximum, so
	// without this the issuance below is rejected outright ("requested duration
	// ... is more than the authorized maximum certificate duration").
	//
	// This has to happen after the service is up: `step ca provisioner update`
	// is not an offline edit of ca.json, it talks to the CA's admin API, and the
	// CA only picks the new limits up on restart.
	certHours := p.cfg.LBTLSCertLifetimeHours()
	durationsCmd := fmt.Sprintf(`export STEPPATH=/root/.step && step ca provisioner update admin \
--x509-min-dur 24h --x509-default-dur %dh --x509-max-dur 8760h \
--ca-url "https://%s:%d" --root /root/.step/certs/root_ca.crt`, certHours, caDomain, caPort)
	if updOut, updErr, updCode, err := p.pool.RunCommand(ctx, jumpIP, durationsCmd); err != nil || updCode != 0 {
		return fmt.Errorf("failed to raise the CA provisioner x509 durations to %dh (exit %d): %v %s%s",
			certHours, updCode, err, strings.TrimSpace(updOut), strings.TrimSpace(updErr))
	}
	if _, _, _, err := p.pool.RunCommand(ctx, jumpIP, "systemctl restart step-ca"); err != nil {
		return fmt.Errorf("failed to restart step-ca after raising the certificate durations: %w", err)
	}
	logging.Info("Waiting for CA health endpoint after reloading the provisioner...")
	if err := retry.DoContext(ctx, retry.Config{MaxAttempts: 15, Interval: 2 * time.Second, Timeout: 60 * time.Second}, func(attemptCtx context.Context) error {
		_, _, _, rerr := p.pool.RunCommand(attemptCtx, jumpIP, fmt.Sprintf("curl -kf https://localhost:%d/health", caPort))
		return rerr
	}); err != nil {
		return fmt.Errorf("step-ca health check failed after reloading the provisioner: %w", err)
	}

	logging.Info("Distributing root CA cert to all nodes...")
	caCert, _, _, err := p.pool.RunCommand(ctx, jumpIP, "cat /root/.step/certs/root_ca.crt")
	if err != nil {
		return fmt.Errorf("failed to read root CA: %w", err)
	}

	var allIPs []string
	for _, node := range p.cfg.AllNodes() {
		if node.IP != "" && node.IP != jumpIP {
			allIPs = append(allIPs, node.IP)
		}
	}

	tasks := make(map[string]func(ctx context.Context, pool *ssh.Pool) error)
	certBytes := []byte(caCert)
	for _, ip := range allIPs {
		nodeIP := ip
		tasks[nodeIP] = func(c context.Context, pool *ssh.Pool) error {
			if err := pool.CopyContent(c, nodeIP, certBytes, "/usr/local/share/ca-certificates/cms_root_ca.crt", 0644); err != nil {
				return err
			}
			// Same task as the copy: this halves the fan-out waves and keeps
			// the CA trust update ordered after the file it consumes.
			if _, _, code, err := pool.RunCommand(c, nodeIP, "update-ca-certificates"); err != nil || code != 0 {
				return fmt.Errorf("update-ca-certificates (exit %d): %w", code, err)
			}
			return nil
		}
	}
	for _, r := range p.pool.RunParallelFunc(ctx, tasks) {
		if r.Err != nil {
			return fmt.Errorf("failed to distribute root CA to %s: %w", r.Host, r.Err)
		}
	}

	if err := p.issueLoadBalancerCertificates(ctx); err != nil {
		return err
	}

	logging.Success("PKI Setup completed successfully.")
	return nil
}

// issueLoadBalancerCertificates issues the load-balancer certificate on the CA
// host (where the admin provisioner credential already lives) and distributes
// it to every load balancer. Doing it here — rather than on the balancers —
// keeps the CA provisioner credential off the LBs entirely; steady-state
// renewal (`step ca renew`) authenticates with mTLS and needs no secret.
func (p *Phase) issueLoadBalancerCertificates(ctx context.Context) error {
	lbs := p.cfg.Nodes.LBs
	if len(lbs) == 0 {
		return nil
	}
	jumpIP := p.cfg.Nodes.Jumpstart.IP
	if jumpIP == "" {
		return fmt.Errorf("cannot issue load-balancer certificate: jumpstart IP is not set")
	}

	certHours := p.cfg.LBTLSCertLifetimeHours()
	dnsNames := p.cfg.LBTLSDNSNames()
	if len(dnsNames) == 0 {
		return fmt.Errorf("cannot issue load-balancer certificate: pki.cert_dns_names is empty")
	}
	subject := dnsNames[0]

	// The primary DNS name (dnsNames[0]) is passed positionally as the subject,
	// but `step ca certificate` only puts the subject in the CommonName: it is
	// added to the SAN extension *only* when no --san flag is given at all. With
	// any --san present the subject has to be listed explicitly, otherwise the
	// certificate has no DNS SAN and every modern TLS client (which ignores the
	// CN) rejects it for the very name the balancer is meant to serve.
	sanFlags := strings.Builder{}
	for _, san := range p.cfg.LBTLSSANs() {
		if san == "" {
			continue
		}
		if ip := net.ParseIP(san); ip == nil && !validDNSName(san) {
			return fmt.Errorf("refusing to use unsafe certificate SAN %q", san)
		}
		sanFlags.WriteString(" --san ")
		sanFlags.WriteString(san)
	}

	logging.Info("Issuing load-balancer certificate from the internal CA...")
	caDomain := p.cfg.PKI.Domain
	if caDomain == "" {
		caDomain = "ca.internal.local"
	}
	issueCmd := fmt.Sprintf(`export STEPPATH=/root/.step && step ca certificate "%s" /tmp/cms-lb.crt /tmp/cms-lb.key \
--ca-url "https://%s:%d" --root /root/.step/certs/root_ca.crt \
--provisioner admin --password-file /root/.step/password.txt \
--not-after %dh --force%s`, subject, caDomain, caPortOrDefault(p.cfg.PKI.CAPort), certHours, sanFlags.String())

	if out, errOut, code, err := p.pool.RunCommand(ctx, jumpIP, issueCmd); err != nil || code != 0 {
		return fmt.Errorf("failed to issue load-balancer certificate (exit %d): %v %s%s",
			code, err, strings.TrimSpace(out), strings.TrimSpace(errOut))
	}

	// Read certificate and key in a single session, separated by a marker.
	bundle, _, _, err := p.pool.RunCommand(ctx, jumpIP, "cat /tmp/cms-lb.crt; printf '\\n===CMSKEY===\\n'; cat /tmp/cms-lb.key")
	if err != nil {
		return fmt.Errorf("failed to read issued certificate: %w", err)
	}
	certPEM, keyPEM, ok := strings.Cut(bundle, "===CMSKEY===")
	certPEM = strings.TrimSpace(certPEM)
	keyPEM = strings.TrimSpace(keyPEM)
	if !ok || certPEM == "" || keyPEM == "" {
		return fmt.Errorf("issued certificate or key is empty")
	}

	// Wipe the CA-host copies: the key now only lives on the balancers.
	defer func() {
		_, _, _, _ = p.pool.RunCommand(ctx, jumpIP, "rm -f /tmp/cms-lb.crt /tmp/cms-lb.key")
	}()

	for _, lb := range lbs {
		if lb.IP == "" {
			continue
		}
		if err := p.pool.CopyContent(ctx, lb.IP, []byte(certPEM), "/etc/nginx/ssl/cms.crt", 0600); err != nil {
			return fmt.Errorf("failed to install certificate on %s: %w", lb.Name, err)
		}
		if err := p.pool.CopyContent(ctx, lb.IP, []byte(keyPEM), "/etc/nginx/ssl/cms.key", 0600); err != nil {
			return fmt.Errorf("failed to install certificate key on %s: %w", lb.Name, err)
		}
		reload := "chown root:root /etc/nginx/ssl/cms.crt /etc/nginx/ssl/cms.key && nginx -t && systemctl reload nginx"
		if out, errOut, code, err := p.pool.RunCommand(ctx, lb.IP, reload); err != nil || code != 0 {
			return fmt.Errorf("failed to reload nginx on %s with the new certificate (exit %d): %v %s%s",
				lb.Name, code, err, strings.TrimSpace(out), strings.TrimSpace(errOut))
		}
	}

	logging.Success("Load-balancer certificate issued by the internal CA and distributed.")
	return nil
}

// stepReleaseURLs returns the candidate download URLs for a step release. Both
// known Debian asset shapes are returned for a pinned version; when the version
// is empty the version-independent alias is used instead. ${ARCH} is expanded by
// the remote shell, not here.
func stepReleaseURLs(repo, asset, version string) []string {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	base := fmt.Sprintf("https://github.com/smallstep/%s/releases", repo)
	if v == "" {
		return []string{fmt.Sprintf("%s/latest/download/%s_${ARCH}.deb", base, asset)}
	}
	return []string{
		fmt.Sprintf("%s/download/v%s/%s_%s-1_${ARCH}.deb", base, v, asset, v),
		fmt.Sprintf("%s/download/v%s/%s_%s_${ARCH}.deb", base, v, asset, v),
	}
}

// quoteURLs renders candidate URLs for the remote shell, double-quoted so the
// shell still expands ${ARCH} inside them.
func quoteURLs(urls []string) string {
	quoted := make([]string, 0, len(urls))
	for _, u := range urls {
		quoted = append(quoted, `"`+u+`"`)
	}
	return strings.Join(quoted, " ")
}

// caPortOrDefault returns the CA port, applying the same 8443 default used
// during CA initialisation.
func caPortOrDefault(port int) int {
	if port == 0 {
		return 8443
	}
	return port
}

// validDNSName reports whether s is a syntactically valid DNS name safe to
// interpolate into the remote command unquoted beyond the double quotes.
func validDNSName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			case r == '-' && i != 0 && i != len(label)-1:
			default:
				return false
			}
		}
	}
	return true
}
