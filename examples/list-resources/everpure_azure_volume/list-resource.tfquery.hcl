# Save as discover.tfquery.hcl next to your configuration, then run
#   terraform query -generate-config-out=generated.tf
# to write an import block and a resource block for every volume found.

list "everpure_azure_volume" "all" {
  provider         = everpure
  include_resource = true

  config {
    volume_group_id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-storage-prod/providers/PureStorage.Block/storagePools/pool-prod-east/volumeGroups/vg-prod-east"
  }
}
