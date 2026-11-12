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

resource "everpure_azure_volume_group" "example" {
  name            = "vg-prod-east"
  storage_pool_id = var.storage_pool_id
}

resource "everpure_azure_volume" "example" {
  name            = "vol-prod-data"
  volume_group_id = everpure_azure_volume_group.example.id

  # Can be increased in place; decreasing is rejected.
  provisioned_size_in_gib = 100

  # provisioned_size_in_bytes exists for sizes that are not a whole GiB.
}

# A volume cloned from an existing one. source_volume_id and
# source_type are create-only: changing either forces a new volume to be
# created. The clone inherits its source's size, so no size attribute is set
# here.
resource "everpure_azure_volume" "clone" {
  name             = "vol-prod-data-clone"
  volume_group_id  = everpure_azure_volume_group.example.id
  source_type      = "volume"
  source_volume_id = everpure_azure_volume.example.id

  # Overrides the defaults, which normally protect a volume from destroy.
  deletion_protection  = false # true (default) blocks destroy + replace
  eradicate_on_destroy = true  # false (default) soft-deletes, recoverable
}

# A volume recovered after it was soft-deleted. The source must be in the same
# volume group, and is addressed by the timestamped name soft deletion gave it.
# Recovering restores that volume under the name given here; it does not copy it.
resource "everpure_azure_volume" "restored_soft_deleted" {
  name                         = "vol-prod-data-restored-soft-deleted"
  volume_group_id              = everpure_azure_volume_group.example.id
  source_type                  = "recoverableVolume"
  source_recoverable_volume_id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-storage-prod/providers/PureStorage.Block/storagePools/pool-prod-east/volumeGroups/vg-prod-east/volumes/vol-prod-data-20260827141136"
}

# A volume restored from a volume group snapshot. Like the clone above, the
# restored volume inherits its source's size. volume_name is the name the
# source volume had when the snapshot was taken.
resource "everpure_azure_volume" "restored" {
  name            = "vol-prod-data-restored"
  volume_group_id = everpure_azure_volume_group.example.id
  source_type     = "snapshot"

  source_snapshot {
    id          = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-storage-prod/providers/PureStorage.Block/storagePools/pool-prod-east/volumeGroups/vg-prod-east/snapshots/snap-2026-09-01"
    volume_name = "vol-prod-data"
  }
}

output "volume_id" {
  value = everpure_azure_volume.example.id
}

output "volume_serial_number" {
  value = everpure_azure_volume.example.serial_number
}
