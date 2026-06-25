# main.tf
# Declarative infrastructure definition for the CMS HA environment.
#
# This is an alternative to the imperative scripts/00_init_vms.sh approach.
# It creates all virtual networks, disk volumes, and VM domains using the
# dmacvicar/libvirt Terraform provider.
#
# VMs are created in a stopped state (running = false) so that the existing
# deploy_all.sh orchestrator controls the boot sequence and PXE provisioning.
#
# Usage:
#   terraform init
#   terraform plan -var="vm_storage_path=$HOME/vm_storage"
#   terraform apply
#   # Then run: ./deploy_all.sh --skip-vm-create

# ──────────────────────────────────────────────────────────────────────────────
# Provider
# ──────────────────────────────────────────────────────────────────────────────

provider "libvirt" {
  uri = var.libvirt_uri
}

# ──────────────────────────────────────────────────────────────────────────────
# Storage Pool
# ──────────────────────────────────────────────────────────────────────────────

resource "libvirt_pool" "vm_storage" {
  name = "cms-ha-pool"
  type = "dir"
  path = var.vm_storage_path
}

# ──────────────────────────────────────────────────────────────────────────────
# Virtual Networks
# ──────────────────────────────────────────────────────────────────────────────

# Internal network — cluster backbone (K3s, DRBD, monitoring, provisioning)
resource "libvirt_network" "internal" {
  name      = "internal-net"
  mode      = "none"
  bridge    = "virbr-int"
  autostart = true

  addresses = ["192.168.10.0/24"]

  # STP enabled for L2 redundancy
  xml {
    xslt = <<-XSLT
      <?xml version="1.0"?>
      <xsl:stylesheet version="1.0" xmlns:xsl="http://www.w3.org/1999/XSL/Transform">
        <xsl:template match="@*|node()">
          <xsl:copy><xsl:apply-templates select="@*|node()"/></xsl:copy>
        </xsl:template>
        <xsl:template match="bridge">
          <bridge name="virbr-int" stp="on" delay="0"/>
        </xsl:template>
      </xsl:stylesheet>
    XSLT
  }
}

# Main network — user-facing services (LB, CMS frontends, hot-desks)
resource "libvirt_network" "main" {
  name      = "main-net"
  mode      = "none"
  bridge    = "virbr-main"
  autostart = true

  addresses = ["192.168.20.0/24"]

  xml {
    xslt = <<-XSLT
      <?xml version="1.0"?>
      <xsl:stylesheet version="1.0" xmlns:xsl="http://www.w3.org/1999/XSL/Transform">
        <xsl:template match="@*|node()">
          <xsl:copy><xsl:apply-templates select="@*|node()"/></xsl:copy>
        </xsl:template>
        <xsl:template match="bridge">
          <bridge name="virbr-main" stp="on" delay="0"/>
        </xsl:template>
      </xsl:stylesheet>
    XSLT
  }
}

# ──────────────────────────────────────────────────────────────────────────────
# Disk Volumes
# ──────────────────────────────────────────────────────────────────────────────

# Helper local for all fixed VMs and their disk sizes (in bytes)
locals {
  gb = 1073741824

  fixed_vms = {
    "jumpstart"        = { disk_gb = 30 }
    "ufw-router"       = { disk_gb = 5 }
    "internal-master1" = { disk_gb = 8 }
    "internal-master2" = { disk_gb = 8 }
    "internal-worker1" = { disk_gb = 8 }
    "internal-worker2" = { disk_gb = 8 }
    "internal-storage" = { disk_gb = 8 }
    "internal-monitor" = { disk_gb = 4 }
    "main-lb"          = { disk_gb = 4 }
    "main-cms1"        = { disk_gb = 4 }
    "main-cms2"        = { disk_gb = 4 }
  }
}

resource "libvirt_volume" "os_disk" {
  for_each = local.fixed_vms

  name = "${each.key}.qcow2"
  pool = libvirt_pool.vm_storage.name
  size = each.value.disk_gb * local.gb

  format = "qcow2"
}

