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

data "everpure_azure_volume_group" "example" {
  name            = "vg-prod-east"
  storage_pool_id = var.storage_pool_id
}

# A snapshot of every volume in the volume group. It has no updatable
# attributes: changing the name or volume group creates a new snapshot. It stays
# until it is destroyed.
resource "everpure_azure_snapshot" "example" {
  name            = "snap-before-upgrade"
  volume_group_id = data.everpure_azure_volume_group.example.id

  # Overrides the defaults, which normally protect a snapshot from destroy.
  deletion_protection  = false # true (default) blocks destroy + replace
  eradicate_on_destroy = true  # false (default) soft-deletes, recoverable
}

# A volume restored from the snapshot above. volume_name is the name the source
# volume had when the snapshot was taken, as reported in volume_snapshots.
resource "everpure_azure_volume" "restored" {
  name            = "vol-prod-data-restored"
  volume_group_id = data.everpure_azure_volume_group.example.id
  source_type     = "snapshot"

  source_snapshot {
    id          = everpure_azure_snapshot.example.id
    volume_name = everpure_azure_snapshot.example.volume_snapshots[0].volume_name
  }
}

# A whole volume group restored from the snapshot.
resource "everpure_azure_volume_group" "restored" {
  name               = "vg-prod-east-restored"
  storage_pool_id    = var.storage_pool_id
  source_type        = "snapshot"
  source_snapshot_id = everpure_azure_snapshot.example.id
}

# A snapshot recovered after it was soft-deleted. The source must be in the same
# volume group, and must not have been taken by the snapshot schedule. Recovering
# restores that snapshot under the name given here; it does not copy it.
resource "everpure_azure_snapshot" "recovered" {
  name                           = "snap-before-upgrade-recovered"
  volume_group_id                = data.everpure_azure_volume_group.example.id
  source_recoverable_snapshot_id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-storage-prod/providers/PureStorage.Block/storagePools/pool-prod-east/volumeGroups/vg-prod-east/snapshots/snap-deleted-20260827141136"
}

output "snapshot_id" {
  value = everpure_azure_snapshot.example.id
}

output "snapshot_created_at" {
  value = everpure_azure_snapshot.example.created_at
}
