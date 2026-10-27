# Security Model — CMS High-Availability Infrastructure

This document describes how the orchestrator (`cms-ha`) and the provisioned
nodes are hardened, what is scanned in CI, and which risks are explicitly
accepted.

---

## 1. No direct root access

`cms-ha` **never logs in as `root`**. Every SSH connection uses a non-root
deploy account:

- `ssh.user` (default `ubuntu`) — created by both installers:
  - **Cobbler autoinstall** (`templates/cobbler/ubuntu-24.04-autoinstall.yaml`)
  - **cloud-init** for the jumpstart (`templates/autoinstall/user-data.tmpl`,
    `ansible/roles/cloud_init/templates/user-data.j2`)

  Each installs the operator's public key under the deploy account and disables
  password authentication.
- **Privilege elevation is transparent.** The SSH layer wraps privileged
  commands with `sudo -n -H bash -c '<cmd>'` (see `internal/ssh/client.go`).
  File writes to root-owned paths stream through `sudo` (`cat` into a sidecar,
  `chmod`, atomic `mv`); large uploads and `CopyDir` use the same mechanism.
  `-n` makes `sudo` fail fast instead of blocking on a password prompt, and
  `-H` gives the command root's `HOME` so root-owned kubeconfigs and CA files
  resolve as before.
- **Root logins are disabled.** Every node is provisioned with
  `PermitRootLogin no` (sshd drop-in `01-permitrootlogin.conf`), and Puppet
  enforces the same at `/etc/ssh/sshd_config.d/10-cms-ha-root-login.conf` so
  existing nodes converge.

### Sudo scope

The orchestrator executes **arbitrary privileged commands** (package installs,
`systemctl`, k3s, `drbdadm`, `kubectl`, `ufw`, `step`, …). Command-level sudo
whitelisting is therefore not achievable without breaking the tool. Scope is
enforced instead by:

- a **dedicated, key-only, non-root account** (never a shared root account);
- **all sudo invocations logged** (`Defaults log_output`) via the managed
  drop-in `/etc/sudoers.d/90-cms-ha`;
- `!requiretty` so the non-interactive session works, without weakening auth.

---

## 2. Host-key verification (TOFU)

Host keys are pinned on **first use** and persisted to
`ssh.known_hosts_file` (default `~/.ssh/cms_known_hosts`, mode `0600`):

- first contact with a host → its key is recorded (a `WARN` logs the SHA-256
  fingerprint);
- subsequent contacts → the key must match the pinned value;
- a **mismatch fails closed** with a clear error instead of silently trusting a
  changed key (man-in-the-middle protection).

Implementation: `internal/ssh/client.go` (`verifyHostKey`, `trustHostKey`),
tested in `internal/ssh/client_internal_test.go`.

> To re-provision a node, remove its stale line from the known-hosts file.
> `cms-ha phase repair-ssh` does this automatically for every cluster address in
> **both** `~/.ssh/known_hosts` and the configured `ssh.known_hosts_file` *before*
> it connects, so a re-created VM's new host key is re-pinned on first contact
> instead of failing closed.

---

## 3. Continuous scanning (CI)

The `security-scan` job in `.github/workflows/ci.yml` (and `make security`)
runs:

| Tool | Scope | Gate |
|------|-------|------|
| **gosec** | Go SAST (HIGH severity, MEDIUM confidence) | **Blocking** — build fails on any HIGH finding |
| **govulncheck** | Reachable vulnerabilities in dependencies | **Blocking** |
| **Gitleaks** | Committed secrets in the working tree (`.gitleaks.toml`) | **Blocking** — any finding |
| **Trivy** (`vuln,secret`) | Filesystem CVEs and leaked secrets | **Blocking** — HIGH/CRITICAL |
| **Trivy** (`misconfig`) | Kubernetes manifest posture | Report only (see *Accepted risks*) |

gosec results are also uploaded as SARIF to GitHub code scanning.

### CI supply chain

- Every workflow declares `permissions: contents: read`; only the security job
  raises `security-events: write` for the SARIF upload. Checkout does not
  persist `GITHUB_TOKEN` in the repository's git config
  (`persist-credentials: false`).
- Every action is pinned to a **commit SHA**, not a mutable tag. Dependabot
  (`.github/dependabot.yml`) updates both Go modules and the pinned Actions
  weekly.
- Integration artifacts are retained for **7 days**, not 14: they contain
  diagnostics and guest console captures, not release deliverables.
- The hosted runner grants `/dev/kvm` and the libvirt socket to the runner user
  with **ACLs** where the filesystem supports them (owner/group-scoped
  fallback otherwise); it no longer makes either world-writable
  (`chmod 0666/0777`). Diagnostics SSH probes verify the pinned host keys
  instead of disabling host-key checking.

