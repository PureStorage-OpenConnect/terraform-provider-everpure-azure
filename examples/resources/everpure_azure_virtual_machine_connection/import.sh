# Virtual machine connections are imported by the full Azure resource ID of
# the underlying VM extension. volume_group_ids cannot be recovered from the
# extension's settings, so it is left unset after import; the first plan
# shows it being set from configuration.
terraform import everpure_azure_virtual_machine_connection.app \
  "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-app/providers/Microsoft.Compute/virtualMachines/vm-app01/extensions/PureMountIscsi"
