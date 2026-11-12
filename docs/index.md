---
page_title: "Everpure Azure Provider"
description: |-
  Manages Everpure resources on Azure through the PureStorage.Block Azure Resource Manager API.
---

# Everpure Azure Provider

Manage Everpure resources on Azure — enterprise block storage delivered as an
Azure native service — through the `PureStorage.Block` Azure Resource Manager
API.

Resources managed by this provider are ordinary ARM resources, so their IDs can
be passed to and from `azurerm` resources such as management locks and role
assignments.

## Requirements

* Terraform 1.0 or later for resources and data sources. Some features need a
  newer Terraform:

  | Feature | Terraform |
  |---|---|
  | Resources and data sources | 1.0 |
  | Ephemeral resource `everpure_azure_platform_console_activation_credentials` | 1.10 |
  | Passing its password to a write-only attribute | 1.11 |
  | Import by resource identity (`import` block with `identity`) | 1.12 |
  | List resources (`terraform query`) and actions | 1.14 |
  | `caller` in an `action_trigger` | 1.16 |

* An Azure subscription onboarded to Everpure, with a **Reservation** already
  purchased in the region you want to deploy in. A storage pool cannot be
  created without one.
* A subnet delegated to `PureStorage.Block/storagePools` for the pool to be
  injected into.

## Provider configuration

Declare the provider under the local name `everpure`:

```terraform
terraform {
  required_providers {
    everpure = {
      source = "PureStorage-OpenConnect/everpure-azure"
    }
  }
}
```

The local name must be `everpure`, because resource type names begin with
`everpure_`. The "Use provider" snippet on the registry page shows a different
local name; change it to `everpure`.

## Example Usage

```terraform
# A worked example: a storage pool injected into a virtual network you own.
#
# The network side is built with the standard `azurerm` provider; only the pool
# itself comes from this provider. Replace the placeholder subscription ID and
# reservation ID with your own before running.

terraform {
  required_providers {
    everpure = {
      source = "PureStorage-OpenConnect/everpure-azure"
    }
    azurerm = {
      source  = "hashicorp/azurerm"
      version = ">= 4.0"
    }
  }
}

provider "everpure" {
  subscription_id = var.subscription_id
}

provider "azurerm" {
  features {}
  subscription_id = var.subscription_id
}

variable "subscription_id" {
  type        = string
  description = "Subscription the pool and its network are created in."
}

variable "reservation_id" {
  type        = string
  description = <<-EOT
    Full ARM ID of an existing Reservation in the same subscription and region,
    e.g. /subscriptions/.../providers/PureStorage.Block/reservations/prod.
  EOT
}

resource "azurerm_resource_group" "example" {
  name     = "rg-storage-prod"
  location = "eastus2"
}

resource "azurerm_virtual_network" "example" {
  name                = "vnet-prod"
  resource_group_name = azurerm_resource_group.example.name
  location            = azurerm_resource_group.example.location
  address_space       = ["10.0.0.0/16"]
}

# The pool's network interfaces are placed in a subnet delegated to the
# PureStorage.Block service. The delegation is required.
resource "azurerm_subnet" "storage" {
  name                 = "snet-storage"
  resource_group_name  = azurerm_resource_group.example.name
  virtual_network_name = azurerm_virtual_network.example.name
  address_prefixes     = ["10.0.1.0/24"]

  delegation {
    name = "everpure"
    service_delegation {
      name = "PureStorage.Block/storagePools"
    }
  }
}

resource "everpure_azure_storage_pool" "prod" {
  name                = "pool-prod-east"
  resource_group_name = azurerm_resource_group.example.name
  location            = azurerm_resource_group.example.location

  # Changing the zone replaces the pool — a full teardown and rebuild.
  zone = "1"

  # Megabytes per second. Can be changed in place.
  provisioned_bandwidth_in_mbps = 2048

  reservation_id = var.reservation_id

  vnet_injection {
    subnet_id = azurerm_subnet.storage.id
  }

  # Enables platform console access (GUI, REST API and CLI) from the injected
  # subnet. Remove the block to disable console access.
  platform_console {
    gui_enabled = true
    api_enabled = true
    cli_enabled = false
  }

  tags = {
    environment = "prod"
  }

  # Pool deletion is permanent. This makes any plan that would destroy or
  # replace the pool fail. See the "Deletion policies" guide.
  lifecycle {
    prevent_destroy = true
  }
}

output "storage_pool_id" {
  value = everpure_azure_storage_pool.prod.id
}

output "storage_pool_provisioned_iops" {
  value = everpure_azure_storage_pool.prod.provisioned_iops
}
```

## Authentication

The provider block takes one optional argument, `subscription_id` — the
subscription resources are managed in. If unset, it falls back to the
`ARM_SUBSCRIPTION_ID` then `AZURE_SUBSCRIPTION_ID` environment variables; one
of the three must resolve to a value.

