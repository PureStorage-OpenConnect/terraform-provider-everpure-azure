resource "everpure_azure_storage_pool" "example" {
  name                          = "pool-example"
  resource_group_name           = var.resource_group_name
  location                      = var.location
  zone                          = "1"
  reservation_id                = var.reservation_id
  provisioned_bandwidth_in_mbps = 2048

  vnet_injection {
    subnet_id = var.subnet_id
  }
}
