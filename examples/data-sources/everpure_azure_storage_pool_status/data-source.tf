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

# Looks up a Storage Pool.
data "everpure_azure_storage_pool" "example" {
  name                = "pool-prod-east"
  resource_group_name = "rg-storage-prod"
}

# A check block reports a failed assert as a warning during plan and apply; it never
# stops the run.
check "storage_pool_status" {
  data "everpure_azure_storage_pool_status" "example" {
    storage_pool_id = data.everpure_azure_storage_pool.example.id
  }

  assert {
    condition     = data.everpure_azure_storage_pool_status.example.used_capacity_percentage < 75
    error_message = "Storage pool is above 75% of its maximum capacity."
  }
}

# To block a run instead of warning, read the status outside a check block and use it in
# a precondition.
data "everpure_azure_storage_pool_status" "gate" {
  storage_pool_id = data.everpure_azure_storage_pool.example.id
}

resource "terraform_data" "require_pool_headroom" {
  lifecycle {
    precondition {
      condition     = data.everpure_azure_storage_pool_status.gate.used_capacity_percentage < 90
      error_message = "Storage pool is above 90% of its maximum capacity."
    }
  }
}
