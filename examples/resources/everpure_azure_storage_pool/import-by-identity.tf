import {
  to = everpure_azure_storage_pool.example
  identity = {
    subscription_id     = "00000000-0000-0000-0000-000000000000"
    resource_group_name = "rg-storage-prod"
    name                = "pool-prod-east"
  }
}

resource "everpure_azure_storage_pool" "example" {
  # ... configuration matching the imported storage pool
}
