# variables.tf
# Configurable parameters for the CMS HA virtual infrastructure.
#
# Override defaults via terraform.tfvars or -var flags.
# Example: terraform apply -var="num_hotdesks=5" -var="vm_storage_path=/data/vms"

# ── Connection ────────────────────────────────────────────────────────────────

variable "libvirt_uri" {
  description = "Libvirt connection URI (qemu:///system for root, qemu:///session for unprivileged)"
  type        = string
  default     = "qemu:///system"
}

# ── Storage ───────────────────────────────────────────────────────────────────

variable "vm_storage_path" {
  description = "Absolute path for the VM disk image storage pool"
  type        = string
  default     = "/home/user/vm_storage"
}

variable "ubuntu_iso_path" {
  description = "Path to the Ubuntu 24.04 Server ISO for Jumpstart installation (leave empty to skip)"
  type        = string
  default     = ""
}

# ── Hot-desk scaling ──────────────────────────────────────────────────────────

variable "num_hotdesks" {
  description = "Number of hot-desk workstations to provision (1–8)"
  type        = number
  default     = 3

  validation {
    condition     = var.num_hotdesks >= 1 && var.num_hotdesks <= 8
    error_message = "num_hotdesks must be between 1 and 8 (IP range limitation)."
  }
}

# ── SSH ───────────────────────────────────────────────────────────────────────

variable "ssh_public_key_path" {
  description = "Path to the SSH public key injected into cloud-init for the Jumpstart node"
  type        = string
  default     = "~/.ssh/id_ed25519_gar.pub"
}

# ── RAM (MiB) ─────────────────────────────────────────────────────────────────

variable "ram_jumpstart" {
  description = "RAM in MiB for the Jumpstart (Cobbler + Puppet) node"
  type        = number
  default     = 2048
}

variable "ram_router" {
  description = "RAM in MiB for the UFW router/firewall"
  type        = number
  default     = 512
}

variable "ram_master" {
  description = "RAM in MiB for each K3s master node"
  type        = number
  default     = 1024
}

variable "ram_worker" {
  description = "RAM in MiB for each K3s worker node"
  type        = number
  default     = 768
}

variable "ram_storage" {
  description = "RAM in MiB for the storage node"
  type        = number
  default     = 1024
}

variable "ram_monitor" {
  description = "RAM in MiB for the monitoring node (Prometheus + Grafana)"
  type        = number
  default     = 512
}

variable "ram_lb" {
  description = "RAM in MiB for the Nginx load balancer"
  type        = number
  default     = 512
}

variable "ram_cms" {
  description = "RAM in MiB for each CMS frontend (WordPress + Apache)"
  type        = number
  default     = 512
}

variable "ram_hotdesk" {
  description = "RAM in MiB for each hot-desk workstation"
  type        = number
  default     = 768
}

# ── vCPUs ─────────────────────────────────────────────────────────────────────

variable "vcpu_jumpstart" {
  type    = number
  default = 2
}

variable "vcpu_router" {
  type    = number
  default = 1
}

variable "vcpu_master" {
  type    = number
  default = 1
}

variable "vcpu_worker" {
  type    = number
  default = 1
}

variable "vcpu_storage" {
  type    = number
  default = 1
}

variable "vcpu_monitor" {
  type    = number
  default = 1
}

variable "vcpu_lb" {
  type    = number
  default = 1
}

variable "vcpu_cms" {
  type    = number
  default = 1
}

variable "vcpu_hotdesk" {
  type    = number
  default = 1
}
