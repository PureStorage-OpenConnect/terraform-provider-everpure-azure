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

data "everpure_azure_volume_group" "example" {
  name            = "vg-prod-east"
  storage_pool_id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-storage-prod/providers/PureStorage.Block/storagePools/pool-prod-east"
}

# Lists every volume in the volume group above, including soft-deleted
# (destroyed, not yet eradicated) ones.
data "everpure_azure_volumes" "example" {
  volume_group_id   = data.everpure_azure_volume_group.example.id
  include_destroyed = true
}

# Volume IDs keyed by name.
output "volume_ids_by_name" {
  value = {
    for v in data.everpure_azure_volumes.example.volumes :
    v.name => v.id
  }
}

locals {
  # Every soft-deleted volume that was named "vol-prod-data", oldest first.
  deleted_vol_prod_data = [
    for v in data.everpure_azure_volumes.example.volumes :
    v if v.soft_deletion != null && v.soft_deletion.previous_name == "vol-prod-data"
  ]
  newest_deleted_vol_prod_data_id = (
    length(local.deleted_vol_prod_data) > 0
    ? local.deleted_vol_prod_data[length(local.deleted_vol_prod_data) - 1].id
    : null
  )
}

# Recovers the most recently soft-deleted "vol-prod-data", if there is one.
resource "everpure_azure_volume" "recovered" {
  count                        = local.newest_deleted_vol_prod_data_id == null ? 0 : 1
  name                         = "vol-prod-data-recovered"
  volume_group_id              = data.everpure_azure_volume_group.example.id
  source_type                  = "recoverableVolume"
  source_recoverable_volume_id = local.newest_deleted_vol_prod_data_id
}
