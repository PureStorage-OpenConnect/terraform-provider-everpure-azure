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

# Looks up a Volume Group, and the Storage Pool it lives on, both created
# outside this Terraform configuration.
data "everpure_azure_storage_pool" "example" {
  name                = "pool-prod-east"
  resource_group_name = "rg-storage-prod"
}

data "everpure_azure_volume_group" "example" {
  name            = "vg-prod-east"
  storage_pool_id = data.everpure_azure_storage_pool.example.id
}

output "volume_group_id" {
  value = data.everpure_azure_volume_group.example.id
}

output "volume_group_internal_id" {
  value = data.everpure_azure_volume_group.example.internal_id
}
