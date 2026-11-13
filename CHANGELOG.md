# Changelog

## v1.0.1

NOTES:

* Documentation pages on the Terraform Registry no longer repeat `azure_` in their URLs.

## v1.0.0

FEATURES:

* **Resources:** `everpure_azure_reservation`, `everpure_azure_storage_pool`,
  `everpure_azure_volume_group`, `everpure_azure_volume`, `everpure_azure_snapshot`,
  `everpure_azure_virtual_machine_connection`.
* **Data sources:** `everpure_azure_reservation(s)`, `everpure_azure_storage_pool(s)`,
  `everpure_azure_storage_pool_status`, `everpure_azure_volume_group(s)`,
  `everpure_azure_volume_group_status`, `everpure_azure_volume_group_connection_parameters`,
  `everpure_azure_volume(s)`, `everpure_azure_snapshot(s)`,
  `everpure_azure_recoverable_volume_groups`.
* **List resources** (`terraform query`): `everpure_azure_reservation`,
  `everpure_azure_storage_pool`, `everpure_azure_volume_group`, `everpure_azure_volume`,
  `everpure_azure_snapshot`.
* **Ephemeral resource:** `everpure_azure_platform_console_activation_credentials`.
* **Action:** `everpure_azure_create_platform_console_ssh_user`.