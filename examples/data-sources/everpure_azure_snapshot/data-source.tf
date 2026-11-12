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

# Looks up a snapshot created outside this Terraform configuration. A snapshot
# taken by the volume group's snapshot schedule is named with digits only.
data "everpure_azure_snapshot" "example" {
  name            = "snap-before-upgrade"
  volume_group_id = data.everpure_azure_volume_group.example.id
}

# A volume restored from that snapshot. volume_name is the name the source volume
# had when the snapshot was taken.
resource "everpure_azure_volume" "restored" {
  name            = "vol-prod-data-restored"
  volume_group_id = data.everpure_azure_volume_group.example.id
  source_type     = "snapshot"

  source_snapshot {
    id          = data.everpure_azure_snapshot.example.id
    volume_name = data.everpure_azure_snapshot.example.volume_snapshots[0].volume_name
  }
}

output "snapshot_id" {
  value = data.everpure_azure_snapshot.example.id
}

output "snapshot_created_at" {
  value = data.everpure_azure_snapshot.example.created_at
}

output "snapshot_created_by_policy" {
  value = data.everpure_azure_snapshot.example.created_by_policy
}
