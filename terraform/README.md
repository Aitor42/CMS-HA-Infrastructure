# Terraform / OpenTofu — CMS HA Infrastructure (libvirt)

This directory provides a **declarative alternative** to the imperative shell script `scripts/00_init_vms.sh` for creating the virtual infrastructure.

It uses the [dmacvicar/libvirt](https://registry.terraform.io/providers/dmacvicar/libvirt/latest) Terraform provider to manage KVM/QEMU virtual networks, disk volumes, and VM domains through HCL configuration files.

## Prerequisites

- **Terraform** ≥ 1.0 or **OpenTofu** ≥ 1.6
- **libvirt** daemon running (`systemctl status libvirtd`)
- **KVM** support enabled (`/dev/kvm` writable)
- Ubuntu 24.04 Server ISO (for Jumpstart node initial installation)

## Usage

```bash
cd terraform/

# 1. Initialise providers
terraform init

# 2. Review the execution plan
terraform plan -var="vm_storage_path=$HOME/vm_storage"

# 3. Create all networks, disks, and VMs (VMs are created but NOT started)
terraform apply -var="vm_storage_path=$HOME/vm_storage"

# 4. Use the main orchestrator to boot and provision VMs
cd ..
./deploy_all.sh --skip-vm-create
```

## Customisation

Override defaults via a `terraform.tfvars` file or command-line flags:

```hcl
# terraform.tfvars
vm_storage_path = "/data/vms"
num_hotdesks    = 5
ram_master      = 2048
libvirt_uri     = "qemu:///session"
```

## Important Notes

- **VMs start in a stopped state** (`running = false`). The `deploy_all.sh` orchestrator controls the boot sequence to ensure correct PXE provisioning order.
- **IP addresses are not assigned by Terraform**. They are assigned by the Cobbler DHCP server based on MAC address reservations (configured in `scripts/add_cobbler_nodes.sh`).
- **This does not replace the provisioning scripts**. Terraform only creates the virtual hardware; Cobbler, Puppet, and the deployment scripts handle the software stack.

## Tear Down

```bash
# Destroy all VMs, disks, and networks
terraform destroy -var="vm_storage_path=$HOME/vm_storage"
```

## Files

| File | Purpose |
|------|---------|
| `versions.tf` | Terraform and provider version constraints |
| `variables.tf` | Configurable parameters (RAM, vCPU, paths, scaling) |
| `main.tf` | Full infrastructure definition (networks, volumes, VMs) |
| `outputs.tf` | Post-apply reference outputs (VM names, IPs, networks) |
