# Volumes are imported by their full Azure resource ID.
terraform import everpure_azure_volume.example \
  "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-storage-prod/providers/PureStorage.Block/storagePools/pool-prod-east/volumeGroups/vg-prod-east/volumes/vol-prod-data"
