---
page_title: "Authentication"
subcategory: "Guides"
description: |-
  Every way to authenticate the provider: the Azure CLI, a service
  principal, OpenID Connect, and managed identity, with the exact attribute
  and environment variable for each.
---

# Authentication

The provider authenticates the same way `azurerm` and `azapi` do: pick a
method below, set the attributes or environment variables it needs, or set
nothing and use your Azure CLI login.

## Methods

| Method | Provider attributes | Environment variables |
|---|---|---|
| Client secret | `tenant_id`, `client_id`, `client_secret` (or `client_secret_file_path`) | `ARM_TENANT_ID`, `ARM_CLIENT_ID`, `ARM_CLIENT_SECRET` (or `ARM_CLIENT_SECRET_FILE_PATH`) |
| Client certificate | `tenant_id`, `client_id`, `client_certificate` or `client_certificate_path`, `client_certificate_password` | `ARM_TENANT_ID`, `ARM_CLIENT_ID`, `ARM_CLIENT_CERTIFICATE` or `ARM_CLIENT_CERTIFICATE_PATH`, `ARM_CLIENT_CERTIFICATE_PASSWORD` |
| OpenID Connect (generic token) | `use_oidc`, `tenant_id`, `client_id`, `oidc_token` or `oidc_token_file_path` | `ARM_USE_OIDC`, `ARM_TENANT_ID`, `ARM_CLIENT_ID`, `ARM_OIDC_TOKEN` or `ARM_OIDC_TOKEN_FILE_PATH` |
| OpenID Connect (GitHub Actions) | `use_oidc`, `tenant_id`, `client_id` | `ARM_USE_OIDC`, `ARM_TENANT_ID`, `ARM_CLIENT_ID` (token and request URL come from GitHub's own `ACTIONS_ID_TOKEN_REQUEST_TOKEN` / `ACTIONS_ID_TOKEN_REQUEST_URL`) |
| OpenID Connect (Azure Pipelines) | `use_oidc`, `tenant_id`, `client_id`, `oidc_azure_service_connection_id`, `oidc_request_token` | `ARM_USE_OIDC`, `ARM_TENANT_ID`, `ARM_CLIENT_ID`, `ARM_ADO_PIPELINE_SERVICE_CONNECTION_ID`, `ARM_OIDC_REQUEST_TOKEN` or `SYSTEM_ACCESSTOKEN` |
| AKS workload identity | `use_aks_workload_identity` | `ARM_USE_AKS_WORKLOAD_IDENTITY` (`AZURE_CLIENT_ID`, `AZURE_TENANT_ID` and `AZURE_FEDERATED_TOKEN_FILE` are set by the AKS pod identity webhook) |
| Managed identity | `use_msi`, `client_id` (for a user-assigned identity) | `ARM_USE_MSI`, `ARM_CLIENT_ID` |
| Azure CLI | `use_cli` (default `true`) | `ARM_USE_CLI` |

`auxiliary_tenant_ids` (`ARM_AUXILIARY_TENANT_IDS`, a `;`-separated list) adds
tenants for cross-tenant resource access; it is independent of which method
above is used.

## Precedence

Each attribute above resolves independently, in this order:

1. The provider block attribute.
2. Its `ARM_*` environment variable.
3. Its `AZURE_*` environment variable, for the attributes that have one (see
   the full list in the
   [provider schema](https://registry.terraform.io/providers/PureStorage-OpenConnect/everpure-azure/latest/docs)).

This means a value from any source can be mixed with values from another -
for example, `tenant_id` and `client_id` from `ARM_*` environment variables in
CI, with `client_secret` passed as a provider block attribute from a secret
store.

Once every attribute is resolved, every method with enough information
configured is added to one credential chain. The chain is built by the same
library the `azapi` provider uses, so the order and the fall-through
behaviour below are identical to `azapi`'s. The order is always:

1. OIDC token
2. OIDC token file
3. OIDC token request (GitHub Actions or Azure Pipelines)
4. Client secret
5. Client certificate
6. Managed identity
7. Azure CLI

When a token is actually needed, the chain tries its methods in that order
and uses the first one that returns a token. A method that reaches
Microsoft Entra ID and is rejected (a wrong secret, an expired certificate)
stops the chain there and reports its error; only a method that cannot run
at all - Azure CLI without `az login`, for example - is skipped in favour of
the next one. The methods table above is ordered for reading, not by this
precedence.

Values from the provider block and from environment variables are merged
before the chain order applies, so an environment variable that enables a
different method - an inherited `ARM_CLIENT_SECRET`, say, alongside a
`client_certificate_path` set in the block - takes part in the chain too, and
can be tried ahead of the method configured in the block. This matches how
the `azurerm` and `azapi` providers behave. The provider's `TF_LOG=INFO`
output lists the chained methods and where each one was configured.

## Default: Azure CLI

With nothing else configured, the chain holds Azure CLI only (`use_cli`
defaults to `true`), so a bare

```hcl
provider "everpure" {
  subscription_id = "00000000-0000-0000-0000-000000000000"
}
```

authenticates as whatever `az login` last authenticated as, with no further
attributes needed. Setting `use_cli = false` with no other method configured
is a configuration error: set another method, or leave `use_cli` at its
default.

## Examples

### Local development: Azure CLI

```shell
az login
az account set --subscription 00000000-0000-0000-0000-000000000000
terraform apply
```

No provider attributes are needed; `use_cli` defaults to `true`.

### CI: `ARM_*` environment variables, shared with `azurerm`

```shell
export ARM_TENANT_ID=...
export ARM_CLIENT_ID=...
export ARM_CLIENT_SECRET=...
export ARM_SUBSCRIPTION_ID=...
terraform apply
```

The same variables configure an `azurerm` provider block in the same
configuration.

### Service principal in the provider block

```hcl
variable "client_secret" {
  type      = string
  sensitive = true
}

provider "everpure" {
  subscription_id = "00000000-0000-0000-0000-000000000000"
  tenant_id       = "11111111-1111-1111-1111-111111111111"
  client_id       = "22222222-2222-2222-2222-222222222222"
  client_secret   = var.client_secret
}
```

### GitHub Actions: OpenID Connect

```hcl
provider "everpure" {
  subscription_id = "00000000-0000-0000-0000-000000000000"
  tenant_id       = "11111111-1111-1111-1111-111111111111"
  client_id       = "22222222-2222-2222-2222-222222222222"
  use_oidc        = true
}
```

No further attributes are needed: GitHub's `ACTIONS_ID_TOKEN_REQUEST_TOKEN`
and `ACTIONS_ID_TOKEN_REQUEST_URL` are picked up automatically. The workflow
needs `permissions: { id-token: write }`.

### Azure Pipelines: OpenID Connect

```hcl
provider "everpure" {
  subscription_id                  = "00000000-0000-0000-0000-000000000000"
  tenant_id                        = "11111111-1111-1111-1111-111111111111"
  client_id                        = "22222222-2222-2222-2222-222222222222"
  use_oidc                         = true
  oidc_azure_service_connection_id = "my-service-connection"
}
```

Azure Pipelines does not expose `SYSTEM_ACCESSTOKEN` to a step unless the
pipeline YAML maps it explicitly, so `oidc_request_token` needs this alongside
the `terraform apply` step:

```yaml
- script: terraform apply -auto-approve
  env:
    SYSTEM_ACCESSTOKEN: $(System.AccessToken)
```

This flow also needs `SYSTEM_OIDCREQUESTURI`, which Azure Pipelines sets
automatically for every job - there is nothing to configure for it, but it
means `oidc_azure_service_connection_id` only works when the run is actually
inside an Azure Pipelines job.

### Managed identity or AKS workload identity

On an Azure host with a managed identity, `use_msi = true` is required to use
it - it is also needed to select a user-assigned identity via `client_id`.

```hcl
provider "everpure" {
  subscription_id = "00000000-0000-0000-0000-000000000000"
  use_msi         = true
}
```

Running inside AKS with workload identity, `use_aks_workload_identity = true`
is required to use it. That flag, plus the `AZURE_CLIENT_ID`,
`AZURE_TENANT_ID` and `AZURE_FEDERATED_TOKEN_FILE` environment variables the
AKS pod identity webhook sets, is all that's needed - no other provider
attributes.

## Azure public cloud only

The provider always authenticates against Azure public cloud. There is no
`environment` setting, and sovereign clouds (Azure Government, Azure China)
are not supported.

## Permissions

Authentication only establishes which identity the provider acts as.
Authorization is granted through Azure role assignments on that identity, as
for any other Azure resource. Which permissions are needed depends on the
resources you manage and the Azure resources they reference.
