# Tests

The repository-level Go tests live in this directory, in the single Go
package `tests`. They exercise exported APIs (the `cms-ha` CLI and `internal/*`
packages), embedded templates, Puppet/Kustomize manifests and workflow contracts.
These tests use fakes/temporary directories and do not need a real hypervisor or
cluster, apart from the opt-in live Prometheus gate below. The separate
`integration/run.sh` driver **does** operate a real KVM lab.

## Layout

```
tests/
├── *_test.go                       # black-box tests (package tests)
├── dashboards_manual_gate_test.go  # the one opt-in test (live Prometheus)
├── integration/run.sh              # end-to-end build → deploy → verify → failover
└── README.md
```

One file covers one area (`config_test.go`, `phases_test.go`, …). Test names
follow `Test<Area>_<Behaviour>` so a failure names the subsystem, and the
`repoFile` helper resolves repository paths independently of the package
working directory.

Run them with:

```bash
go test ./...            # everything (including the white-box suites below)
go test ./tests/         # just this directory
```

The one test that needs a deployed lab is opt-in:

```bash
CMS_GATE_PROM=http://192.168.10.20:9090 go test ./tests -run ManualGate -v
```

## White-box suites (by design, next to their package)

These suites assert *unexported* helpers, so they stay in their package
directory: Go only lets a `_test.go` file reach unexported identifiers when it
is declared in the same package **and** directory. Moving them into `tests/`
would require exporting internal helpers purely for testing.

The table lists representative suites, not every test file.

| Suite | Covers |
|:------|:-------|
| `internal/dashboards/check_queries_internal_test.go` | `checkQueries` (runtime Prometheus gate) |
| `internal/deploy/state_internal_test.go` | unique phase keys, suffix resets and completed vs assumed checkpoint planning |
| `internal/deploy/identity_internal_test.go` | configuration/executable/host identity, legacy or mismatched resume rejection before preflight |
| `internal/phases/backup/backup_test.go` | `garageRunArgs`, `ensureGarageConfig` |
| `internal/phases/drbd/fencing_test.go` | restricted authorized-key migration, domain allowlisting, persistent host-key pins and unsafe-path rejection |
| `internal/phases/initvms/initvms_internal_test.go` | cloud-image selection, cloud interface mapping and rendered client user-data |
| `internal/phases/kubernetes/kubernetes_internal_test.go` | `countNonEmptyLines`, `isTransientAPIError` |
| `internal/phases/pki/pki_internal_test.go` | `stepReleaseURLs`, `quoteURLs` |
| `internal/phases/routerbootstrap/routerbootstrap_test.go` | `netplanBootstrapScript`, `netplanRestoreScript`, `withPrefix` |
| `internal/phases/traffic/traffic_internal_test.go` | `resolveBaseURL`, `buildArgs`, `firstExistingFile` |
| `internal/ssh/client_internal_test.go` | `shellQuote`, `remoteCommand`, host-key TOFU, elevated writes |
| `internal/ssh/knownhosts_internal_test.go` | `removeHostsFromKnownHosts` |
| `internal/hypervisor/bootorder_internal_test.go` | `reorderBootElements` |
| `internal/hypervisor/install_by_batches_internal_test.go` | `planInstallBatches`, `chunkInstallNodes` |
| `internal/verify/verify_functional_http_internal_test.go` | CMS/load-balancer/monitoring probes |
| `internal/verify/verify_functional_internal_test.go` | probe parsers, dashboard checks, PHP package floor, 128M memory and four prefork workers |
| `internal/utils/failover_test.go` | fake-driven failover, restoration, cancellation, strict SKIP handling and atomic JSON evidence |
| `internal/verify/verify_internal_test.go` | `countReadyNodes` |
| `internal/httpcheck/httpcheck_test.go` | `Get` (body, Host header, timeout) and `LBReachable` |
| `internal/prom/prom_test.go` | Prometheus sample parsing |
| `internal/drills/drills_test.go` | CronJob creation, wait, diagnostics and cleanup |
| `internal/hypervisor/vmcreate_internal_test.go` | `ClientVMDefinition` |

Any assertion that can be made through the public API belongs here instead: the
checkpoint `State` round-trip, the `TrafficVHost`/`K6Image` accessors and the
`RunCommandStdin` nil-writer guard are covered from `tests/` for that reason.

## Safety regression coverage

- `storage_safety_test.go` exercises uploads mount identity checks and controlled
  legacy-soft-to-hard migration with fake mount/systemd commands; it also checks
  Apache mount dependencies and local-directory protection. It is not a live NFS
  outage test.
- `fencing_security_test.go` exercises the forced-command boundary and checks
  strict pinned-key SSH, rather than granting a fencing key general shell access.
- `network_isolation_test.go` checks generated frontend allowlists, NFS export
  and firewall scoping, and K3s API/NodePort subnet restrictions.
- `reliability_controls_test.go` checks failover CLI flags, strict integration
  acceptance and restore scripts with a deterministic clock. The restore metric
  includes S3 listing/download and ephemeral DB recovery/validation; it is not
  end-to-end CMS RTO.
- `ci_safety_test.go` checks fail-closed CI Summary results, exact-SHA reusable
  CI/PXE gates before publication, evidence handling and resume without rebuild.

## Live integration and evidence

`tests/integration/run.sh` (or `make integration`) builds and deploys the lab,
then verifies and runs failover; `--with-traffic` and `--with-drills` add those
stages. Its preflight requires `virsh`, `docker`, `/dev/kvm` and a configuration.
This is disruptive and is not part of `go test ./...`.

`--resume` requires the existing checkpoint and original executable and skips
`make build`: checkpoint identity binds progress to the executable, host and
non-secret deployment inputs. Keep the same config and provisioning flags;
inspect the lab before migrating old/foreign progress with a new state file
and explicit `--from-phase`. See [MANUAL migration notes](../docs/MANUAL.md#step-3-service-and-application-deployment).

Failover runs with `--require-all` and writes `failover.json` plus combined
stdout/stderr in `failover.log`; verify, traffic and drill logs also capture
stderr, and `pipefail` preserves failures through `tee`. Later acceptance stages
do not run after an earlier failure. `PASS`, `FAIL` and `SKIP` are distinct:
`SKIP` fails strict acceptance, `complete` only means all scenarios were visited,
and cancellation can leave a partial report. Missing evidence is not success.
Scenario durations include preflight and restoration, not the CMS outage interval.

The release workflow runs reusable CI on the tagged SHA, then a full hosted
ISO/PXE run (`fast_provisioning=false`, chaos enabled, teardown required) before
building/signing/publishing. Ordinary hosted cloud/provision-only runs are not
this acceptance gate. Hosted diagnostics and artifact uploads run with
`always()` before teardown and retain available evidence for 30 days; failed
teardown blocks the release too. CI Summary rejects skipped/cancelled jobs as
well as failures. These are implemented gates, not claims that a current live
run has passed.
