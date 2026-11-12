// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/provider"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/purestorage-openconnect/everpure-azure",
	}); err != nil {
		log.Fatal(err)
	}
}
