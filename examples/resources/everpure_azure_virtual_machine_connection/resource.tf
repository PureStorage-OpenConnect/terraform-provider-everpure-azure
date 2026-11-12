terraform {
  required_providers {
    everpure = {
      source = "PureStorage-OpenConnect/everpure-azure"
    }
  }
}

provider "everpure" {
  subscription_id = var.subscription_id
}

variable "subscription_id" {
  type = string
}

variable "storage_pool_id" {
  type        = string
  description = "Full ARM ID of an existing Everpure Storage Pool in the same subscription."
}

variable "linux_virtual_machine_id" {
  type        = string
  description = "Full ARM ID of an existing Linux virtual machine that can reach the storage pool's subnet."
}

variable "windows_virtual_machine_id" {
  type        = string
  description = "Full ARM ID of an existing Windows virtual machine that can reach the storage pool's subnet."
}

resource "everpure_azure_volume_group" "example" {
  name            = "vg-prod-east"
  storage_pool_id = var.storage_pool_id
}

resource "everpure_azure_volume" "example" {
  name                    = "vol-data"
  volume_group_id         = everpure_azure_volume_group.example.id
  provisioned_size_in_gib = 100
}

resource "everpure_azure_virtual_machine_connection" "linux" {
  virtual_machine_id = var.linux_virtual_machine_id
  volume_group_ids = [
    everpure_azure_volume_group.example.id,
  ]

  # Connecting to a volume group that has no volumes fails, and
  # volume_group_ids alone does not wait for the volume to be created.
  depends_on = [
    everpure_azure_volume.example,
  ]
}

resource "everpure_azure_virtual_machine_connection" "windows" {
  virtual_machine_id = var.windows_virtual_machine_id
  volume_group_ids = [
    everpure_azure_volume_group.example.id,
  ]

  depends_on = [
    everpure_azure_volume.example,
  ]

  # First-time setup on Windows needs a reboot to finish. Without this, the
  # apply ends with a warning and the VM has to be rebooted by hand.
  reboot_allowed = true
}