Dependencies are kept current: `golang.org/x/crypto` is pinned at **v0.56.0**
or newer, which clears GO-2026-6354 / GO-2026-6355 (DoS in `x/crypto/ssh`).
`go.mod` requires Go **1.26**.

---

## 4. PHP runtime patching (CVE-2026-109618 / CVE-2026-109619)

On **2026-10-21** the PHP project published two unauthenticated remote code
executions in the PHP core, tracked as **CVE-2026-109618** (memory corruption
when an out-of-memory bailout interrupts a request) and **CVE-2026-109619**
(stale global state left behind by a fatal error). The pair is known as
*"php2shell"* and is rated **CVSS 9.2**. Every PHP that answers web requests
is affected — PHP-FPM, Apache with `mod_php`, `php-cgi`, FrankenPHP and
LiteSpeed — and the PHP team considers **`mod_php` the highest-risk setup**,
because a single unauthenticated request can trigger the bug before any
application code runs.

The CMS frontends are exactly that setup: Apache 2.4 with `mod_php` serving
WordPress, reachable through the nginx VIP.

### The fix

| Layer | What it does |
|-------|--------------|
| **Package (the fix)** | `role::cms_frontend` pins `php8.3-cli`, `php8.3-common` and `libapache2-mod-php8.3` to **≥ 8.3.6-0ubuntu0.24.04.13**, the Ubuntu Noble revision that carries `CVE-2026-109618_109619.patch`. Ubuntu backports the upstream patch instead of bumping the upstream version, so a patched node still answers `PHP 8.3.6`: the Debian revision is what tells the two apart. A guarded `apt-get update` (skipped once every pinned package satisfies the floor) makes the patched release visible before the packages are resolved. |
| **PHP ini** | `/etc/php/8.3/apache2/conf.d/99-cms-ha-php-hardening.ini` sets `session.upload_progress.enabled = Off`. The Apache SAPI reads it at startup, so it is in place *before* the request is parsed — the PHP team asks for exactly this, and not for `ini_set()` (too late) nor for a `.user.ini` (per request). It closes the session path CVE-2026-109619 was confirmed with. |
| **Apache** | `/etc/apache2/conf-enabled/php-hardening.conf` sets `php_admin_value memory_limit -1` (removes the user-controllable out-of-memory bailout CVE-2026-109618 abuses — `php_admin_value` cannot be overridden by the application nor from `.htaccess`), `php_admin_flag session.upload_progress.enabled Off`, `php_admin_value post_max_size 32M`, `upload_max_filesize 16M` and `LimitRequestBody 33554432`. |
| **nginx** | `client_max_body_size 32m` bounds the request at the proxy, so the same limit applies at every layer and a single request can never make PHP ask for more memory than the node has. |

Only the package upgrade is the fix; the rest are the mitigations the PHP team
and CERT-EU publish for the case where a node has not been patched yet.

### Proving it

`cms-ha verify` asks every frontend for the installed revision of
`php8.3-cli`, `php8.3-common` and `libapache2-mod-php8.3`, for whether Apache
loads `mod_php`, and for the mitigations, and **fails the phase** when any
pinned package is older than the floor or a mitigation is missing
(`internal/verify/verify_functional.go`, `checkPHPSecurityPatch`). Checking the
Apache SAPI as well as the CLI matters: a patched CLI alone would leave
`mod_php` serving the vulnerable library. The version comparison is delegated
to `dpkg --compare-versions` on the node itself, so the answer follows
Debian's own version rules.

### Keeping it patched

The floor lives in three places that have to move together when the next PHP
security update lands (`TestPHPPatchFloorIsInSyncWithPuppet` fails if the
Puppet and Go copies drift):

| Where | What to change |
|-------|----------------|
| `puppet/modules/role/manifests/cms_frontend.pp` | `$php_patched_version` |
| `internal/verify/verify_functional.go` | `phpPatchedPackage` |
| `docs/SOFTWARE_BASELINE.md` | the *PHP Engine* row |

---

## 5. Accepted risks

These are known and tracked; each is either a deliberate trade-off or a
follow-up that needs a live cluster to validate safely.

1. **Broad passwordless sudo for the deploy account.** Required by design (see
   *No direct root access*). Mitigated by the dedicated account, key-only auth,
   disabled root logins and sudo logging.
2. **Kubernetes `misconfig` HIGH findings (Trivy `KSV-0014`, `KSV-0118`)** on
   the MariaDB StatefulSet and the backup/restore CronJobs. The official
   `mariadb` image needs a **writable root filesystem** and a **root
   entrypoint** (it chowns the datadir and then drops to `mysql`), so
   `readOnlyRootFilesystem: true` / `runAsNonRoot: true` cannot be added
   safely without a live cluster to validate the datadir ownership and the
   emptyDir mounts. The scan stays **enabled and visible** (report-only) so the
   debt is not hidden; hardening is a follow-up to be done against real
   hardware.
