# Snapshots are imported by their full Azure resource ID. A snapshot taken by the
# volume group's snapshot schedule cannot be imported; read it with the
# everpure_azure_snapshot data source instead.
terraform import everpure_azure_snapshot.example \
  "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-storage-prod/providers/PureStorage.Block/storagePools/pool-prod-east/volumeGroups/vg-prod-east/snapshots/snap-before-upgrade"
