# Ansible — optional Infrastructure-as-Code path

`ansible/` declares the infrastructure: libvirt networks, disks and VM domains,
generated from `config.yaml`. It is an **optional alternative** for teams that
prefer a declarative creation layer.

The supported default and the path covered by the PXE/Cobbler gate is
`cms-ha deploy` (the Go `init-vms` phase). Ansible is kept because it is a useful
IaC showcase and is checked by `ansible-lint`, `--syntax-check` and inventory
parity tests, but it is **not** the gate-validated path and must not be presented
as a second official deployment entry point. The two paths share `config.yaml`
and the same inventory data; there is no independent source of addresses, MACs
or VM sizes.

Ansible is **not** used for runtime operations or fencing. Start/stop/shrink
and state queries use the Go orchestrator's libvirt primitive. Guest-initiated
STONITH uses the scripts installed by `setup-drbd`; the derived SSH path pins
hypervisor host keys and uses a forced command restricted to `destroy`/`domstate`
of the two configured DRBD domains. It does not invoke Ansible or its collections.
It still depends on SSH, hypervisor access and successful power-off confirmation.

```bash
make provision                     # optional: define networks, disks and domains
# Boot/install jumpstart, configure Cobbler/register nodes, and install clients
# using the MANUAL procedure before attempting service deployment:
./cms-ha deploy --skip-vm-create   # skips VM creation, not Cobbler/registration
```

If you extend the optional Ansible path, put its creation logic in the Ansible
roles. The Go `init-vms` phase remains the default and must keep working
independently.

## Boundary: Ansible vs Puppet vs Go

| Layer | Tool | Responsibility |
|:------|:-----|:---------------|
| Infrastructure (create/declare) | **Go by default; Ansible optional** | libvirt networks, disks, domain XML |
| Node desired state | **Puppet** | packages, services, config files inside the guest |
| Orchestration & hot ops | **Go/Cobra** | control flow, timeouts, phases, runtime libvirt primitive, fencing |

`config.yaml` is the **single source of truth**: it generates the inventory
*and* the parameters of the runtime primitive. The Ansible code never invents
addresses, MACs, RAM or disk sizes — everything comes from the generated
inventory.

## Layout

```
ansible/
├── ansible.cfg
├── requirements.yml              # community.libvirt
├── inventory/
│   └── hosts.yml                 # GENERATED — do not edit (cms-ha inventory)
├── group_vars/all.yml            # network bridge vars + secrets contract
├── playbooks/
│   └── provision.yml             # networks + cloud-init seed + VM domains
└── roles/
    ├── libvirt_networks/         # internal + main bridges
    ├── cloud_init/               # jumpstart autoinstall seed ISO
    └── vm_create/                # disks + deterministic domain XML
```

## Usage

```bash
# 1. Install the required collection (once)
ansible-galaxy collection install -r requirements.yml

# 2. Regenerate the inventory after any config.yaml change
cms-ha inventory                 # writes ansible/inventory/hosts.yml
cms-ha inventory --stdout        # review without writing

# 3. Provision (networks + disks + domains)
cd ansible
ansible-playbook playbooks/provision.yml --check   # dry run
ansible-playbook playbooks/provision.yml

# Partial runs
ansible-playbook playbooks/provision.yml --tags networks
ansible-playbook playbooks/provision.yml --tags vms
```

### Jumpstart cloud-init secret

The jumpstart VM is bootstrapped with an Ubuntu autoinstall seed ISO, which
needs an admin password **hash**. It must be supplied out of band and never
committed:

```bash
ansible-playbook playbooks/provision.yml \
  --extra-vars "jumpstart_password_hash=$(openssl passwd -6)"
```

or via an `ansible-vault`-encrypted `group_vars/all/vault.yml`.

## Parity & Tests

`config.yaml` ↔ inventory parity is enforced by a unit test
(`tests/ansible_test.go`): every node in `config.yaml` must appear in the
inventory with a matching `ansible_host`, and the role groups must match the
configured counts. CI also runs `ansible-lint` and `--syntax-check`.

## Idempotency

Disks are guarded with `creates:` and the seed ISO build handler is notified
when user-data or meta-data changes. Domain definitions are reapplied; bridge
address assignment treats `File exists` as unchanged. This is intended to be
repeatable, not a verified guarantee that every second run reports zero changes.

The network templates enable bridge STP only; they do not configure LACP,
RSTP, bonded uplinks or physical cable failover. Inventory parity and syntax/lint
checks are not equivalent to live deployment acceptance. The release's
prepublication ISO/PXE gate uses the Go path, not this optional playbook.

Re-running Ansible alone does not migrate guest storage or fencing policy.
Existing labs need `setup-puppet` for generated frontend NFS allowlists, hard
uploads mounts, Apache dependencies and PHP limits, and `setup-drbd` for the
restricted fencing key and pinned host keys. Follow the standalone migration
notes in [MANUAL](../docs/MANUAL.md#62-wordpress-media-and-file-backups) and
[the fencing procedure](../docs/MANUAL.md#71-master-1-failure-drbdk3s-primary).
Checkpoint resume additionally requires the original executable, host and
non-secret deployment inputs; do not rebuild before `--resume` or carry progress
from the optional provisioning path into a different deployment plan blindly.
