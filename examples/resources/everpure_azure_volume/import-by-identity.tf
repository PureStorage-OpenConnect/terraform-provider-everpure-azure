import {
  to = everpure_azure_volume.example
  identity = {
    subscription_id     = "00000000-0000-0000-0000-000000000000"
    resource_group_name = "rg-storage-prod"
    storage_pool_name   = "pool-prod-east"
    volume_group_name   = "vg-prod-east"
    name                = "vol-prod-data"
  }
}

resource "everpure_azure_volume" "example" {
  # ... configuration matching the imported volume
}
