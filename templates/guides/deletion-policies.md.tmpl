---
page_title: "Deletion policies"
subcategory: "Guides"
description: |-
  What destroy does to each resource type: what is recoverable, which deletes
  fail by design, and how to tear down a full stack.
---

# Deletion policies

Delete behaviour differs per resource type. Storage pools and reservations are
deleted permanently; volume groups, volumes and snapshots are soft-deleted and
stay recoverable until eradication.

## When Terraform destroys a resource

1. The resource was removed from the configuration.
2. An attribute that cannot be updated in place was changed. Terraform plans a
   destroy followed by a create, marked *"forces replacement"* in the plan.

For example, changing an `everpure_azure_storage_pool`'s `zone` or `vnet_injection`
forces replacement: Terraform destroys the existing pool and creates a new one,
losing everything stored on it.

## Behaviour by resource type

| Resource | Delete | Recoverable | Guard |
|---|---|---|---|
| Storage pool | Permanent | No | Fails while the pool holds volume groups, volumes or other resources, including soft-deleted ones |
| Reservation | Permanent | No | Fails while the reservation has storage pools |
| Volume group | Soft delete | Until eradication | `deletion_protection`, default `true` |
| Volume | Soft delete | Until eradication | `deletion_protection`, default `true` |
| Snapshot | Soft delete | Until eradication | `deletion_protection`, default `true` |

## Storage pools and reservations

There is no soft delete. A successful delete is permanent.

The API refuses to delete a non-empty pool, or a reservation that still has
pools. The provider surfaces that failure as-is, with an error naming what
blocks the delete. It does not cascade or force-delete children.

The guard does not protect an empty pool or a childless reservation. Use
`prevent_destroy` for those:

```hcl
resource "everpure_azure_storage_pool" "prod" {
  # ...

  lifecycle {
    prevent_destroy = true
  }
}
```

Any plan that would destroy the pool then fails, including a plan that would
replace it after a `zone` or `vnet_injection` change.

-> `prevent_destroy` is configuration, not state. Removing the `resource` block
removes the protection with it, and the destroy proceeds. For protection that
survives that, use an Azure
[management lock](https://learn.microsoft.com/en-us/azure/azure-resource-manager/management/lock-resources).

## Volume groups, volumes and snapshots

Destroy performs a soft delete: the API renames the object server-side, marks
it destroyed, and eradicates it at its eradication timestamp, derived from the
pool's data retention period. It is recoverable until then. The
`soft_deletion.eradication_timestamp` the data sources report is approximate:
it is derived from the remaining retention when read, so it can change by
seconds to minutes between reads. Use `soft_deletion.destroyed_at` as the
stable reference.

* The rename frees the name immediately, so recreating under the same name
  always works.
* `Read` treats a soft-deleted object as absent — it is removed from state.
* List data sources exclude destroyed objects by default.
* Recovery is explicit: create a resource with
  `source_type = "recoverableVolumeGroup"` (or the volume equivalent) and the
  recoverable object's ID. A create never adopts a soft-deleted object
  implicitly.
* For a volume, recovery is `source_type = "recoverableVolume"` plus
  `source_recoverable_volume_id`, the soft-deleted volume's own resource ID —
  its name carries a timestamp suffix added when it was destroyed. The
  recoverable volume must be in the same volume group as the volume being
  created; recovering into a different volume group is rejected.
* For a snapshot, recovery is `source_recoverable_snapshot_id`, the
  soft-deleted snapshot's own resource ID — its name carries the same timestamp
  suffix. Recovery restores that snapshot under the new snapshot's `name`; it
  does not copy it, so the soft-deleted snapshot no longer exists afterwards.
  The source must be in the same volume group. A snapshot taken by the volume
  group's snapshot schedule cannot be recovered through Terraform, and cannot be
  managed by an `everpure_azure_snapshot` resource at all.

Destroying a volume group takes everything inside it along. Its volumes and
snapshots, including ones Terraform doesn't manage, are soft-deleted with it as
one unit after any hosts are disconnected, and eradicating the volume group
(directly, or through `eradicate_on_destroy`) eradicates them too. Unlike a
pool, the API does not refuse to delete a non-empty volume group: the volume
group's own `deletion_protection` is the guard.

Recovering a volume group restores only the volumes still inside it when it was
deleted. After a full-stack `terraform destroy`, the volumes were destroyed first
and are soft-deleted separately, so each needs its own
`source_type = "recoverableVolume"` recovery.

### `eradicate_on_destroy`

Per-resource boolean, default `false`. When `true`, destroy eradicates the
resource instead of soft-deleting it. There is no recovery window.

```hcl
resource "everpure_azure_volume_group" "scratch" {
  # ...

  eradicate_on_destroy = true
}
```

### `deletion_protection`

Per-resource boolean on volume groups, volumes and snapshots, default `true`. While
`true`, the provider refuses to delete the resource; the error names the
attribute to change.

Differences from `prevent_destroy`:

* It defaults to on.
* It lives in state, so it survives removal of the `resource` block.

Deleting a protected resource therefore takes two applies: set
`deletion_protection = false` and apply, then remove the resource and apply.

It guards only deletes made through this state. Another state, the Portal, the CLI
or the API can still delete the object.

The two flags are orthogonal: `deletion_protection` controls whether a delete
happens, `eradicate_on_destroy` controls whether it is soft or permanent.

## Tearing down a full stack

Terraform destroys leaf-first — volumes and snapshots, volume groups, pool, reservation —
which matches the order the API guards require. But soft-deleted children still
block the pool delete, so a full teardown takes two applies:

1. On every volume, snapshot and volume group, set `eradicate_on_destroy = true` and
   `deletion_protection = false`. Remove `prevent_destroy` from the pool and
   reservation. Apply.
2. Run `terraform destroy`.

There is no single flag that authorises this.

## Recovering from a failed pool delete

If a stack is destroyed without setting `eradicate_on_destroy`, its volumes,
snapshots and volume groups are soft-deleted and dropped from state. The pool delete then
fails, because soft-deleted objects still count as occupants and Terraform can
no longer see them.

Resolve it outside Terraform:

1. List the destroyed volume groups, volumes, snapshots and any other resources still on
   the pool via the Azure Portal or CLI.
2. Eradicate them, or wait for automatic eradication.
3. Re-run `terraform destroy`.

## Resources not managed by Terraform

Volume groups, volumes and other resources created outside Terraform also
block a pool delete.
Terraform does not manage them and will not remove them. To delete them through
Terraform, import them first:

```shell
terraform import everpure_azure_volume_group.adopted \
  "/subscriptions/.../providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg1"
```

Then set the deletion attributes and destroy as above.
