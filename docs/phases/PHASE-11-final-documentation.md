# PHASE 11 — Final Documentation

## Objectives to Achieve

- [x] List complete IP address inventory.
- [x] Create operations and disaster recovery manual.
- [x] Document implemented software pins/floors, unpinned dependencies and source URLs (not a measured runtime inventory).
- [x] Implement prepublication CI/PXE gates for tagged delivery (a successful live run must be recorded separately).

---

## Generated Documentation

### IP Inventory

Documented in:
- [`docs/PLAN.md`](../PLAN.md) — Unified node inventory table containing IP, MAC, role, RAM, and disk size.
- [`docs/phases/PHASE-00-network-design.md`](PHASE-00-network-design.md) — Network design and subnet allocation.

### Operations Manual

Documented in [`docs/MANUAL.md`](../MANUAL.md), which covers:
1. Prerequisites (hardware and software requirements)
2. Deployment Procedure (Batch Mode)
3. Deployment Verification
4. Monitoring and Metrics
5. Scaling and Node Expansion
6. Maintenance and Backups
7. Failover and Disaster Recovery
8. Lab Resume Procedure

The manual also contains standalone migration notes for legacy checkpoints,
restricted/pinned fencing keys, allowlisted hard NFS mounts, and bounded PHP /
prefork settings; these do not require separate unfinished reports.

### Software Baseline

Documented in [`docs/SOFTWARE_BASELINE.md`](../SOFTWARE_BASELINE.md), which records:
- Software names, versions, classifications, source URLs, and deployment scopes.
- Covers: Ubuntu Server, Cobbler, Puppet Server & Agent, Nginx, K3s, MariaDB, WordPress, Apache, PHP, Prometheus, Grafana, Prometheus alert rules, exporters, DRBD, UFW, OpenSSL, and libvirt/KVM.

### Network Diagrams

Documented in [`docs/NETWORK_DIAGRAM.md`](../NETWORK_DIAGRAM.md):
- Detailed network topology diagram (Mermaid)
- E2E service architecture and traffic flow diagram (Mermaid)
- Automated deployment sequence chart (Mermaid)

### Repository Tagging and Publication Gates

A pushed `v*` tag triggers `release.yml`. It calls reusable CI for that exact
SHA, then the full hosted ISO/PXE integration workflow with chaos enabled and
teardown required. Only after both succeed may the build/sign/publish job run;
there is no post-publication PXE acceptance shortcut. CI Summary treats skipped
and cancelled dependencies as unsuccessful.

Hosted diagnostics and artifact uploads run after failures as well, before
teardown, with 30-day retention. Check job conclusions and scenario verdicts,
not just whether files exist. `--require-all` rejects `SKIP`; a JSON report marked
`complete` can still contain failures. Neither whole-scenario timings nor the
ephemeral database restore duration proves end-to-end CMS RTO. These are workflow
contracts, not a claim that this revision has passed a runtime release gate.
See [MANUAL integration guidance](../MANUAL.md#37-end-to-end-integration-test).

### Final Repository Structure

```
CMS-HA-Infrastructure/
├── README.md                    # Project general description
├── Makefile                     # Build, deploy, verify and maintenance targets
├── go.mod / go.sum              # Go module definition and pinned dependencies
├── config.yaml                  # Centralised typed configuration (age-encrypted secrets)
├── cmd/cms-ha/                  # Go CLI entry point (Cobra commands)
├── internal/                    # Core Go packages (config, ssh, libvirt, deploy, phases, ...)
├── embed.go                     # Embeds templates/ and kubernetes/ into the binary
├── docs/
│   ├── PLAN.md                  # Comprehensive planning and inventory
│   ├── MANUAL.md                # Operations and administration manual
│   ├── SECURITY.md              # Access hardening and TOFU host-key model
│   ├── LEGACY-V1-MAPPING.md     # v1 shell scripts → v2 commands
│   ├── SOFTWARE_BASELINE.md     # Software version control baseline
│   ├── NETWORK_DIAGRAM.md       # Architectural diagrams (Mermaid)
│   └── phases/PHASE-00..11.md   # Project milestone documentation
├── tests/                       # Go unit/integration test suites
│   ├── *_test.go                # Package-level tests (config, ssh, phases, ...)
│   └── integration/run.sh       # End-to-end lab driver (used by the KVM CI job)
├── ansible/                     # Infra as code: libvirt networks + VM domains (create/declare)
├── puppet/                      # Puppet manifests and role modules
│   ├── manifests/site.pp
│   └── modules/role/manifests/
├── kubernetes/                  # Kubernetes manifests (kustomize)
│   ├── base/                    # MariaDB StatefulSet, PV/PVC, Service, namespace
│   ├── jobs/                    # Initial database bootstrap Job
│   ├── backups/                 # Backup and restore-drill CronJobs
│   ├── monitoring/              # Prometheus RBAC for the cluster
│   └── overlays/lab/            # Lab overlay (node affinity / labels patch)
└── templates/                   # PXE, monitoring, DRBD, nginx and libvirt templates
```
