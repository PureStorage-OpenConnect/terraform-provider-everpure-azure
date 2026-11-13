---
page_title: "Connecting a virtual machine to Everpure storage"
subcategory: "Guides"
description: |-
  Step by step: create a storage pool, a volume group and a volume, then
  connect an Azure virtual machine to them over iSCSI.
---

# Connecting a virtual machine to Everpure storage

This guide walks through provisioning Everpure block storage on Azure and
attaching it to an existing virtual machine. By the end, the VM sees a new
100 GiB disk served by Everpure over iSCSI.

## How the pieces fit together

Everpure resources nest inside each other:

* A **Reservation** holds the purchased capacity and performance for a region.
  It is bought outside Terraform and is the starting point for everything
  else.
* A **storage pool** uses the Reservation and is placed into a subnet of
  your virtual network, where your VMs can reach it.
* A **volume group** lives in a storage pool. It is the unit VMs connect to,
  and the unit that carries performance limits and snapshot schedules.
* A **volume** lives in a volume group. It is what the VM sees as a disk.
* A **virtual machine connection** attaches a VM to one or more volume groups,
  making every volume in them visible to it.

The guide builds these one step at a time, one file per step.

## Before you begin

You need:

* An Azure subscription onboarded to Everpure, with a **Reservation** in the
  region you want to deploy in.
* The `PureStorage.Block` resource provider registered on the subscription:

  ```shell
  az provider register --namespace PureStorage.Block
  ```

