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

# Lists every reservation in one resource group.
data "everpure_azure_reservations" "example" {
  resource_group_name = "rg-storage-prod"
}

# Omit resource_group_name to list every reservation in the subscription.
data "everpure_azure_reservations" "all" {}

# Reservation IDs keyed by name, e.g. { "reservation-prod-east" = "/subscriptions/..." }.
output "reservation_ids" {
  value = {
    for reservation in data.everpure_azure_reservations.example.reservations :
    reservation.name => reservation.id
  }
}
