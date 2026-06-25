# versions.tf
# Provider and Terraform/OpenTofu version constraints for the CMS HA infrastructure.
#
# Compatible with both HashiCorp Terraform >= 1.0 and OpenTofu >= 1.6.

terraform {
  required_version = ">= 1.0"

  required_providers {
    libvirt = {
      source  = "dmacvicar/libvirt"
      version = "~> 0.8.0"
    }
  }
}
