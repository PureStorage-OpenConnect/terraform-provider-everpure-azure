import {
  to = everpure_azure_reservation.example
  identity = {
    subscription_id     = "00000000-0000-0000-0000-000000000000"
    resource_group_name = "rg-storage-prod"
    name                = "reservation-prod-east"
  }
}

resource "everpure_azure_reservation" "example" {
  # ... configuration matching the imported reservation, including its offer
}
