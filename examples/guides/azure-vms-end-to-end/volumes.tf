resource "everpure_azure_volume_group" "example" {
  name            = "vg-example"
  storage_pool_id = everpure_azure_storage_pool.example.id
}

resource "everpure_azure_volume" "example" {
  name                    = "vol-example"
  volume_group_id         = everpure_azure_volume_group.example.id
  provisioned_size_in_gib = 100
}
