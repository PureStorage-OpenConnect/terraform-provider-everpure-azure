import {
  to = everpure_azure_snapshot.example
  identity = {
    subscription_id     = "00000000-0000-0000-0000-000000000000"
    resource_group_name = "rg-storage-prod"
    storage_pool_name   = "pool-prod-east"
    volume_group_name   = "vg-prod-east"
    name                = "snap-before-upgrade"
  }
}

resource "everpure_azure_snapshot" "example" {
  # ... configuration matching the imported snapshot
}
