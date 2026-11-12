import {
  to = everpure_azure_virtual_machine_connection.app
  identity = {
    subscription_id      = "00000000-0000-0000-0000-000000000000"
    resource_group_name  = "rg-app"
    virtual_machine_name = "vm-app01"
    name                 = "MountIscsiLinux-deploy" # the extension name
  }
}

resource "everpure_azure_virtual_machine_connection" "app" {
  # ... configuration matching the imported connection
}
