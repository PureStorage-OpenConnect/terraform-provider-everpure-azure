# Store the credentials in an Azure Key Vault secret.
#
# Run this configuration only when you need platform console credentials. Every
# plan and apply issues new credentials and invalidates the previous password, so keep it apart
# from the configuration that manages your infrastructure.
#
# Requires Terraform 1.11 or later, for write-only attributes.

terraform {
  required_providers {
    everpure = {
      source = "PureStorage-OpenConnect/everpure-azure"
    }
    azurerm = {
      source  = "hashicorp/azurerm"
      version = ">= 4.23.0"
    }
  }
}

provider "everpure" {
  subscription_id = var.subscription_id
}

provider "azurerm" {
  subscription_id = var.subscription_id
  features {}
}

variable "subscription_id" {
  type = string
}

data "everpure_azure_storage_pool" "example" {
  name                = "pool-prod-east"
  resource_group_name = "rg-storage-prod"
}

data "azurerm_key_vault" "example" {
  name                = "kv-storage-prod"
  resource_group_name = "rg-storage-prod"
}

ephemeral "everpure_azure_platform_console_activation_credentials" "this" {
  storage_pool_id = data.everpure_azure_storage_pool.example.id
}

# value_wo_version changes on every run, so the secret always holds the password this run
# issued and not an earlier one that has already been invalidated.
resource "azurerm_key_vault_secret" "console_activation_credentials" {
  name             = "storage-pool-console-activation-credentials"
  key_vault_id     = data.azurerm_key_vault.example.id
  value_wo         = ephemeral.everpure_azure_platform_console_activation_credentials.this.password
  value_wo_version = parseint(formatdate("YYYYMMDDhhmmss", plantimestamp()), 10)
}
