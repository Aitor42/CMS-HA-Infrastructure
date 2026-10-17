# `scripts/` — DEPRECATED (v1, reference only)

> ⚠️ **These shell scripts are no longer the deployment path.** They are kept
> for historical reference only and are **not maintained**: `config.sh` and the
> numbered scripts predate the Go orchestrator and have already drifted from it.
> Do not add features or fixes here.

Everything these scripts did is now implemented by the **v2** path:

- **`cms-ha`** (Go orchestrator) — the single entry point (`cms-ha deploy` and
  the per-phase commands).
- **Ansible** (`ansible/`) — declares and creates the infrastructure
  (networks, disks, domains) from `config.yaml`.
- **Puppet** (`puppet/`) — node desired state.

`config.yaml` is the single source of truth (the equivalents of this directory's
`config.sh`).

## v1 → v2 mapping

| Deprecated script | v2 replacement |
|:--|:--|
| `scripts/config.sh` | `config.yaml` (single source of truth) |
| `scripts/00_init_vms.sh` | `cms-ha deploy` (init-vms phase) · `ansible/playbooks/provision.yml` |
| `scripts/01_setup_cobbler.sh` | `cms-ha phase setup-cobbler` |
| `scripts/02_register_cobbler_nodes.sh` | `cms-ha phase register-nodes` |
| `scripts/03_repair_ssh_puppet.sh` | `cms-ha phase repair-ssh` · `cms-ha repair` |
| `scripts/04_setup_puppet.sh` | `cms-ha phase setup-puppet` |
| `scripts/05_setup_drbd.sh` | `cms-ha phase setup-drbd` |
| `scripts/06_setup_kubernetes.sh` | `cms-ha phase setup-kubernetes` |
| `scripts/07_setup_nginx_wordpress.sh` | `cms-ha phase setup-nginx` |
| `scripts/08_setup_monitoring.sh` | `cms-ha phase setup-monitoring` |
| `scripts/09_setup_ufw.sh` | `cms-ha phase setup-ufw` |
| `scripts/10_setup_internal_ca.sh` | `cms-ha phase setup-pki` |
| `scripts/11_traffic_mix.sh` | `cms-ha traffic` (k6) |
| `scripts/start_all_vms.sh` | `cms-ha vm start` |
| `scripts/utils/init_mariadb_db.sh` | `kubernetes/jobs/init-db-job.yaml` (`kubectl apply -k`) |
| `scripts/utils/install_by_batches.sh` | `ansible/playbooks/provision.yml` · `cms-ha install-batches` |
| `scripts/utils/recreate_failed.sh` | `cms-ha cleanup` · `cms-ha recreate` |
| `scripts/utils/repair_paused_kubernetes.sh` | `cms-ha cleanup` |
| `scripts/utils/shrink_vm_ram.sh` | `cms-ha vm shrink` |
| `scripts/utils/sync_vm_clocks.sh` | `cms-ha vm clocks` · chrony (`role::base`) |
| `scripts/utils/test_failover.sh` | `cms-ha test failover` |
| `scripts/utils/verify_all.sh` | `cms-ha verify` |

## Divergence guard

`tests/scripts_test.go` fails if a shell script is added under `scripts/` that
is not listed in this file or that is missing the deprecation banner. That keeps
this inventory complete: no new v1 script can slip in unnoticed.
