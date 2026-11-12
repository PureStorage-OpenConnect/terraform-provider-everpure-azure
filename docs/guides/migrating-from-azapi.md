---
page_title: "Migrating from azapi and azurerm"
subcategory: "Guides"
description: |-
  Take over reservations, storage pools, volume groups, volumes, snapshots and
  virtual machine connections that are managed by azapi_resource or
  azurerm_virtual_machine_extension,
  with a moved block and without recreating anything.
---

# Migrating from azapi and azurerm

If you manage Everpure block storage with `azapi_resource` today, or the
Everpure iSCSI mount extension of a virtual machine with `azapi_resource` or
`azurerm_virtual_machine_extension`, you can hand the same Azure objects to this
provider's resources with a `moved` block. Nothing is deleted or recreated:
Terraform rewrites the state entry, and the next plan refreshes the object from
Azure.

## Requirements

* Terraform 1.8 or later. Earlier versions cannot move a resource to a resource
  of another type or provider.
* For every resource except a virtual machine connection, the provider's
  `subscription_id` must be the subscription the object is in. Use a [provider alias](https://developer.hashicorp.com/terraform/language/block/provider#alias-multiple-provider-configurations)
  for an object in another subscription. A virtual machine connection may be on
  a virtual machine in any subscription.
* The source must be one of these, with any api-version for an `azapi_resource`:

| Source | Moves to |
|---|---|
| `azapi_resource` of type `PureStorage.Block/reservations` | `everpure_azure_reservation` |
| `azapi_resource` of type `PureStorage.Block/storagePools` | `everpure_azure_storage_pool` |
| `azapi_resource` of type `PureStorage.Block/storagePools/volumeGroups` | `everpure_azure_volume_group` |
| `azapi_resource` of type `PureStorage.Block/storagePools/volumeGroups/volumes` | `everpure_azure_volume` |
| `azapi_resource` of type `PureStorage.Block/storagePools/volumeGroups/snapshots` | `everpure_azure_snapshot` |
| `azapi_resource` of type `Microsoft.Compute/virtualMachines/extensions` | `everpure_azure_virtual_machine_connection` |
| `azurerm_virtual_machine_extension` | `everpure_azure_virtual_machine_connection` |

Only `azapi_resource` of the `azure/azapi` provider and
`azurerm_virtual_machine_extension` of the `hashicorp/azurerm` provider are
supported sources. `azapi_update_resource`, other `azurerm` resources and
anything else are refused with an error that says so.

## Moving a storage pool

Start from a configuration that manages a pool with azapi:

```terraform
resource "azapi_resource" "pool" {
  type      = "PureStorage.Block/storagePools@2026-05-01-preview"
  name      = "pool-prod-east"
  parent_id = azurerm_resource_group.storage.id
  # ...
}
```

1. Replace the `azapi_resource` block with the matching Everpure resource,
   written to describe the existing pool.
2. Add a `moved` block from the old address to the new one.

```terraform
resource "everpure_azure_storage_pool" "pool" {
  name                = "pool-prod-east"
  resource_group_name = azurerm_resource_group.storage.name
  # ... the same settings the pool has today
}

moved {
  from = azapi_resource.pool
  to   = everpure_azure_storage_pool.pool
}
```

3. Run `terraform plan`. It reports the resource as moved, and then compares it
   with the live object. A plan that shows nothing but the move, and perhaps an
   in-place update described below, means the configuration matches.
4. Run `terraform apply`. Once every environment has applied, the `moved` block
   can be deleted.

Unlike an `import` block, a `moved` block can be used inside a child module.

## Reservations

Move a reservation the same way, from the `azapi_resource` of type
`PureStorage.Block/reservations`, and point the storage pools' `reservation_id`
at the new resource:

```terraform
resource "everpure_azure_reservation" "res" {
  name                = "reservation-prod-east"
  resource_group_name = azurerm_resource_group.storage.name
  location            = azurerm_resource_group.storage.location

  offer = {
    offer_id  = "krypton_3_plan"
    plan_id   = "psc_capacity_perf_ondemand"
    term_unit = "P1M"
  }

  lifecycle {
    prevent_destroy = true
  }
}

moved {
  from = azapi_resource.reservation
  to   = everpure_azure_reservation.res
}

resource "everpure_azure_storage_pool" "pool" {
  # ...
  reservation_id = everpure_azure_reservation.res.id
}
```

* `user` is write-only and never read back, so you can leave it out. A moved
  reservation plans clean whether or not the configuration sets it.
* The move takes over the reservation only. If the Marketplace purchase exists
  as its own SaaS resource in the reservation's resource group, it is a
  separate object: keep managing it as you do today.
* Every storage pool in the region depends on the reservation. Keep
  `prevent_destroy`, so that a mistake in the new configuration fails the plan
  instead of planning a replacement.

## Volume groups, volumes and snapshots

Move each nested object with its own `moved` block, and keep the parent
references in the new resources (`storage_pool_id`, `volume_group_id`) pointing
at the new resources:

```terraform
moved {
  from = azapi_resource.volume_group
  to   = everpure_azure_volume_group.vg
}

moved {
  from = azapi_resource.volume
  to   = everpure_azure_volume.data
}

moved {
  from = azapi_resource.snapshot
  to   = everpure_azure_snapshot.nightly_check
}
```

Move the objects in one apply, or one at a time; the order does not matter.

## Virtual machine connections

A virtual machine connection is implemented via the Everpure iSCSI mount
extension on the virtual machine: the connection is the set of volume groups the
virtual machine is connected to, and the extension is how they are connected.
Move the resource that manages the extension today, an `azapi_resource` of type
`Microsoft.Compute/virtualMachines/extensions` or an
`azurerm_virtual_machine_extension`, to
`everpure_azure_virtual_machine_connection`. For the azurerm resource:

```terraform
resource "azurerm_virtual_machine_extension" "iscsi" {
  name                 = "PureStorageMountIscsi"
  virtual_machine_id   = azurerm_linux_virtual_machine.app.id
  publisher            = "PureStorage.Extensions"
  type                 = "MountIscsiLinux"
  type_handler_version = "1.0"
  # ...
}
```

Replace it with the Everpure resource, written to describe the connection the
virtual machine has today, and add the `moved` block:

```terraform
resource "everpure_azure_virtual_machine_connection" "iscsi" {
  virtual_machine_id = azurerm_linux_virtual_machine.app.id
  volume_group_ids   = [everpure_azure_volume_group.vg.id]
  # ...
}

moved {
  from = azurerm_virtual_machine_extension.iscsi
  to   = everpure_azure_virtual_machine_connection.iscsi
}
```

The move keeps the extension's current name. Leave `extension_name` out of the
new configuration, or set it to that exact name: any other value replaces the
resource, which deletes the extension and disconnects every session.

The extension must be the Everpure iSCSI mount extension: publisher
`PureStorage.Extensions` and type `MountIscsiLinux` or `MountIscsiWindows`. An
`azurerm_virtual_machine_extension` with another publisher or type is refused
when the plan starts. For an `azapi_resource`, the plan refreshes the extension
and fails with an error if it is some other extension. A refused move changes no
state.

### What the first plan and apply do

The list of volume groups a virtual machine is connected to cannot be read back
from the extension, so the first plan shows `volume_group_ids` being set, as
after an [import](https://developer.hashicorp.com/terraform/language/import).
The apply sends the extension's settings again, with the targets of exactly the
volume groups in `volume_group_ids`:

* If they are the volume groups the virtual machine is connected to, nothing
  changes on the virtual machine.
* A volume group the virtual machine is connected to that is not in
  `volume_group_ids` is disconnected.

List every volume group the virtual machine should keep before you apply.

## What the plan shows after a move

A move gives the new resource only what the Azure ID tells it, and the refresh
that follows fills in everything Azure reports. As after an
[import](https://developer.hashicorp.com/terraform/language/import), the
settings that exist only in your configuration show up as an in-place update on
the first plan. Applying it changes nothing in Azure; it records them in state.
The one exception is a virtual machine connection, whose apply sends the
extension's settings again, as described above.

| Resource | Shown as an update on the first plan |
|---|---|
| `everpure_azure_reservation` | `timeouts`, if set. `user` is write-only and never shows: a moved reservation plans clean whether or not the configuration carries it |
| `everpure_azure_storage_pool` | `timeouts`, if set |
| `everpure_azure_volume_group` | `deletion_protection` and `eradicate_on_destroy`, when set to anything but their defaults of `true` and `false`; `timeouts`, if set |
| `everpure_azure_volume` | `provisioned_size_in_gib`, when used instead of `provisioned_size_in_bytes`; `deletion_protection` and `eradicate_on_destroy`, when set to anything but their defaults of `true` and `false`; `timeouts`, if set |
| `everpure_azure_snapshot` | `deletion_protection` and `eradicate_on_destroy`, when set to anything but their defaults of `true` and `false`; `timeouts`, if set |
| `everpure_azure_virtual_machine_connection` | `volume_group_ids`, as described above; `timeouts`, if set |

The `source_*` arguments of a volume group or a volume only say how it was
created, and Azure never reports them back. A moved object always counts as
having no source, so leave those arguments out of the new configuration.
Setting a source would plan a replacement.

## Snapshots taken by a schedule are refused

A volume group's snapshot schedule creates snapshots and deletes them again
when their retention ends, so Terraform cannot manage them. If the
`azapi_resource` is such a snapshot, the plan fails with an error naming it, and
the azapi state is left untouched. Read it with the `everpure_azure_snapshot` or
`everpure_azure_snapshots` data source instead. Only snapshots you took by hand
can be moved.

## If a move is refused

The error says why. The usual causes are a `moved` block whose `from` and `to`
do not match in kind (a volume moved to `everpure_azure_volume_group`, say), a
source whose `id` is not the ID of that kind of object, and an extension that is
not the Everpure iSCSI mount extension. Fix the block and run the plan again; a
refused move changes no state.
