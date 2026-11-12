# Storage pools are imported by their full Azure resource ID.
terraform import everpure_azure_storage_pool.example \
  "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-storage-prod/providers/PureStorage.Block/storagePools/pool-prod-east"
