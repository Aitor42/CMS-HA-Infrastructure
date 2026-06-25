# outputs.tf
# Terraform outputs for the CMS HA infrastructure.
# Provides quick-reference information after terraform apply.

output "network_names" {
  description = "Names of the virtual networks created"
  value = {
    internal = libvirt_network.internal.name
    main     = libvirt_network.main.name
  }
}

output "vm_names" {
  description = "List of all virtual machine names"
  value = concat(
    [libvirt_domain.jumpstart.name],
    [libvirt_domain.router.name],
    [for m in libvirt_domain.master : m.name],
    [for w in libvirt_domain.worker : w.name],
    [libvirt_domain.storage.name],
    [libvirt_domain.monitor.name],
    [for v in libvirt_domain.main_vm : v.name],
    [for h in libvirt_domain.hotdesk : h.name],
  )
}

output "node_ip_map" {
  description = "Reference map of hostname to expected IP address (assigned by Cobbler DHCP)"
  value = {
    "jumpstart"        = "192.168.10.10 / 192.168.20.10"
    "ufw-router"       = "192.168.10.1 / 192.168.20.1"
    "internal-master1" = "192.168.10.11"
    "internal-master2" = "192.168.10.12"
    "internal-worker1" = "192.168.10.13"
    "internal-worker2" = "192.168.10.14"
    "internal-storage" = "192.168.10.15"
    "internal-monitor" = "192.168.10.20"
    "main-lb"          = "192.168.20.100"
    "main-cms1"        = "192.168.20.101"
    "main-cms2"        = "192.168.20.102"
  }
}

output "hotdesk_count" {
  description = "Number of hot-desk workstations provisioned"
  value       = var.num_hotdesks
}
