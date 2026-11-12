# Create a platform console user right after the Storage Pool is created.
#
# The action runs once, when the pool is created. In-place updates of the pool do not
# run it again, changing the key or role here later does nothing, and replacing the
# pool creates the user on the new pool. To change a user afterwards, use the platform
# console's GUI or REST API.
#
# Requires Terraform 1.16 or later: `caller` refers to the resource that triggered the
# action, which avoids a dependency cycle between the pool and the action.

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

variable "reservation_id" {
  type = string
}

variable "subnet_id" {
  type = string
}

variable "console_public_key_path" {
  type    = string
  default = "~/.ssh/id_ed25519.pub"
}

action "everpure_azure_create_platform_console_ssh_user" "admin" {
  config {
    storage_pool_id = caller.id
    username        = "tfadmin"
    role            = "array_admin"
    public_key      = file(var.console_public_key_path)
  }
}

resource "everpure_azure_storage_pool" "this" {
  name                          = "pool-prod-east"
  resource_group_name           = "rg-storage-prod"
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = var.reservation_id
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = var.subnet_id
  }

  platform_console {
    gui_enabled = true
    api_enabled = true
    cli_enabled = true
  }

  lifecycle {
    action_trigger {
      events  = [after_create]
      actions = [action.everpure_azure_create_platform_console_ssh_user.admin]
    }
  }
}
