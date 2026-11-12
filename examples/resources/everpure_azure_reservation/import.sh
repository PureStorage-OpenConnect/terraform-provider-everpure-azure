# Reservations are imported by their full Azure resource ID. The `user`
# attribute is write-only and is never read back, so it does not take part.
terraform import everpure_azure_reservation.example \
  "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-storage-prod/providers/PureStorage.Block/reservations/reservation-prod-east"