# Extra DRBD data disks for master nodes (3 GB each)
resource "libvirt_volume" "drbd_disk" {
  for_each = toset(["internal-master1", "internal-master2"])

  name = "${each.key}-drbd.qcow2"
  pool = libvirt_pool.vm_storage.name
  size = 3 * local.gb

  format = "qcow2"
}

# Hot-desk disks (dynamic count)
resource "libvirt_volume" "hotdesk_disk" {
  count = var.num_hotdesks

  name = "main-hotdesk${count.index + 1}.qcow2"
  pool = libvirt_pool.vm_storage.name
  size = 3 * local.gb

  format = "qcow2"
}

# ──────────────────────────────────────────────────────────────────────────────
# VM Domains — Jumpstart (dual-homed: internal + main)
# ──────────────────────────────────────────────────────────────────────────────

resource "libvirt_domain" "jumpstart" {
  name    = "jumpstart"
  memory  = var.ram_jumpstart
  vcpu    = var.vcpu_jumpstart
  running = false

  disk {
    volume_id = libvirt_volume.os_disk["jumpstart"].id
  }

  # Boot from CD-ROM first (for initial ISO install), then HD
  boot_device {
    dev = ["cdrom", "hd"]
  }

  network_interface {
    network_id = libvirt_network.internal.id
    mac        = "52:54:00:10:00:01"
  }

  network_interface {
    network_id = libvirt_network.main.id
    mac        = "52:54:00:10:02:0a"
  }

  console {
    type        = "pty"
    target_port = "0"
    target_type = "serial"
  }
}

# ──────────────────────────────────────────────────────────────────────────────
# VM Domains — UFW Router (triple-homed: internal + main + WAN)
# ──────────────────────────────────────────────────────────────────────────────

resource "libvirt_domain" "router" {
  name    = "ufw-router"
  memory  = var.ram_router
  vcpu    = var.vcpu_router
  running = false

  disk {
    volume_id = libvirt_volume.os_disk["ufw-router"].id
  }

  boot_device {
    dev = ["network", "hd"]
  }

  # WAN interface (default NAT network for internet access)
  network_interface {
    network_name = "default"
  }

  # Internal network interface
  network_interface {
    network_id = libvirt_network.internal.id
  }

  # Main network interface
  network_interface {
    network_id = libvirt_network.main.id
  }

  console {
    type        = "pty"
    target_port = "0"
    target_type = "serial"
  }
}

# ──────────────────────────────────────────────────────────────────────────────
# VM Domains — K3s Masters (internal network + DRBD disk)
# ──────────────────────────────────────────────────────────────────────────────

locals {
  masters = {
    "internal-master1" = { mac = "52:54:00:10:01:11" }
    "internal-master2" = { mac = "52:54:00:10:01:12" }
  }
}

resource "libvirt_domain" "master" {
  for_each = local.masters

  name    = each.key
  memory  = var.ram_master
  vcpu    = var.vcpu_master
  running = false

  # OS disk
  disk {
    volume_id = libvirt_volume.os_disk[each.key].id
  }

  # DRBD data disk (/dev/vdb)
  disk {
    volume_id = libvirt_volume.drbd_disk[each.key].id
  }

  boot_device {
    dev = ["network", "hd"]
  }

  network_interface {
    network_id = libvirt_network.internal.id
    mac        = each.value.mac
  }

  console {
    type        = "pty"
    target_port = "0"
    target_type = "serial"
  }
}

# ──────────────────────────────────────────────────────────────────────────────
# VM Domains — K3s Workers (internal network)
# ──────────────────────────────────────────────────────────────────────────────

locals {
  workers = {
    "internal-worker1" = { mac = "52:54:00:10:01:13" }
    "internal-worker2" = { mac = "52:54:00:10:01:14" }
  }
}

