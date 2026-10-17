# Tests

All black-box and integration tests live in this directory, in the single Go
package `tests`. They exercise the project through its exported API (the
`cms-ha` CLI, the `internal/*` packages, embedded templates, Puppet/Kustomize
manifests, …) and do not need a real hypervisor or cluster to run.

## Layout

```
tests/
├── *_test.go            # black-box unit/integration tests (package tests)
├── integration/         # end-to-end run.sh (build → deploy → verify → failover)
└── README.md
```

Run them with:

```bash
go test ./...            # everything (including the white-box suites below)
go test ./tests/         # just this directory
```

## White-box suites (by design, next to their package)

Four suites assert *unexported* helpers and are therefore co-located with the
code they test: Go only lets a `_test.go` file reach unexported identifiers when
it is declared in the same package **and directory**. Moving them into `tests/`
would require exporting internal helpers purely for testing, so they stay put:

| Suite | Tests |
|:------|:------|
| `internal/deploy/state_internal_test.go` | `phaseKey`, `planRun` (checkpoint planning) |
| `internal/phases/repairssh/repair_internal_test.go` | `removeHostsFromKnownHosts` |
| `internal/phases/traffic/traffic_internal_test.go` | `resolveBaseURL`, `buildArgs` (k6 invocation) |
| `internal/ssh/client_internal_test.go` | `shellQuote`, `remoteCommand`, `verifyHostKey` (TOFU) |

Any assertion that can be made through the public API belongs here instead: the
checkpoint `State` round-trip, the `TrafficVHost`/`K6Image` accessors and the
`RunCommandStdin` nil-writer guard are covered from `tests/` for that reason.
