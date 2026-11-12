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

# Looks up a Volume, and the Volume Group and Storage Pool above it, all
# created outside this Terraform configuration.
data "everpure_azure_storage_pool" "example" {
  name                = "pool-prod-east"
  resource_group_name = "rg-storage-prod"
}

data "everpure_azure_volume_group" "example" {
  name            = "vg-prod-east"
  storage_pool_id = data.everpure_azure_storage_pool.example.id
}

data "everpure_azure_volume" "example" {
  name            = "vol-prod-data"
  volume_group_id = data.everpure_azure_volume_group.example.id
}

output "volume_id" {
  value = data.everpure_azure_volume.example.id
}

output "volume_serial_number" {
  value = data.everpure_azure_volume.example.serial_number
}
