# Hand the credentials to a script.
#
# Run this configuration only when you need a platform console credentials. Every
# plan and apply issues a new code and invalidates the previous one, so keep it apart
# from the configuration that manages your infrastructure.
#
# Requires Terraform 1.10 or later.

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

data "everpure_azure_storage_pool" "example" {
  name                = "pool-prod-east"
  resource_group_name = "rg-storage-prod"
}

ephemeral "everpure_azure_platform_console_activation_credentials" "this" {
  storage_pool_id = data.everpure_azure_storage_pool.example.id
}

# The provisioner runs at apply only, never during plan. triggers_replace runs it on
# every apply, each time with the code that apply just issued. The code goes in
# environment, not in command: command-line arguments are visible to other users of
# the machine. Terraform hides the script's output while the code is in its
# environment. If the script fails, the apply fails and shows the script's output in
# full, so the script must never print the code.
resource "terraform_data" "console_setup" {
  triggers_replace = plantimestamp()

  provisioner "local-exec" {
    command = "./setup-console-user.sh"
    environment = {
      CONSOLE_USERNAME = ephemeral.everpure_azure_platform_console_activation_credentials.this.username
      CONSOLE_PASSWORD = ephemeral.everpure_azure_platform_console_activation_credentials.this.password
    }
  }
}
