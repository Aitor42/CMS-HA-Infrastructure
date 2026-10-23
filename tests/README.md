# Tests

All black-box and integration tests live in this directory, in the single Go
package `tests`. They exercise the project through its exported API (the
`cms-ha` CLI, the `internal/*` packages, embedded templates, Puppet/Kustomize
manifests, …) and do not need a real hypervisor or cluster to run.

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

| Suite | Covers |
|:------|:-------|
| `internal/dashboards/check_queries_internal_test.go` | `checkQueries` (runtime Prometheus gate) |
| `internal/deploy/state_internal_test.go` | `phaseKey`, `planRun` (checkpoint planning) |
| `internal/phases/backup/backup_test.go` | `garageRunArgs`, `ensureGarageConfig` |
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
| `internal/verify/verify_functional_internal_test.go` | probe parsers, dashboard and PHP checks |
| `internal/verify/verify_internal_test.go` | `countReadyNodes` |
| `internal/httpcheck/httpcheck_test.go` | `Get` (body, Host header, timeout) and `LBReachable` |
| `internal/prom/prom_test.go` | Prometheus sample parsing |
| `internal/drills/drills_test.go` | CronJob creation, wait, diagnostics and cleanup |
| `internal/hypervisor/vmcreate_internal_test.go` | `ClientVMDefinition` |

Any assertion that can be made through the public API belongs here instead: the
checkpoint `State` round-trip, the `TrafficVHost`/`K6Image` accessors and the
`RunCommandStdin` nil-writer guard are covered from `tests/` for that reason.
