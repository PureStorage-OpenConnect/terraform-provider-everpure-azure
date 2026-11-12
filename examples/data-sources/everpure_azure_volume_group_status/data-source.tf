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

# Looks up a Volume Group and the Storage Pool it lives on.
data "everpure_azure_storage_pool" "example" {
  name                = "pool-prod-east"
  resource_group_name = "rg-storage-prod"
}

data "everpure_azure_volume_group" "example" {
  name            = "vg-prod-east"
  storage_pool_id = data.everpure_azure_storage_pool.example.id
}

# In a check block, a failed assert is reported as a warning during plan and apply, and
# the run continues.
check "volume_group_status" {
  data "everpure_azure_volume_group_status" "example" {
    volume_group_id = data.everpure_azure_volume_group.example.id
  }

  assert {
    condition     = coalesce(data.everpure_azure_volume_group_status.example.max_connected_hosts_per_volume, 0) > 0
    error_message = "Volume group is not connected to any host."
  }

  # The service sets no limit on a volume group's space; this threshold (1 TiB) is
  # your own.
  assert {
    condition     = try(data.everpure_azure_volume_group_status.example.space.total_used_in_bytes, 0) < 1099511627776
    error_message = "Volume group uses more than 1 TiB."
  }
}

# In a precondition, a failed condition stops the run. The status is read outside a
# check block here.
data "everpure_azure_volume_group_status" "gate" {
  volume_group_id = data.everpure_azure_volume_group.example.id
}

resource "terraform_data" "require_volume_group_headroom" {
  lifecycle {
    precondition {
      condition     = try(data.everpure_azure_volume_group_status.gate.space.total_used_in_bytes, 0) < 1099511627776
      error_message = "Volume group uses more than 1 TiB."
    }
  }
}
