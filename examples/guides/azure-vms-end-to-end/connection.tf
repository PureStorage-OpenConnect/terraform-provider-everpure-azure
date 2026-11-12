resource "everpure_azure_virtual_machine_connection" "example" {
  virtual_machine_id = var.virtual_machine_id
  volume_group_ids = [
    everpure_azure_volume_group.example.id,
  ]

  # Connecting to a volume group that has no volumes fails, and
  # volume_group_ids alone does not wait for the volume to be created.
  depends_on = [
    everpure_azure_volume.example,
  ]
}
