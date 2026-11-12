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

variable "contact_email" {
  type        = string
  description = "Email address of the person responsible for the purchase."
}

resource "azurerm_resource_group" "example" {
  name     = "rg-storage-prod"
  location = "eastus2"
}

resource "everpure_azure_reservation" "example" {
  name                = "reservation-prod-east"
  resource_group_name = azurerm_resource_group.example.name
  location            = azurerm_resource_group.example.location

  # The Marketplace offer to purchase. Only plan_id can be changed in place.
  offer = {
    offer_id  = "krypton_3_plan"
    plan_id   = "psc_capacity_perf_ondemand"
    term_unit = "P1M"
  }

  # Write-only: sent when the reservation is created, never stored in the plan
  # or the state. Needs Terraform 1.11 or newer. Offers whose SaaS resource is
  # hidden reject a create without it.
  user = {
    first_name    = "Ada"
    last_name     = "Lovelace"
    email_address = var.contact_email

    company = {
      company_name = "Example Corp"
      address = {
        address_line1 = "1 Main Street"
        city          = "Seattle"
        state         = "WA"
        country       = "US"
        postal_code   = "98101"
      }
    }
  }

  tags = {
    environment = "prod"
  }

  # Destroying a reservation does not always cancel its Marketplace purchase,
  # and an Azure subscription can hold only one reservation per region. This
  # makes any plan that would destroy or replace the reservation fail.
  lifecycle {
    prevent_destroy = true
  }
}

# Storage pools reference the reservation.
output "reservation_id" {
  value = everpure_azure_reservation.example.id
}
