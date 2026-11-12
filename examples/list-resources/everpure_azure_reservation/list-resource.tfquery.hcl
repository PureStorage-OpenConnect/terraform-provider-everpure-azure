# Save as discover.tfquery.hcl next to your configuration, then run
#   terraform query -generate-config-out=generated.tf
# to write an import block and a resource block for every reservation found.

# Every reservation in the provider's subscription_id.
list "everpure_azure_reservation" "all" {
  provider         = everpure
  include_resource = true
}

# Only the reservations in one resource group.
list "everpure_azure_reservation" "one_group" {
  provider         = everpure
  include_resource = true

  config {
    resource_group_name = "rg-storage-prod"
  }
}
