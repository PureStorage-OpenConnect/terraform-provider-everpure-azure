# terraform-provider-everpure-azure

Terraform provider for Everpure on Azure — manage storage resources through the
`PureStorage.Block` Azure Resource Manager API.

## Building

Requires [Go](https://go.dev/doc/install) (see `go.mod` for the version).

```sh
make build       # produces ./terraform-provider-everpure-azure
make test        # unit + integration tests (downloads a pinned terraform binary if none is on PATH)
make test-unit   # unit tests only, fast loop
make vet fmt     # static checks / formatting
```

## Documentation

Full documentation is published on the Terraform Registry:

* [Provider index](https://registry.terraform.io/providers/PureStorage-OpenConnect/everpure-azure/latest/docs)
  — getting started, authentication, worked examples
* [Deletion policies](https://registry.terraform.io/providers/PureStorage-OpenConnect/everpure-azure/latest/docs/guides/deletion-policies)
  — what `terraform destroy` does to each resource, and what is recoverable

The same pages are in [`docs/`](docs/) in this repository.

## Authentication

By default the provider authenticates with the Azure CLI (`az login`). It
also accepts the same azurerm/azapi-style `ARM_*` environment variables and
provider block attributes for a service principal (secret, certificate or
OpenID Connect), managed identity, or AKS workload identity. The
subscription to manage resources in is set via the provider's
`subscription_id` argument, or falls back to `ARM_SUBSCRIPTION_ID` then
`AZURE_SUBSCRIPTION_ID`. See the
[Authentication guide](https://registry.terraform.io/providers/PureStorage-OpenConnect/everpure-azure/latest/docs/guides/authentication)
for every method.

## Local development

Point Terraform at your local build via `~/.terraformrc`:

```hcl
provider_installation {
  dev_overrides {
    "registry.terraform.io/purestorage-openconnect/everpure-azure" = "/path/to/terraform-provider-everpure-azure"
  }
  direct {}
}
```

Then run the example:

```sh
make build
terraform -chdir=examples/getting_started apply \
  -var subscription_id=<your-subscription-id> \
  -var reservation_id=<your-reservation-arm-id>
```

See [examples/](examples/) for usage.

## Support

This repository is a read-only mirror of the provider source. Issues and pull
requests are not accepted here; for help or to report a problem, contact
Everpure support.

## License

[Apache-2.0](LICENSE)
