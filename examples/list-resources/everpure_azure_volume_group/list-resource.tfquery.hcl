# Save as discover.tfquery.hcl next to your configuration, then run
#   terraform query -generate-config-out=generated.tf
# to write an import block and a resource block for every volume group found.

list "everpure_azure_volume_group" "all" {
  provider         = everpure
  include_resource = true

  config {
    storage_pool_id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-storage-prod/providers/PureStorage.Block/storagePools/pool-prod-east"
  }
}