* A subnet delegated to `PureStorage.Block/storagePools`, for the storage pool
  to be placed into. With `azurerm`, this is a
  [`delegation`](https://registry.terraform.io/providers/hashicorp/azurerm/latest/docs/resources/subnet#delegation)
  block on the subnet.
* A Linux or Windows virtual machine that can reach that subnet. The simplest
  setup is a VM in the same virtual network.
* An Azure identity with `Contributor` on the resource group that holds the
  storage pool, on the resource group that holds the virtual network and the
  virtual machine, and on the Reservation, signed in where Terraform runs, for
  example with `az login`.
  See
  [Authentication](https://registry.terraform.io/providers/PureStorage-OpenConnect/everpure-azure/latest/docs#authentication)
  for other ways to sign in.

## Project layout

```text
.
├── providers.tf     # Step 1
├── variables.tf     # Step 1
├── storage_pool.tf  # Step 2
├── volumes.tf       # Step 3
└── connection.tf    # Step 4
```

## Step 1: Configure the provider

```terraform
terraform {
  required_providers {
    everpure = {
      source = "PureStorage-OpenConnect/everpure-azure"
    }
  }
}

provider "everpure" {
  subscription_id = var.subscription_id
}
```

The configuration takes the existing pieces from the prerequisites as
variables:

```terraform
variable "subscription_id" {
  type = string
}

variable "resource_group_name" {
  type        = string
  description = "Resource group the Everpure resources are created in."
}

variable "location" {
  type        = string
  description = "Azure region of the Reservation and the virtual network."
}

variable "reservation_id" {
  type        = string
  description = "Full ARM ID of the Everpure Reservation that funds the storage pool."
}

variable "subnet_id" {
  type        = string
  description = "Full ARM ID of the subnet delegated to PureStorage.Block/storagePools."
}

variable "virtual_machine_id" {
  type        = string
  description = "Full ARM ID of the virtual machine to connect."
}
```

Put the values in a `terraform.tfvars` file:

```hcl
subscription_id     = "00000000-0000-0000-0000-000000000000"
resource_group_name = "rg-everpure"
location            = "eastus2"
reservation_id      = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-everpure/providers/PureStorage.Block/reservations/reservation-example"
subnet_id           = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-app/providers/Microsoft.Network/virtualNetworks/vnet-app/subnets/snet-everpure"
virtual_machine_id  = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-app/providers/Microsoft.Compute/virtualMachines/vm-app"
```

## Step 2: Create the storage pool

```terraform
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
```

* `location` must be the region of both the Reservation and the subnet.
* `zone` is the availability zone. Use `""` in a region without availability
  zones.
* `provisioned_bandwidth_in_mbps` is the pool's bandwidth in MB/s.
  It can be changed later without recreating the pool.

~> Creating a storage pool takes a long time, up to about 90 minutes.
`terraform apply` waits until it is done.

See
[`everpure_azure_storage_pool`](https://registry.terraform.io/providers/PureStorage-OpenConnect/everpure-azure/latest/docs/resources/storage_pool)
for the other options, such as tags and platform console access.

## Step 3: Create a volume group and a volume

```terraform
resource "everpure_azure_volume_group" "example" {
  name            = "vg-example"
  storage_pool_id = everpure_azure_storage_pool.example.id
}

resource "everpure_azure_volume" "example" {
  name                    = "vol-example"
  volume_group_id         = everpure_azure_volume_group.example.id
  provisioned_size_in_gib = 100
}
```

The volume can be grown later by raising `provisioned_size_in_gib`; it
cannot be shrunk.

See
[`everpure_azure_volume_group`](https://registry.terraform.io/providers/PureStorage-OpenConnect/everpure-azure/latest/docs/resources/volume_group)
for performance limits and snapshot schedules, and
[`everpure_azure_volume`](https://registry.terraform.io/providers/PureStorage-OpenConnect/everpure-azure/latest/docs/resources/volume)
for cloning and restoring volumes.

## Step 4: Connect the virtual machine

```terraform
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
```

This connects the VM to every volume group in `volume_group_ids` over iSCSI.
The provider looks up the volume groups' iSCSI endpoints itself and sets up the
connection on the VM through the Everpure iSCSI VM extension, picking the Linux
or Windows version from the VM's operating system.

~> Connecting a VM to a volume group that has no volumes fails. The connection
refers only to the volume group, so on its own Terraform may create it before
the volume, while the volume group is still empty. The `depends_on` line makes
Terraform create the volume first. Keep it, and list every volume the VM
should see: a volume created after the connection is not visible on the VM
until its iSCSI sessions are rescanned by hand.

-> On a Windows VM, also set `reboot_allowed = true`. The first setup on
Windows needs a reboot to finish; without it, the VM has to be rebooted by
hand.

See
[`everpure_azure_virtual_machine_connection`](https://registry.terraform.io/providers/PureStorage-OpenConnect/everpure-azure/latest/docs/resources/virtual_machine_connection)
for session counts, initiator names and Windows-specific settings.

-> Configuring iSCSI yourself instead (your own initiator setup, cloud-init,
configuration management)? Skip this step and read the volume group's portal
IPs, ports and IQN from the
[`everpure_azure_volume_group_connection_parameters` data source](https://registry.terraform.io/providers/PureStorage-OpenConnect/everpure-azure/latest/docs/data-sources/volume_group_connection_parameters)
instead.

## Step 5: Deploy

```shell
terraform init
terraform plan
terraform apply
```

## Step 6: Check the disk on the VM

On Linux:

```shell
sudo iscsiadm -m session
lsblk
```

On Windows:

```powershell
Get-IscsiSession
Get-Disk
```

The volume shows up as a new 100 GiB disk. From here it is an ordinary disk:
partition, format and mount it as you would any other.

## Next steps

* **More volume groups**: add them to `volume_group_ids`, add their volumes
  to `depends_on`, and apply. Volume groups already
  connected are left untouched.
* **More VMs**: add one `everpure_azure_virtual_machine_connection` per VM.

## Cleaning up

Volume groups and volumes are protected from deletion by default, and a
destroyed volume group or volume is kept as recoverable, which still blocks
deleting the pool. To remove everything, first add these two lines to both
`everpure_azure_volume_group.example` and `everpure_azure_volume.example`, and
apply:

```hcl
  deletion_protection  = false
  eradicate_on_destroy = true
```

Then destroy:

```shell
terraform destroy
```

Destroying the connection disconnects the VM from its volume groups before
removing the extension. See
[Deletion policies](https://registry.terraform.io/providers/PureStorage-OpenConnect/everpure-azure/latest/docs/guides/deletion-policies)
for the full picture.
