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

# Lists every storage pool in one resource group.
data "everpure_azure_storage_pools" "example" {
  resource_group_name = "rg-storage-prod"
}

# Omit resource_group_name to list every storage pool in the subscription.
data "everpure_azure_storage_pools" "all" {}

# Storage pool IDs keyed by name, e.g. { "pool-prod-east" = "/subscriptions/..." }.
output "storage_pool_ids" {
  value = {
    for pool in data.everpure_azure_storage_pools.example.storage_pools :
    pool.name => pool.id
  }
}
