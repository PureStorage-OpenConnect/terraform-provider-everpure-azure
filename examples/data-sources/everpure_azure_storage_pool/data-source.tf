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

# Looks up a Storage Pool created outside this Terraform configuration, e.g. by
# a different root module or a different team.
data "everpure_azure_storage_pool" "example" {
  name                = "pool-prod-east"
  resource_group_name = "rg-storage-prod"
}

output "storage_pool_id" {
  value = data.everpure_azure_storage_pool.example.id
}

output "storage_pool_provisioned_iops" {
  value = data.everpure_azure_storage_pool.example.provisioned_iops
}
