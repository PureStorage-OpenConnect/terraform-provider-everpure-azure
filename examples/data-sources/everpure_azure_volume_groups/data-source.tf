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

variable "virtual_machine_id" {
  type        = string
  description = "Full ARM ID of an existing virtual machine that can reach the storage pool's subnet."
}

data "everpure_azure_storage_pool" "example" {
  name                = "pool-prod-east"
  resource_group_name = "rg-storage-prod"
}

# Lists every volume group on the pool above.
data "everpure_azure_volume_groups" "example" {
  storage_pool_id = data.everpure_azure_storage_pool.example.id
}

# Connects the virtual machine to every volume group on the pool, including ones
# created later: the next apply picks them up.
resource "everpure_azure_virtual_machine_connection" "example" {
  virtual_machine_id = var.virtual_machine_id
  volume_group_ids   = data.everpure_azure_volume_groups.example.volume_groups[*].id
}