resource "libvirt_domain" "worker" {
  for_each = local.workers

  name    = each.key
  memory  = var.ram_worker
  vcpu    = var.vcpu_worker
  running = false

  disk {
    volume_id = libvirt_volume.os_disk[each.key].id
  }

  boot_device {
    dev = ["network", "hd"]
  }

  network_interface {
    network_id = libvirt_network.internal.id
    mac        = each.value.mac
  }

  console {
    type        = "pty"
    target_port = "0"
    target_type = "serial"
  }
}

# ──────────────────────────────────────────────────────────────────────────────
# VM Domains — Storage Node (internal network)
# ──────────────────────────────────────────────────────────────────────────────

resource "libvirt_domain" "storage" {
  name    = "internal-storage"
  memory  = var.ram_storage
  vcpu    = var.vcpu_storage
  running = false

  disk {
    volume_id = libvirt_volume.os_disk["internal-storage"].id
  }

  boot_device {
    dev = ["network", "hd"]
  }

  network_interface {
    network_id = libvirt_network.internal.id
    mac        = "52:54:00:10:01:15"
  }

  console {
    type        = "pty"
    target_port = "0"
    target_type = "serial"
  }
}

# ──────────────────────────────────────────────────────────────────────────────
# VM Domains — Monitor Node (internal network)
# ──────────────────────────────────────────────────────────────────────────────

resource "libvirt_domain" "monitor" {
  name    = "internal-monitor"
  memory  = var.ram_monitor
  vcpu    = var.vcpu_monitor
  running = false

  disk {
    volume_id = libvirt_volume.os_disk["internal-monitor"].id
  }

  boot_device {
    dev = ["network", "hd"]
  }

  network_interface {
    network_id = libvirt_network.internal.id
    mac        = "52:54:00:10:01:10"
  }

  console {
    type        = "pty"
    target_port = "0"
    target_type = "serial"
  }
}

# ──────────────────────────────────────────────────────────────────────────────
# VM Domains — Main Network (LB + CMS frontends)
# ──────────────────────────────────────────────────────────────────────────────

locals {
  main_vms = {
    "main-lb"   = { ram = var.ram_lb, vcpu = var.vcpu_lb, mac = "52:54:00:10:02:64" }
    "main-cms1" = { ram = var.ram_cms, vcpu = var.vcpu_cms, mac = "52:54:00:10:02:65" }
    "main-cms2" = { ram = var.ram_cms, vcpu = var.vcpu_cms, mac = "52:54:00:10:02:66" }
  }
}

resource "libvirt_domain" "main_vm" {
  for_each = local.main_vms

  name    = each.key
  memory  = each.value.ram
  vcpu    = each.value.vcpu
  running = false

  disk {
    volume_id = libvirt_volume.os_disk[each.key].id
  }

  boot_device {
    dev = ["network", "hd"]
  }

  network_interface {
    network_id = libvirt_network.main.id
    mac        = each.value.mac
  }

  console {
    type        = "pty"
    target_port = "0"
    target_type = "serial"
  }
}

# ──────────────────────────────────────────────────────────────────────────────
# VM Domains — Hot-desk Workstations (dynamic count)
# ──────────────────────────────────────────────────────────────────────────────

resource "libvirt_domain" "hotdesk" {
  count = var.num_hotdesks

  name    = "main-hotdesk${count.index + 1}"
  memory  = var.ram_hotdesk
  vcpu    = var.vcpu_hotdesk
  running = false

  disk {
    volume_id = libvirt_volume.hotdesk_disk[count.index].id
  }

  boot_device {
    dev = ["network", "hd"]
  }

  network_interface {
    network_id = libvirt_network.main.id
    # MAC addresses: 52:54:00:10:02:c9, :ca, :cb, ... (matching 00_init_vms.sh)
    mac = format("52:54:00:10:02:%02x", 201 + count.index)
  }

  console {
    type        = "pty"
    target_port = "0"
    target_type = "serial"
  }
}