3. **fence SSH key** on the DRBD masters targets the hypervisor's **operator
   account** (not root) and is scoped to `virsh` lifecycle calls; it is a
   separate trust path from the VM deploy account.
4. **`memory_limit = -1` on the frontends.** This is the mitigation the PHP
   team asks for: a finite limit is exactly the out-of-memory bailout
   CVE-2026-109618 abuses. The request size is bounded at every layer
   (`client_max_body_size`, `LimitRequestBody`, `post_max_size`), so a request
   cannot ask for more memory than the node has, and the kernel's own OOM
   killer remains the last boundary if something does. The alternative — a
   cgroup limit on `apache2.service` — was not adopted because a cgroup OOM
   would stop the whole service instead of a single worker, which trades an
   RCE for an outage on a 768 MB node.
5. **VM disk images are mode `0666` on the hypervisor.** The deploy account and
   the libvirt qemu account can be different users, and the current image
   creation does not assume the ability to chown. This is acceptable only on the
   dedicated, single-operator hypervisor the lab targets; a multi-user host must
   use libvirt dynamic ownership or an ACL scoped to the qemu account before
   running this topology.
6. **K3s is installed with the upstream `https://get.k3s.io` script.** The
   channel is pinned to `v1.29` and fetched over TLS, but the installer resolves
   the latest patch and is executed as root. A fully pinned binary plus a
   published checksum is the intended follow-up; it needs a live cluster to
   validate the upgrade path.

---

## 6. Configuring

```yaml
ssh:
  user: "ubuntu"                                  # non-root deploy account
  known_hosts_file: "${HOME}/.ssh/cms_known_hosts" # TOFU store
  private_key: "${HOME}/.ssh/id_ed25519_gar"
```

`ssh.user` is validated as a conservative POSIX user name; the value flows into
the Ansible inventory (`ansible_user` + `ansible_become`) and the SSH pool.

---

## 7. Secrets management (SOPS / ExternalSecrets)

SOPS and the ExternalSecrets operator were evaluated but are **not adopted** in
the current design:

- **Host secrets** (`config.yaml` service passwords, `backup.secret_key`) are
  `age`-encrypted on first load, with the identity kept outside the repository.
  The private key must be `0600`; `cms-ha` refuses to use a key that is readable
  by group or other.
- **In-cluster secrets** (MariaDB credentials, `backup-secret`) are generated by
  the `kubernetes` phase from the decrypted `config.yaml` and applied only to
  the cluster — they are never committed.
- **Grafana's admin password** is generated per deployment by the monitoring
  phase (256 bits, URL-safe) and written to
  `/etc/grafana/grafana-admin.env` on the monitor node (root:grafana, 0640).
  The monitoring phase also runs `grafana cli admin reset-admin-password`, so
  the default `admin/admin` does not survive a deployment or a re-run. Setting
  `monitoring.grafana_admin_password` in `config.yaml` overrides the generated
  value and is encrypted by `cms-ha secrets encrypt`.
- **Certificates** are issued by the internal step-ca and distributed by the
  orchestrator; no long-lived private material lives in the repository.

### Rotating the age identity

`cms-ha secrets rotate` re-encrypts every secret field with a new age identity:

```bash
# Generate a new keypair and install it at ~/.config/cms-ha/age.key
cms-ha secrets rotate

# Or use an existing private key (for example, from a team key store)
cms-ha secrets rotate --new-key /secure/team.age.key
```

The command writes `config.yaml.bak-<timestamp>` before touching the config and
keeps the previous default key as `age.key.bak-<timestamp>`. A failed decrypt
aborts before the config is replaced. This rotates **encryption at rest**, not
the service passwords themselves: rotate MariaDB, WordPress and CA credentials
with their own procedures (see `docs/MANUAL.md`), then re-run the affected
phases. Keep the timestamped backups until the new key is verified on every
operator workstation; the old key is the only way to recover the old config.

`gitleaks` runs in CI and can be run locally with the same configuration:

```bash
gitleaks detect --source . --no-git --config .gitleaks.toml --redact --exit-code 1
```

### Puppet certificate signing

The Puppet server does **not** autosign the whole `*.internal.local` and
`*.main.local` domains. The phase writes `/etc/puppetlabs/puppet/autosign.conf`
with the exact FQDNs from the configured topology immediately before the agents
request certificates. A host that is not in the configuration cannot obtain a
signed catalog, even if it can reach the server. `templates/puppet/autosign.conf`
is wildcard-free as well; it requires explicit signing if anyone uses that
legacy template.

Introducing SOPS (git-level encryption) or ExternalSecrets (vault-driven) would
add an operator dependency without removing the files we already keep out of
git. Revisit if secrets ever need to be versioned or fetched from an external
vault.
