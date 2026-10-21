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
| **Trivy** (`vuln,secret`) | Filesystem CVEs and leaked secrets | **Blocking** — HIGH/CRITICAL |
| **Trivy** (`misconfig`) | Kubernetes manifest posture | Report only (see *Accepted risks*) |

gosec results are also uploaded as SARIF to GitHub code scanning.

Dependencies are kept current: `golang.org/x/crypto` is pinned at **v0.56.0**
or newer, which clears GO-2026-6354 / GO-2026-6355 (DoS in `x/crypto/ssh`).
`go.mod` requires Go **1.26**.

---

## 4. Accepted risks

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

---

## 5. Configuring

```yaml
ssh:
  user: "ubuntu"                                  # non-root deploy account
  known_hosts_file: "${HOME}/.ssh/cms_known_hosts" # TOFU store
  private_key: "${HOME}/.ssh/id_ed25519_gar"
```

`ssh.user` is validated as a conservative POSIX user name; the value flows into
the Ansible inventory (`ansible_user` + `ansible_become`) and the SSH pool.

---

## 6. Secrets management (SOPS / ExternalSecrets)

SOPS and the ExternalSecrets operator were evaluated but are **not adopted** in
the current design:

- **Host secrets** (`config.yaml` service passwords, `backup.secret_key`) are
  `age`-encrypted on first load, with the identity kept outside the repository.
- **In-cluster secrets** (MariaDB credentials, `backup-secret`) are generated by
  the `kubernetes` phase from the decrypted `config.yaml` and applied only to
  the cluster — they are never committed.
- **Certificates** are issued by the internal step-ca and distributed by the
  orchestrator; no long-lived private material lives in the repository.

Introducing SOPS (git-level encryption) or ExternalSecrets (vault-driven) would
add an operator dependency without removing the files we already keep out of
git. Revisit if secrets ever need to be versioned or fetched from an external
vault.
