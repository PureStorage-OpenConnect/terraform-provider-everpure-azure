# Save as discover.tfquery.hcl next to your configuration, then run
#   terraform query -generate-config-out=generated.tf
# to write an import block and a resource block for every pool found.

# Every storage pool in the provider's subscription_id.
list "everpure_azure_storage_pool" "all" {
  provider         = everpure
  include_resource = true
}

# Only the pools in one resource group.
list "everpure_azure_storage_pool" "one_group" {
  provider         = everpure
  include_resource = true

  config {
    resource_group_name = "rg-storage-prod"
  }
}
