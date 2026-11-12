# Create a platform console user on demand.
#
# Nothing here runs automatically. Create the user with:
#
#   terraform apply -invoke=action.everpure_azure_create_platform_console_ssh_user.admin
#
# The action only creates new users: it fails if the user already exists. Requires
# Terraform 1.14 or later.

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

variable "console_public_key_path" {
  type    = string
  default = "~/.ssh/id_ed25519.pub"
}

data "everpure_azure_storage_pool" "example" {
  name                = "pool-prod-east"
  resource_group_name = "rg-storage-prod"
}

action "everpure_azure_create_platform_console_ssh_user" "admin" {
  config {
    storage_pool_id = data.everpure_azure_storage_pool.example.id
    username        = "tfadmin"
    role            = "array_admin"
    public_key      = file(var.console_public_key_path)
  }
}

# To give the user several keys, put one per line, for example with
# join("\n", [file("a.pub"), file("b.pub")]), or point public_key at a whole
# authorized_keys file.
