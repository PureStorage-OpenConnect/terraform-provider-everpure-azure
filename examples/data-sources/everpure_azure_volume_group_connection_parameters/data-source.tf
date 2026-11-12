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

# Looks up a Volume Group, created outside this Terraform configuration, and its
# iSCSI connection parameters.
data "everpure_azure_storage_pool" "example" {
  name                = "pool-prod-east"
  resource_group_name = "rg-storage-prod"
}

data "everpure_azure_volume_group" "example" {
  name            = "vg-prod-east"
  storage_pool_id = data.everpure_azure_storage_pool.example.id
}

data "everpure_azure_volume_group_connection_parameters" "example" {
  volume_group_id = data.everpure_azure_volume_group.example.id
}

# Sorted by portal IP and port; every endpoint of one Volume Group shares its IQN:
#
# iscsi_endpoints = tolist([
#   {
#     "ip" = "10.0.1.4"
#     "iqn" = "iqn.2010-06.com.purestorage:flasharray.1a2b3c4d5e6f7a8b"
#     "port" = 3260
#   },
#   {
#     "ip" = "10.0.1.5"
#     "iqn" = "iqn.2010-06.com.purestorage:flasharray.1a2b3c4d5e6f7a8b"
#     "port" = 3260
#   },
# ])
output "iscsi_endpoints" {
  value = data.everpure_azure_volume_group_connection_parameters.example.iscsi_endpoints
}
