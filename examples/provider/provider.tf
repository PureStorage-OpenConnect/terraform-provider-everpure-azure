terraform {
  required_providers {
    everpure = {
      source = "PureStorage-OpenConnect/everpure-azure"
    }
  }
}

# Credentials: `az login` by default, or the ARM_* environment variables /
# provider attributes described in the Authentication guide. subscription_id
# falls back to ARM_SUBSCRIPTION_ID then AZURE_SUBSCRIPTION_ID when unset.
provider "everpure" {
  subscription_id = "00000000-0000-0000-0000-000000000000"
}

# Alternative: an explicit service principal, with the secret from a
# sensitive variable rather than committed to configuration. See the
# Authentication guide for OpenID Connect, managed identity and other
# methods.
#
# variable "client_secret" {
#   type      = string
#   sensitive = true
# }
#
# provider "everpure" {
#   subscription_id = "00000000-0000-0000-0000-000000000000"
#   tenant_id       = "11111111-1111-1111-1111-111111111111"
#   client_id       = "22222222-2222-2222-2222-222222222222"
#   client_secret   = var.client_secret
# }