Credentials are resolved the same way `azurerm` and `azapi` resolve them: a
service principal (secret, certificate or OpenID Connect), managed identity,
or the Azure CLI, which is what a provider block with nothing configured
uses. See the
[Authentication guide](https://registry.terraform.io/providers/PureStorage-OpenConnect/everpure-azure/latest/docs/guides/authentication)
for every method, its attributes and environment variables, and worked
examples for CI, GitHub Actions, Azure Pipelines and AKS.

## Timing

Storage pool creation is a long-running operation and can take up to 90
minutes. `terraform apply` blocks until Azure reports the operation complete;
set CI timeouts accordingly. Cancelling an in-flight apply does not cancel the
Azure operation — the pool is still created, but is no longer tracked in state.

## Deleting resources

Delete behaviour differs per resource type, and some deletes fail by design
while a resource still holds data. See
[Deletion policies](https://registry.terraform.io/providers/PureStorage-OpenConnect/everpure-azure/latest/docs/guides/deletion-policies).

## Getting support

Every error from this provider includes an `x-ms-correlation-request-id`
value — one ID shared by every API call the provider made during that
`terraform` run. Include it when filing a support case; it's what support
uses to locate the matching server-side request logs.

<!-- schema generated by tfplugindocs -->
## Schema

### Optional

- `auxiliary_tenant_ids` (List of String) A list of the Tenant IDs of Microsoft Entra ID tenants other than the one specified in `tenant_id`, for multi-tenancy. This can also be sourced from the `ARM_AUXILIARY_TENANT_IDS` environment variable as a `;`-separated list. An empty list is treated the same as omitting the attribute and still falls back to the environment variable.
- `client_certificate` (String, Sensitive) Base64-encoded client certificate, for authenticating as a service principal: either a PKCS#12 (.pfx/.p12) bundle, or a PEM file holding the certificate and its unencrypted private key. This can also be sourced from the `ARM_CLIENT_CERTIFICATE` environment variable.
- `client_certificate_password` (String, Sensitive) Password for a PKCS#12 bundle given in `client_certificate` or `client_certificate_path`; a PEM private key must be unencrypted. This can also be sourced from the `ARM_CLIENT_CERTIFICATE_PASSWORD` or `AZURE_CLIENT_CERTIFICATE_PASSWORD` environment variables.
- `client_certificate_path` (String) Path to a client certificate: either a PKCS#12 (.pfx/.p12) bundle, or a PEM file holding the certificate and its unencrypted private key. This can also be sourced from the `ARM_CLIENT_CERTIFICATE_PATH` or `AZURE_CLIENT_CERTIFICATE_PATH` environment variables.
- `client_id` (String) The Client ID which should be used. This can also be sourced from the `ARM_CLIENT_ID` or `AZURE_CLIENT_ID` environment variables. When `use_msi` is `true`, this is the client ID of the user-assigned identity to use, rather than the VM's system-assigned one.
- `client_id_file_path` (String) Path to a file containing the Client ID which should be used. This can also be sourced from the `ARM_CLIENT_ID_FILE_PATH` environment variable.
- `client_secret` (String, Sensitive) The Client Secret which should be used, for authenticating as a service principal. This can also be sourced from the `ARM_CLIENT_SECRET` or `AZURE_CLIENT_SECRET` environment variables.
- `client_secret_file_path` (String) Path to a file containing the Client Secret which should be used. This can also be sourced from the `ARM_CLIENT_SECRET_FILE_PATH` environment variable.
- `oidc_azure_service_connection_id` (String) The Azure Pipelines service connection ID to request an OIDC token through. This can also be sourced from the `ARM_ADO_PIPELINE_SERVICE_CONNECTION_ID`, `ARM_OIDC_AZURE_SERVICE_CONNECTION_ID` or `AZURESUBSCRIPTION_SERVICE_CONNECTION_ID` environment variables. Its presence selects the Azure Pipelines OIDC flow over the GitHub Actions one. `oidc_request_token` (or `ARM_OIDC_REQUEST_TOKEN` / `SYSTEM_ACCESSTOKEN`) must also resolve to a value; the service connection ID alone is not enough.
- `oidc_request_token` (String, Sensitive) The bearer token to present when requesting an OIDC token, for GitHub Actions or Azure Pipelines. This can also be sourced from the `ARM_OIDC_REQUEST_TOKEN`, `ACTIONS_ID_TOKEN_REQUEST_TOKEN` or `SYSTEM_ACCESSTOKEN` environment variables.
- `oidc_request_url` (String) The URL to request an OIDC token from, for GitHub Actions. This can also be sourced from the `ARM_OIDC_REQUEST_URL` or `ACTIONS_ID_TOKEN_REQUEST_URL` environment variables.
- `oidc_token` (String, Sensitive) The OIDC token to use for authentication. This can also be sourced from the `ARM_OIDC_TOKEN` environment variable.
- `oidc_token_file_path` (String) Path to a file containing an OIDC token to use for authentication. This can also be sourced from the `ARM_OIDC_TOKEN_FILE_PATH` environment variable, or from `AZURE_FEDERATED_TOKEN_FILE` when `use_aks_workload_identity` is `true`.
- `subscription_id` (String) ID of the subscription resources are managed in. Falls back to the `ARM_SUBSCRIPTION_ID` then `AZURE_SUBSCRIPTION_ID` environment variables when unset. One of the three must resolve to a value.
- `tenant_id` (String) The Tenant ID which should be used. This can also be sourced from the `ARM_TENANT_ID` or `AZURE_TENANT_ID` environment variables.
- `use_aks_workload_identity` (Boolean) Allow Azure AKS Workload Identity to be used for authentication. This can also be sourced from the `ARM_USE_AKS_WORKLOAD_IDENTITY` environment variable. Defaults to `false`. When `true`, `client_id`, `tenant_id` and `oidc_token_file_path` default from the `AZURE_CLIENT_ID`, `AZURE_TENANT_ID` and `AZURE_FEDERATED_TOKEN_FILE` environment variables the AKS pod identity webhook sets, and a `tenant_id` that disagrees with `AZURE_TENANT_ID` is an error.
- `use_cli` (Boolean) Allow Azure CLI to be used for authentication. This can also be sourced from the `ARM_USE_CLI` environment variable. Defaults to `true`.
- `use_msi` (Boolean) Allow Managed Identity to be used for authentication. This can also be sourced from the `ARM_USE_MSI` environment variable. Defaults to `false`.
- `use_oidc` (Boolean) Allow OpenID Connect to be used for authentication. This can also be sourced from the `ARM_USE_OIDC` environment variable. Defaults to `false`.
