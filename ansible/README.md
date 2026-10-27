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

Ansible is **not** used for runtime operations or fencing. Start/stop/shrink,
state queries and STONITH stay on the libvirt primitive inside the Go
orchestrator, so a failure in Python or the collections can never block a fence.

```bash
make provision                       # Ansible creates networks + VMs
./cms-ha deploy --skip-vm-create     # service phases only
```

If you extend the optional Ansible path, put its creation logic in the Ansible
roles. The Go `init-vms` phase remains the default and must keep working
independently.

## Boundary: Ansible vs Puppet vs Go

| Layer | Tool | Responsibility |
|:------|:-----|:---------------|
| Infrastructure (create/declare) | **Ansible** | libvirt networks, pools, disks, domain XML |
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

Disks are guarded with `creates:` and the seed ISO is only rebuilt when the
rendered user-data changes, so a second `provision.yml` run reports **0
changes** (except when `config.yaml` genuinely changed).
