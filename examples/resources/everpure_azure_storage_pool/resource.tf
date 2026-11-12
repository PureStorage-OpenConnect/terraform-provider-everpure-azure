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

# Credentials: `az login` by default, or the ARM_* environment variables /
# provider attributes described in the Authentication guide. The subscription
# can also be set via ARM_SUBSCRIPTION_ID.
provider "everpure" {
  subscription_id = var.subscription_id
}

provider "azurerm" {
  features {}
  subscription_id = var.subscription_id
}

variable "subscription_id" {
  type = string
}

variable "reservation_id" {
  type        = string
  description = "Full ARM ID of an existing Everpure Reservation in the same subscription and region."
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

# The pool is injected into a subnet delegated to the Everpure service.
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

resource "everpure_azure_storage_pool" "example" {
  name                = "pool-prod-east"
  resource_group_name = azurerm_resource_group.example.name
  location            = azurerm_resource_group.example.location
  zone                = "1"
  reservation_id      = var.reservation_id

  # Megabytes per second. Can be changed in place.
  provisioned_bandwidth_in_mbps = 2048

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

  # Optional: override the default per-operation timeouts. Creation defaults
  # to 95m already; this raises it further for a slower region or subscription.
  timeouts {
    create = "120m"
  }
}

output "storage_pool_id" {
  value = everpure_azure_storage_pool.example.id
}

output "storage_pool_provisioned_iops" {
  value = everpure_azure_storage_pool.example.provisioned_iops
}

output "storage_pool_console_management_ip" {
  value = everpure_azure_storage_pool.example.platform_console[0].subnet[0].management_ip_address
}
