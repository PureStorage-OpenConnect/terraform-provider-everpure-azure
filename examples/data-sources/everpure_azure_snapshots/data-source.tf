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

# Lists the snapshots in the volume group above, including those taken by its
# snapshot schedule. Soft-deleted snapshots are excluded by default.
data "everpure_azure_snapshots" "example" {
  volume_group_id = data.everpure_azure_volume_group.example.id
}

locals {
  # Scheduled snapshot IDs keyed by created_at, an RFC 3339 UTC timestamp, so
  # sorting the keys sorts by time.
  scheduled = {
    for s in data.everpure_azure_snapshots.example.snapshots :
    s.created_at => s.id if s.created_by_policy
  }
}

# Restores vol-prod-data from the most recent scheduled snapshot.
resource "everpure_azure_volume" "restored" {
  name            = "vol-prod-data-restored"
  volume_group_id = data.everpure_azure_volume_group.example.id
  source_type     = "snapshot"

  source_snapshot {
    id          = local.scheduled[reverse(sort(keys(local.scheduled)))[0]]
    volume_name = "vol-prod-data"
  }
}
