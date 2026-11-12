# A worked example: a storage pool injected into a virtual network you own.
#
# The network side is built with the standard `azurerm` provider; only the pool
# itself comes from this provider. Replace the placeholder subscription ID and
# reservation ID with your own before running.

terraform {
  required_providers {
    everpure = {
      source = "PureStorage-OpenConnect/everpure-azure"
    }
    azurerm = {
      source  = "hashicorp/azurerm"
      version = ">= 4.0"
    }
  }
}

provider "everpure" {
  subscription_id = var.subscription_id
}

provider "azurerm" {
  features {}
  subscription_id = var.subscription_id
}

variable "subscription_id" {
  type        = string
  description = "Subscription the pool and its network are created in."
}

variable "reservation_id" {
  type        = string
  description = <<-EOT
    Full ARM ID of an existing Reservation in the same subscription and region,
    e.g. /subscriptions/.../providers/PureStorage.Block/reservations/prod.
  EOT
}

resource "azurerm_resource_group" "example" {
  name     = "rg-storage-prod"
  location = "eastus2"
}

resource "azurerm_virtual_network" "example" {
  name                = "vnet-prod"
  resource_group_name = azurerm_resource_group.example.name
  location            = azurerm_resource_group.example.location
  address_space       = ["10.0.0.0/16"]
}

# The pool's network interfaces are placed in a subnet delegated to the
# PureStorage.Block service. The delegation is required.
resource "azurerm_subnet" "storage" {
  name                 = "snet-storage"
  resource_group_name  = azurerm_resource_group.example.name
  virtual_network_name = azurerm_virtual_network.example.name
  address_prefixes     = ["10.0.1.0/24"]

  delegation {
    name = "everpure"
    service_delegation {
      name = "PureStorage.Block/storagePools"
    }
  }
}

resource "everpure_azure_storage_pool" "prod" {
  name                = "pool-prod-east"
  resource_group_name = azurerm_resource_group.example.name
  location            = azurerm_resource_group.example.location

  # Changing the zone replaces the pool — a full teardown and rebuild.
  zone = "1"

  # Megabytes per second. Can be changed in place.
  provisioned_bandwidth_in_mbps = 2048

  reservation_id = var.reservation_id

  vnet_injection {
    subnet_id = azurerm_subnet.storage.id
  }

  # Enables platform console access (GUI, REST API and CLI) from the injected
  # subnet. Remove the block to disable console access.
  platform_console {
    gui_enabled = true
    api_enabled = true
    cli_enabled = false
  }

  tags = {
    environment = "prod"
  }

  # Pool deletion is permanent. This makes any plan that would destroy or
  # replace the pool fail. See the "Deletion policies" guide.
  lifecycle {
    prevent_destroy = true
  }
}

output "storage_pool_id" {
  value = everpure_azure_storage_pool.prod.id
}

output "storage_pool_provisioned_iops" {
  value = everpure_azure_storage_pool.prod.provisioned_iops
}
