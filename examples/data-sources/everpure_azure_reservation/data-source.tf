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

# Looks up a Reservation created outside this Terraform configuration, e.g. by
# a different root module or a different team.
data "everpure_azure_reservation" "example" {
  name                = "reservation-prod-east"
  resource_group_name = "rg-storage-prod"
}

output "reservation_id" {
  value = data.everpure_azure_reservation.example.id
}

# Null when the service reports no offer for the reservation.
output "reservation_plan_id" {
  value = try(data.everpure_azure_reservation.example.offer.plan_id, null)
}
