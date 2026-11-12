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

data "everpure_azure_storage_pool" "example" {
  name                = "pool-prod-east"
  resource_group_name = "rg-storage-prod"
}

# Lists every soft-deleted (destroyed, not yet eradicated) volume group on the pool
# above.
data "everpure_azure_recoverable_volume_groups" "example" {
  storage_pool_id = data.everpure_azure_storage_pool.example.id
}

locals {
  # Every soft-deleted volume group that was named "vg-prod-east", oldest first: the list
  # is sorted by id, and a soft-deleted volume group's name (the last segment of its id)
  # is its previous name suffixed with the timestamp it was destroyed at.
  deleted_vg_prod_east = [
    for rvg in data.everpure_azure_recoverable_volume_groups.example.recoverable_volume_groups :
    rvg if rvg.soft_deletion.previous_name == "vg-prod-east"
  ]
  newest_deleted_vg_prod_east_id = (
    length(local.deleted_vg_prod_east) > 0
    ? local.deleted_vg_prod_east[length(local.deleted_vg_prod_east) - 1].id
    : null
  )
}

# Recovers the most recently soft-deleted "vg-prod-east".
resource "everpure_azure_volume_group" "recovered" {
  name                               = "vg-prod-east"
  storage_pool_id                    = data.everpure_azure_storage_pool.example.id
  source_type                        = "recoverableVolumeGroup"
  source_recoverable_volume_group_id = local.newest_deleted_vg_prod_east_id

  lifecycle {
    precondition {
      condition     = local.newest_deleted_vg_prod_east_id != null
      error_message = "No soft-deleted volume group previously named \"vg-prod-east\" was found on the storage pool."
    }
  }
}
