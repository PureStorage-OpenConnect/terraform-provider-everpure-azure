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

  # Both omitted below means unlimited. Removing either attribute from
  # configuration clears an existing limit; both can be changed in place.
  bandwidth_limit_in_mbps = 500
  iops_limit              = 5000

  # Absent means no snapshot schedule. Can be added, changed or removed in
  # place; removing it disables the schedule.
  snapshot_protection_policy {
    frequency = "PT1H"
    retention = "P7D"
  }

  tags = {
    environment = "prod"
  }
}

# A volume group cloned from an existing one. Every source_* attribute is
# create-only: changing source_type or any source_*_id forces a new volume
# group to be created.
resource "everpure_azure_volume_group" "clone" {
  name                   = "vg-prod-east-clone"
  storage_pool_id        = var.storage_pool_id
  source_type            = "volumeGroup"
  source_volume_group_id = everpure_azure_volume_group.example.id

  # Overrides the defaults, which normally protect a volume group from destroy.
  deletion_protection  = false # true (default) blocks destroy + replace
  eradicate_on_destroy = true  # false (default) soft-deletes, recoverable
}

output "volume_group_id" {
  value = everpure_azure_volume_group.example.id
}

output "volume_group_internal_id" {
  value = everpure_azure_volume_group.example.internal_id
}
