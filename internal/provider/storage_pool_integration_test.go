// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azcorefake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	psbfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock/fake"
	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/config"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

const (
	testSubscriptionID = "11111111-1111-1111-1111-111111111111"
	testResourceGroup  = "rg-test"
)

func testReservationID() string {
	return fmt.Sprintf(
		"/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/reservations/res1",
		testSubscriptionID, testResourceGroup,
	)
}

func testVnetID() string {
	return fmt.Sprintf(
		"/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/virtualNetworks/vnet1",
		testSubscriptionID, testResourceGroup,
	)
}

func testSubnetID(name string) string {
	return testVnetID() + "/subnets/" + name
}

// newTestProviderFactories builds an isolated provider instance wired to a
// fresh in-memory fake ARM server, so each test gets its own backing store.
func newTestProviderFactories() (map[string]func() (tfprotov6.ProviderServer, error), *fakeStoragePoolStore) {
	return newTestProviderFactoriesWithTransport(nil)
}

// newTestProviderFactoriesWithTransport is newTestProviderFactories with the fake
// transport passed through wrap (when non-nil) before the provider's clients use it, so a
// test can rewrite responses the way a real service's wire format differs from the
// fake's.
func newTestProviderFactoriesWithTransport(wrap func(policy.Transporter) policy.Transporter) (map[string]func() (tfprotov6.ProviderServer, error), *fakeStoragePoolStore) {
	store := newFakeStoragePoolStore(testSubscriptionID)
	srv := store.newStoragePoolsServer()
	var transport policy.Transporter = psbfake.NewStoragePoolsServerTransport(&srv)
	if wrap != nil {
		transport = wrap(transport)
	}

	opts := &arm.ClientOptions{
		ClientOptions: azcore.ClientOptions{
			Transport: transport,
		},
	}

	p := newProvider("test", fixedCredentialFactory(&azcorefake.TokenCredential{}), opts)()

	return map[string]func() (tfprotov6.ProviderServer, error){
		"everpure": providerserver.NewProtocol6WithError(p),
	}, store
}

// newTestStoragePoolsClient builds a StoragePoolsClient wired directly to
// store's fake transport, bypassing the provider and terraform layers
// entirely. Used where a full terraform lifecycle would hit the fake SDK's
// poller-tracker limitation described above the two delete-blocked tests.
func newTestStoragePoolsClient(t *testing.T, store *fakeStoragePoolStore) *armpurestorageblock.StoragePoolsClient {
	t.Helper()

	srv := store.newStoragePoolsServer()
	transport := psbfake.NewStoragePoolsServerTransport(&srv)
	opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: transport}}

	client, err := armpurestorageblock.NewStoragePoolsClient(store.subscriptionID, &azcorefake.TokenCredential{}, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return client
}

// createPoolDirect drives a minimal, real create through client so the
// tests below have a pool to delete, without going through terraform.
func createPoolDirect(t *testing.T, ctx context.Context, client *armpurestorageblock.StoragePoolsClient, name, subnetID string) {
	t.Helper()

	subnet, err := armids.TypeSubnet.Parse(subnetID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	vnetID, err := subnet.ParentID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	poller, err := client.BeginCreate(ctx, testResourceGroup, name, armpurestorageblock.StoragePool{
		Location: to.Ptr("eastus2"),
		Properties: &armpurestorageblock.StoragePoolProperties{
			AvailabilityZone:             to.Ptr("1"),
			ProvisionedBandwidthMbPerSec: to.Ptr(int64(1024)),
			ReservationResourceID:        to.Ptr(testReservationID()),
			VnetInjection: &armpurestorageblock.VnetInjection{
				SubnetID: to.Ptr(subnetID),
				VnetID:   to.Ptr(vnetID),
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func providerBlock() string {
	return fmt.Sprintf(`
provider "everpure" {
  subscription_id = %[1]q
}
`, testSubscriptionID)
}

func TestStoragePoolResource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	t.Run("creates a pool with all attributes, deriving virtual_network_id and platform_console.subnet_ids at plan time", func(t *testing.T) {
		factories, _ := newTestProviderFactories()
		subnetID := testSubnetID("snet1")

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }

  platform_console {
    gui_enabled = true
    api_enabled = true
    cli_enabled = false
  }

  tags = {
    environment = "test"
  }
}
`, testResourceGroup, testReservationID(), subnetID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectKnownValue(
								"everpure_azure_storage_pool.test",
								tfjsonpath.New("vnet_injection").AtSliceIndex(0).AtMapKey("virtual_network_id"),
								knownvalue.StringExact(testVnetID()),
							),
							plancheck.ExpectKnownValue(
								"everpure_azure_storage_pool.test",
								tfjsonpath.New("platform_console").AtSliceIndex(0).AtMapKey("subnet_ids"),
								knownvalue.ListExact([]knownvalue.Check{knownvalue.StringExact(subnetID)}),
							),
						},
					},
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "name", "pool1"),
						resource.TestCheckResourceAttrSet("everpure_azure_storage_pool.test", "id"),
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "vnet_injection.0.virtual_network_id", testVnetID()),
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "platform_console.0.gui_enabled", "true"),
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "platform_console.0.cli_enabled", "false"),
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "platform_console.0.default_username", "pureuser"),
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "platform_console.0.subnet.0.id", subnetID),
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "tags.environment", "test"),
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "provisioned_iops", "10240"),
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "data_retention_period_in_seconds", "3600"),
					),
				},
			},
		})
	})

	t.Run("accepts a custom timeouts override alongside the resource's own attributes", func(t *testing.T) {
		factories, _ := newTestProviderFactories()
		subnetID := testSubnetID("snet1")

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }

  timeouts {
    create = "5m"
  }
}
`, testResourceGroup, testReservationID(), subnetID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "name", "pool1"),
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "timeouts.create", "5m"),
					),
				},
			},
		})
	})

	t.Run("updates bandwidth, tags and platform_console in place", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		subnetID := testSubnetID("snet1")

		base := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }

  tags = {
    environment = "test"
  }
}
`, testResourceGroup, testReservationID(), subnetID)

		updated := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 2048

  vnet_injection {
    subnet_id = %[3]q
  }

  platform_console {
    gui_enabled = true
    api_enabled = true
    cli_enabled = true
  }

  tags = {
    environment = "prod"
  }
}
`, testResourceGroup, testReservationID(), subnetID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: base},
				{
					Config: updated,
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction("everpure_azure_storage_pool.test", plancheck.ResourceActionUpdate),
						},
					},
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "provisioned_bandwidth_in_mbps", "2048"),
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "tags.environment", "prod"),
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "platform_console.0.cli_enabled", "true"),
					),
				},
			},
		})

		if store.updateCallCount != 1 {
			t.Fatalf("got %d update calls, want exactly 1 for an in-place bandwidth/tags/platform_console change", store.updateCallCount)
		}
	})

	t.Run("changing only timeouts sends no PATCH", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		subnetID := testSubnetID("snet1")

		base := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }

  tags = {
    environment = "test"
  }
}
`, testResourceGroup, testReservationID(), subnetID)

		timeoutsChanged := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }

  tags = {
    environment = "test"
  }

  timeouts {
    update = "45m"
  }
}
`, testResourceGroup, testReservationID(), subnetID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: base},
				{
					Config: timeoutsChanged,
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction("everpure_azure_storage_pool.test", plancheck.ResourceActionUpdate),
						},
					},
					Check: resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "timeouts.update", "45m"),
				},
			},
		})

		if store.updateCallCount != 0 {
			t.Fatalf("got %d update calls, want 0: a timeouts-only change is provider-only config and must send no PATCH", store.updateCallCount)
		}
	})

	// consolePoolConfig renders a pool with a platform_console block, for the tests
	// below that change one thing at a time.
	consolePoolConfig := func(cli bool, env, timeoutsBlock string) string {
		return providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }

  platform_console {
    gui_enabled = true
    api_enabled = true
    cli_enabled = %[4]t
  }

  tags = {
    environment = %[5]q
  }
%[6]s}
`, testResourceGroup, testReservationID(), testSubnetID("snet1"), cli, env, timeoutsBlock)
	}
	const poolAddr = "everpure_azure_storage_pool.test"
	consoleUsername := tfjsonpath.New("platform_console").AtSliceIndex(0).AtMapKey("default_username")
	consoleSubnet := tfjsonpath.New("platform_console").AtSliceIndex(0).AtMapKey("subnet")

	t.Run("changing only tags keeps the computed platform_console values known and leaves the console out of the PATCH", func(t *testing.T) {
		factories, store := newTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: consolePoolConfig(false, "test", "")},
				{
					Config: consolePoolConfig(false, "prod", ""),
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction(poolAddr, plancheck.ResourceActionUpdate),
							plancheck.ExpectKnownValue(poolAddr, consoleUsername, knownvalue.StringExact("pureuser")),
							plancheck.ExpectKnownValue(poolAddr, consoleSubnet, knownvalue.ListSizeExact(1)),
						},
					},
					Check: resource.TestCheckResourceAttr(poolAddr, "platform_console.0.default_username", "pureuser"),
				},
			},
		})

		if store.updateCallCount != 1 {
			t.Fatalf("got %d update calls, want 1 for a tags-only change", store.updateCallCount)
		}
		if got := store.lastUpdate.Properties.PlatformConsoleSettings; got != nil {
			t.Fatalf("got platform console settings %v in the PATCH, want none for a tags-only change", got)
		}
	})

	t.Run("changing only timeouts on a pool with platform_console sends no PATCH and keeps the console values", func(t *testing.T) {
		factories, store := newTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: consolePoolConfig(false, "test", "")},
				{
					Config: consolePoolConfig(false, "test", "\n  timeouts {\n    update = \"45m\"\n  }\n"),
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectKnownValue(poolAddr, consoleUsername, knownvalue.StringExact("pureuser")),
						},
					},
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(poolAddr, "timeouts.update", "45m"),
						resource.TestCheckResourceAttr(poolAddr, "platform_console.0.default_username", "pureuser"),
						resource.TestCheckResourceAttr(poolAddr, "platform_console.0.subnet.0.management_ip_address", "10.0.1.100"),
						resource.TestCheckResourceAttr(poolAddr, "platform_console.0.subnet.0.service_backend_ip_addresses.0", "10.0.1.101"),
						resource.TestCheckResourceAttr(poolAddr, "platform_console.0.subnet_ids.#", "1"),
					),
				},
			},
		})

		if store.updateCallCount != 0 {
			t.Fatalf("got %d update calls, want 0 for a timeouts-only change", store.updateCallCount)
		}
	})

	t.Run("flipping a platform_console setting plans the computed values as unknown and sends the new settings", func(t *testing.T) {
		factories, store := newTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: consolePoolConfig(false, "test", "")},
				{
					Config: consolePoolConfig(true, "test", ""),
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectUnknownValue(poolAddr, consoleUsername),
							plancheck.ExpectUnknownValue(poolAddr, consoleSubnet),
						},
					},
					Check: resource.TestCheckResourceAttr(poolAddr, "platform_console.0.cli_enabled", "true"),
				},
			},
		})

		if store.updateCallCount != 1 {
			t.Fatalf("got %d update calls, want 1", store.updateCallCount)
		}
		settings := store.lastUpdate.Properties.PlatformConsoleSettings
		if settings == nil {
			t.Fatal("got no platform console settings in the PATCH, want the changed ones")
		}
		if settings.Cli == nil || settings.Cli.Enabled == nil || !*settings.Cli.Enabled {
			t.Fatalf("got Cli %v, want enabled", settings.Cli)
		}
	})

	t.Run("clears out-of-band tags on update when config never declares a tags block", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		subnetID := testSubnetID("snet1")

		// No tags block at all: tags is null in config throughout, so this
		// config's plan for tags is always null, never "leave it alone".
		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }
}
`, testResourceGroup, testReservationID(), subnetID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: config},
				{
					PreConfig: func() {
						// Simulate a tag applied out-of-band (e.g. by policy),
						// alongside a change the plan does have to act on.
						store.mutatePool(testResourceGroup, "pool1", func(pool *armpurestorageblock.StoragePool) {
							pool.Tags = map[string]*string{"drift-e2e": to.Ptr("unexpected")}
							pool.Properties.ProvisionedBandwidthMbPerSec = to.Ptr(int64(1024))
						})
					},
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckNoResourceAttr("everpure_azure_storage_pool.test", "tags.drift-e2e"),
						func(*terraform.State) error {
							if store.lastUpdate == nil || store.lastUpdate.Tags == nil {
								return errors.New("expected the PATCH body to include a non-nil tags map clearing the out-of-band tag")
							}
							if len(store.lastUpdate.Tags) != 0 {
								return fmt.Errorf("got PATCH tags %v, want empty", store.lastUpdate.Tags)
							}
							return nil
						},
					),
				},
				{
					// A follow-up plan-only step: if the clear hadn't actually
					// happened, this would show a perpetual "tags -> null" diff.
					Config:   config,
					PlanOnly: true,
				},
			},
		})
	})

	t.Run("create succeeds when the RP stamps an extra tag onto the create response", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		store.stampExtraTagOnCreate = true
		subnetID := testSubnetID("snet1")

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }

  tags = {
    environment = "test"
  }
}
`, testResourceGroup, testReservationID(), subnetID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					// If Create mapped the response's tags into state instead
					// of keeping the plan's, this apply would fail with
					// "Provider produced inconsistent result after apply" as
					// soon as the fake's stamped tag showed up alongside the
					// planned one. The stamped tag genuinely exists
					// server-side, though, so the test harness's own
					// post-apply refresh plan correctly shows it as drift —
					// Read's job, not Create's.
					Config:             config,
					ExpectNonEmptyPlan: true,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "tags.%", "1"),
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "tags.environment", "test"),
					),
				},
			},
		})
	})

	t.Run("replaces the pool when zone changes", func(t *testing.T) {
		factories, _ := newTestProviderFactories()
		subnetID := testSubnetID("snet1")

		configWithZone := func(zone string) string {
			return providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = %[2]q
  reservation_id                = %[3]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[4]q
  }
}
`, testResourceGroup, zone, testReservationID(), subnetID)
		}

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: configWithZone("1")},
				{
					Config: configWithZone("2"),
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction("everpure_azure_storage_pool.test", plancheck.ResourceActionReplace),
						},
					},
				},
			},
		})
	})

	// name is deliberately excluded from armids.IDType: the API has no rename, so a
	// case-only edit can only mean destroy-and-recreate, and that must stay visible
	// as a diff rather than being swallowed by case-insensitive comparison.
	t.Run("replaces the pool when only name's letter case changes", func(t *testing.T) {
		factories, _ := newTestProviderFactories()
		subnetID := testSubnetID("snet1")

		configWithName := func(name string) string {
			return providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = %[1]q
  resource_group_name           = %[2]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[3]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[4]q
  }
}
`, name, testResourceGroup, testReservationID(), subnetID)
		}

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: configWithName("pool1")},
				{
					Config: configWithName("POOL1"),
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction("everpure_azure_storage_pool.test", plancheck.ResourceActionReplace),
						},
					},
				},
			},
		})
	})

	t.Run("accepts an empty zone, for a region with no availability zones", func(t *testing.T) {
		factories, _ := newTestProviderFactories()
		subnetID := testSubnetID("snet1")

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = ""
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }
}
`, testResourceGroup, testReservationID(), subnetID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check:  resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "zone", ""),
				},
			},
		})
	})

	t.Run("replaces the pool when vnet_injection.subnet_id changes", func(t *testing.T) {
		factories, _ := newTestProviderFactories()

		configWithSubnet := func(subnetID string) string {
			return providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }
}
`, testResourceGroup, testReservationID(), subnetID)
		}

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: configWithSubnet(testSubnetID("snet1"))},
				{
					Config: configWithSubnet(testSubnetID("snet2")),
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction("everpure_azure_storage_pool.test", plancheck.ResourceActionReplace),
						},
					},
				},
			},
		})
	})

	t.Run("PATCHes platform console disabled with all services off when the block is removed", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		subnetID := testSubnetID("snet1")

		withConsole := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }

  platform_console {
    gui_enabled = true
    api_enabled = true
    cli_enabled = true
  }
}
`, testResourceGroup, testReservationID(), subnetID)

		withoutConsole := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }
}
`, testResourceGroup, testReservationID(), subnetID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: withConsole},
				{Config: withoutConsole},
			},
		})

		if store.lastUpdate == nil {
			t.Fatal("expected a captured update")
		}
		if store.lastUpdate.Properties == nil {
			t.Fatal("expected update Properties to be set")
		}
		settings := store.lastUpdate.Properties.PlatformConsoleSettings
		if settings == nil {
			t.Fatal("expected PlatformConsoleSettings to be set")
		}
		if settings.Enabled == nil || *settings.Enabled {
			t.Fatalf("Enabled = %v, want false", settings.Enabled)
		}
		if settings.Gui == nil || settings.Gui.Enabled == nil || *settings.Gui.Enabled {
			t.Fatal("expected Gui.Enabled to be false")
		}
		if settings.API == nil || settings.API.Enabled == nil || *settings.API.Enabled {
			t.Fatal("expected API.Enabled to be false")
		}
		if settings.Cli == nil || settings.Cli.Enabled == nil || *settings.Cli.Enabled {
			t.Fatal("expected Cli.Enabled to be false")
		}
		if len(settings.Subnets) != 1 {
			t.Fatalf("got %d subnets, want 1", len(settings.Subnets))
		}
		if settings.Subnets[0].ID == nil || *settings.Subnets[0].ID != subnetID {
			t.Fatalf("subnet ID = %v, want %q", settings.Subnets[0].ID, subnetID)
		}
	})

	t.Run("shows drift and plans an update when tags or bandwidth change out of band", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		subnetID := testSubnetID("snet1")

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }
}
`, testResourceGroup, testReservationID(), subnetID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: config},
				{
					PreConfig: func() {
						store.mutatePool(testResourceGroup, "pool1", func(pool *armpurestorageblock.StoragePool) {
							pool.Properties.ProvisionedBandwidthMbPerSec = to.Ptr(int64(4096))
						})
					},
					Config:             config,
					PlanOnly:           true,
					ExpectNonEmptyPlan: true,
				},
			},
		})
	})

	t.Run("plans a recreate when the pool has been deleted out of band", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		subnetID := testSubnetID("snet1")

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }
}
`, testResourceGroup, testReservationID(), subnetID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: config},
				{
					PreConfig: func() {
						store.removePool(testResourceGroup, "pool1")
					},
					Config:             config,
					PlanOnly:           true,
					ExpectNonEmptyPlan: true,
				},
			},
		})
	})

	t.Run("imports a pool by its full ARM ID", func(t *testing.T) {
		factories, _ := newTestProviderFactories()
		subnetID := testSubnetID("snet1")

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }
}
`, testResourceGroup, testReservationID(), subnetID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: config},
				{
					Config:            config,
					ResourceName:      "everpure_azure_storage_pool.test",
					ImportState:       true,
					ImportStateVerify: true,
					ImportStateId: fmt.Sprintf(
						"/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/storagePools/pool1",
						testSubscriptionID, testResourceGroup,
					),
				},
			},
		})
	})

	// Import-shaped: reservation_id and vnet_injection never round-trip through the
	// import ID itself, but through the API response — so a pool that already exists
	// with one casing (as if created outside Terraform) and a config that spells the
	// same IDs with different, but ARM-equivalent, keyword casing must still plan no
	// changes after import. Only an import-shaped test catches a missing plan
	// modifier here: an apply-then-plan test can't, because by then state already
	// holds the config's own spelling.
	t.Run("an imported pool plans no replace when config spells reservation_id and subnet_id with different casing", func(t *testing.T) {
		factories, store := newTestProviderFactories()

		canonicalSubnetID := testSubnetID("snet1")

		// As if the pool had been created outside Terraform (or by an earlier apply
		// in a different working directory), already carrying ARM's own casing.
		store.mu.Lock()
		store.pools[poolKey(testResourceGroup, "pool1")] = &armpurestorageblock.StoragePool{
			ID:       to.Ptr(store.fakePoolID(testResourceGroup, "pool1")),
			Name:     to.Ptr("pool1"),
			Location: to.Ptr("eastus2"),
			Properties: &armpurestorageblock.StoragePoolProperties{
				AvailabilityZone:             to.Ptr("1"),
				ProvisionedBandwidthMbPerSec: to.Ptr(int64(1024)),
				ProvisionedIops:              to.Ptr(int64(10240)),
				ReservationResourceID:        to.Ptr(testReservationID()),
				DataRetentionPeriod:          to.Ptr(int64(3600)),
				StoragePoolInternalID:        to.Ptr("internal-pool1"),
				ProvisioningState:            to.Ptr(armpurestorageblock.ProvisioningStateSucceeded),
				VnetInjection: &armpurestorageblock.VnetInjection{
					SubnetID: to.Ptr(canonicalSubnetID),
					VnetID:   to.Ptr(testVnetID()),
				},
			},
		}
		store.mu.Unlock()

		// The practitioner's own config spells the same two IDs with different, but
		// ARM-equivalent, keyword casing.
		mangledReservationID := fmt.Sprintf(
			"/Subscriptions/%s/ResourceGroups/%s/providers/purestorage.block/Reservations/res1",
			testSubscriptionID, testResourceGroup,
		)
		mangledSubnetID := fmt.Sprintf(
			"/Subscriptions/%s/ResourceGroups/%s/providers/Microsoft.Network/VirtualNetworks/vnet1/Subnets/snet1",
			testSubscriptionID, testResourceGroup,
		)

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }
}
`, testResourceGroup, mangledReservationID, mangledSubnetID)

		// The import ID's own keyword segments are non-canonically cased too, as a
		// practitioner composing one by hand from the same config would naturally do.
		mangledPoolID := fmt.Sprintf(
			"/Subscriptions/%s/ResourceGroups/%s/providers/purestorage.block/StoragePools/pool1",
			testSubscriptionID, testResourceGroup,
		)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config:             config,
					ResourceName:       "everpure_azure_storage_pool.test",
					ImportState:        true,
					ImportStatePersist: true,
					ImportStateVerify:  false,
					ImportStateId:      mangledPoolID,
				},
				{
					// No update and no replace: the IDs resolve back to the imported (ARM)
					// casing, and provisioned_iops, which the framework marks unknown
					// ahead of the ID modifiers, is restored to state with them.
					Config: config,
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction("everpure_azure_storage_pool.test", plancheck.ResourceActionNoop),
							plancheck.ExpectKnownValue(
								"everpure_azure_storage_pool.test", tfjsonpath.New("reservation_id"),
								knownvalue.StringExact(testReservationID()),
							),
							plancheck.ExpectKnownValue(
								"everpure_azure_storage_pool.test",
								tfjsonpath.New("vnet_injection").AtSliceIndex(0).AtMapKey("subnet_id"),
								knownvalue.StringExact(canonicalSubnetID),
							),
							plancheck.ExpectKnownValue(
								"everpure_azure_storage_pool.test",
								tfjsonpath.New("vnet_injection").AtSliceIndex(0).AtMapKey("virtual_network_id"),
								knownvalue.StringExact(testVnetID()),
							),
						},
					},
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "reservation_id", testReservationID()),
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "vnet_injection.0.subnet_id", canonicalSubnetID),
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "vnet_injection.0.virtual_network_id", testVnetID()),
					),
				},
			},
		})
	})

	t.Run("rejects an import ID from a different subscription", func(t *testing.T) {
		factories, _ := newTestProviderFactories()
		subnetID := testSubnetID("snet1")

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }
}
`, testResourceGroup, testReservationID(), subnetID)

		const otherSubscriptionPoolID = "/subscriptions/22222222-2222-2222-2222-222222222222/resourceGroups/rg-test/providers/PureStorage.Block/storagePools/pool1"
		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: config},
				{
					Config:        config,
					ResourceName:  "everpure_azure_storage_pool.test",
					ImportState:   true,
					ImportStateId: otherSubscriptionPoolID,
					ExpectError:   subscriptionMismatchRegexp(kindStoragePool, otherSubscriptionPoolID),
				},
			},
		})
	})

	t.Run("rejects invalid configuration at plan time", func(t *testing.T) {
		tests := []struct {
			name          string
			resourceBlock string
			wantErr       *regexp.Regexp
		}{
			{
				name: "two vnet_injection blocks",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection { subnet_id = %[3]q }
  vnet_injection { subnet_id = %[4]q }
}
`, testResourceGroup, testReservationID(), testSubnetID("snet1"), testSubnetID("snet2")),
				wantErr: regexp.MustCompile("at most 1 elements"),
			},
			{
				name: "gui_enabled != api_enabled",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection { subnet_id = %[3]q }

  platform_console {
    gui_enabled = true
    api_enabled = false
  }
}
`, testResourceGroup, testReservationID(), testSubnetID("snet1")),
				wantErr: diagRegexp(diag.WithPath(path.Root("platform_console").AtListIndex(0).AtName("gui_enabled"), invalidAttributeCombinationDiagnostic(newConsoleGUIAPIMismatchError()))),
			},
			{
				name: "all platform console services false",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection { subnet_id = %[3]q }

  platform_console {
    gui_enabled = false
    api_enabled = false
    cli_enabled = false
  }
}
`, testResourceGroup, testReservationID(), testSubnetID("snet1")),
				wantErr: diagRegexp(diag.WithPath(path.Root("platform_console").AtListIndex(0).AtName("cli_enabled"), invalidAttributeCombinationDiagnostic(newConsoleServiceRequiredError()))),
			},
			{
				name: "non-canonical location",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "East US 2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection { subnet_id = %[3]q }
}
`, testResourceGroup, testReservationID(), testSubnetID("snet1")),
				wantErr: regexp.MustCompile(wordsRegexp(locationRule)),
			},
			{
				name: "malformed reservation_id",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = "not-a-resource-id"
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection { subnet_id = %[2]q }
}
`, testResourceGroup, testSubnetID("snet1")),
				wantErr: regexp.MustCompile("PureStorage.Block/reservations"),
			},
			{
				name: "malformed timeouts.create duration",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection { subnet_id = %[3]q }

  timeouts {
    create = "not-a-duration"
  }
}
`, testResourceGroup, testReservationID(), testSubnetID("snet1")),
				wantErr: regexp.MustCompile("Invalid Attribute Value Time Duration"),
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				factories, _ := newTestProviderFactories()
				resource.Test(t, resource.TestCase{
					IsUnitTest:               true,
					ProtoV6ProviderFactories: factories,
					Steps: []resource.TestStep{
						{
							Config:      providerBlock() + tt.resourceBlock,
							ExpectError: tt.wantErr,
						},
					},
				})
			})
		}
	})
}

// TestStoragePoolResourceDynamicPlatformConsoleBlock is a regression test for a
// dynamic block whose for_each derives from a Terraform variable rather than a
// literal list: Terraform core then reports the whole platform_console list as
// unknown while validating the config, even before the for_each value itself has
// been resolved (here, a boolean var feeding a conditional expression). That
// crashed every plan and validate against this pattern (see
// blockElements in block_elements.go) until platform_console became a
// types.List, which can represent "unknown". vnet_injection had the same latent
// bug and gets its own dynamic-block subtest below — it is the riskier of the
// two, since ModifyPlan derives virtual_network_id from it and must skip that
// derivation while the list is unknown. Every other test config in this file uses
// a literal block, so none of them would have caught this. resource.Test always
// plans before it applies, so a config that merely applies successfully here is
// already proof plan didn't crash first.
func TestStoragePoolResourceDynamicPlatformConsoleBlock(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	subnetID := testSubnetID("snet1")
	cfg := providerBlock() + fmt.Sprintf(`
variable "enable_console" {
  type    = bool
  default = false
}

resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }

  dynamic "platform_console" {
    for_each = var.enable_console ? [1] : []
    content {
      gui_enabled = true
      api_enabled = true
      cli_enabled = false
    }
  }
}
`, testResourceGroup, testReservationID(), subnetID)

	t.Run("for_each resolves to one element: plan and apply succeed, the block lands in state", func(t *testing.T) {
		factories, _ := newTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: cfg,
					ConfigVariables: config.Variables{
						"enable_console": config.BoolVariable(true),
					},
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "platform_console.#", "1"),
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "platform_console.0.gui_enabled", "true"),
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "vnet_injection.0.virtual_network_id", testVnetID()),
					),
				},
			},
		})
	})

	t.Run("for_each resolves to an empty list (the variable's default): plan and apply succeed with no block", func(t *testing.T) {
		factories, _ := newTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: cfg,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "platform_console.#", "0"),
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "vnet_injection.0.virtual_network_id", testVnetID()),
					),
				},
			},
		})
	})

	// vnet_injection is the block ModifyPlan derives virtual_network_id from, so a
	// variable-driven dynamic block here exercises both the unknown-list decoding and
	// the derivation being skipped at plan time and completed at apply time.
	t.Run("dynamic vnet_injection with variable-derived for_each: plan and apply succeed, virtual_network_id derived", func(t *testing.T) {
		factories, _ := newTestProviderFactories()

		dynamicVnetCfg := providerBlock() + fmt.Sprintf(`
variable "subnet_ids" {
  type = list(string)
}

resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  dynamic "vnet_injection" {
    for_each = var.subnet_ids
    content {
      subnet_id = vnet_injection.value
    }
  }
}
`, testResourceGroup, testReservationID())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: dynamicVnetCfg,
					ConfigVariables: config.Variables{
						"subnet_ids": config.ListVariable(config.StringVariable(subnetID)),
					},
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "vnet_injection.#", "1"),
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "vnet_injection.0.subnet_id", subnetID),
						resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "vnet_injection.0.virtual_network_id", testVnetID()),
					),
				},
			},
		})
	})
}

// TestStoragePoolDeleteBlocked drives the fake StoragePoolsClient's
// BeginDelete/PollUntilDone directly instead of through a full terraform
// apply/destroy lifecycle. Reason: armpurestorageblock/fake's poller
// tracker is keyed only by URL path and never cleared after a terminal
// *error* response (confirmed by reading fake/storagepools_server.go and
// fake/internal.go's tracker), so a second real DELETE call to the same
// pool — exactly what terraform-plugin-testing's automatic post-test
// destroy would issue after an expected-failing destroy step — hits the
// exhausted poller and fails with an unrelated "fake poller response has
// nothing queued" error. Driving BeginDelete/PollUntilDone directly still
// exercises the real production classifyStoragePoolDelete path against a real
// SDK poller and a real fake ARM round trip; it just performs the single
// delete call the scenario needs, once.
func TestStoragePoolDeleteBlocked(t *testing.T) {
	t.Run("surfaces the live-children hint when delete is blocked synchronously", func(t *testing.T) {
		ctx := context.Background()
		store := newFakeStoragePoolStore(testSubscriptionID)
		client := newTestStoragePoolsClient(t, store)
		subnetID := testSubnetID("snet1")

		createPoolDirect(t, ctx, client, "pool1", subnetID)
		store.blockedSync[poolKey(testResourceGroup, "pool1")] = true

		poller, err := client.BeginDelete(ctx, testResourceGroup, "pool1", nil)
		if err == nil {
			_, err = poller.PollUntilDone(ctx, nil)
		}
		if err == nil {
			t.Fatal("expected an error, got nil")
		}

		if got := classifyStoragePoolDelete(err); got != storagePoolDeleteHasChildren {
			t.Fatalf("classifyStoragePoolDelete() = %v, want %v", got, storagePoolDeleteHasChildren)
		}
		f := (&Clients{correlationID: "corr-1"}).apiFailure(err)
		got := newStoragePoolHasChildrenError("pool1", f)
		requireContains(t, got.Error(), "pool1")
		requireContains(t, got.Error(), f.response)
		requireContains(t, got.Error(), "x-ms-correlation-request-id: corr-1")
	})

	t.Run("surfaces the soft-delete hint when delete is blocked by a failed LRO", func(t *testing.T) {
		ctx := context.Background()
		store := newFakeStoragePoolStore(testSubscriptionID)
		client := newTestStoragePoolsClient(t, store)
		subnetID := testSubnetID("snet1")

		createPoolDirect(t, ctx, client, "pool1", subnetID)
		store.blockedLRO[poolKey(testResourceGroup, "pool1")] = true

		poller, err := client.BeginDelete(ctx, testResourceGroup, "pool1", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_, err = poller.PollUntilDone(ctx, nil)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}

		if got := classifyStoragePoolDelete(err); got != storagePoolDeleteContainsData {
			t.Fatalf("classifyStoragePoolDelete() = %v, want %v", got, storagePoolDeleteContainsData)
		}
		f := (&Clients{correlationID: "corr-1"}).apiFailure(err)
		got := newStoragePoolContainsDataError("pool1", 3600, f)
		requireContains(t, got.Error(), "pool1")
		requireContains(t, got.Error(), "3600 seconds")
		requireContains(t, got.Error(), f.response)
	})
}

// TestStoragePoolUpdateNotFound drives StoragePoolResource.Update directly
// (bypassing Terraform's own refresh-before-apply, which would normally
// catch an out-of-band delete during Read and plan a recreate instead of an
// update) to exercise the race where the pool is deleted between the
// refresh that produced the plan and the PATCH that applies it.
func TestStoragePoolUpdateNotFound(t *testing.T) {
	ctx := context.Background()
	store := newFakeStoragePoolStore(testSubscriptionID)
	client := newTestStoragePoolsClient(t, store)
	subnetID := testSubnetID("snet1")

	createPoolDirect(t, ctx, client, "pool1", subnetID)
	store.removePool(testResourceGroup, "pool1")

	srv := store.newStoragePoolsServer()
	transport := psbfake.NewStoragePoolsServerTransport(&srv)
	opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: transport}}
	r := &StoragePoolResource{}
	var configureResp fwresource.ConfigureResponse
	r.Configure(ctx, fwresource.ConfigureRequest{ProviderData: &Clients{
		cred:           &azcorefake.TokenCredential{},
		opts:           opts,
		subscriptionID: store.subscriptionID,
	}}, &configureResp)
	if configureResp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", configureResp.Diagnostics)
	}

	var schemaResp fwresource.SchemaResponse
	r.Schema(ctx, fwresource.SchemaRequest{}, &schemaResp)

	subnet, err := armids.TypeSubnet.Parse(subnetID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	vnetID, err := subnet.ParentID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	vnetInjection, diags := types.ListValueFrom(ctx, vnetInjectionObjectType, []VnetInjectionModel{{
		SubnetID:         armids.IDValue{StringValue: types.StringValue(subnetID)},
		VirtualNetworkID: armids.IDValue{StringValue: types.StringValue(vnetID)},
	}})
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	planData := StoragePoolResourceModel{
		ID:                           armids.IDValue{StringValue: types.StringValue(store.fakePoolID(testResourceGroup, "pool1"))},
		Name:                         types.StringValue("pool1"),
		ResourceGroupName:            types.StringValue(testResourceGroup),
		Location:                     types.StringValue("eastus2"),
		Zone:                         types.StringValue("1"),
		ReservationID:                armids.IDValue{StringValue: types.StringValue(testReservationID())},
		ProvisionedBandwidthInMbps:   types.Int64Value(2048),
		Tags:                         types.MapNull(types.StringType),
		InternalID:                   types.StringValue("internal-pool1"),
		ProvisionedIops:              types.Int64Value(10240),
		DataRetentionPeriodInSeconds: types.Int64Value(3600),
		VnetInjection:                vnetInjection,
		PlatformConsole:              types.ListNull(platformConsoleObjectType),
		Timeouts: timeouts.Value{Object: types.ObjectNull(map[string]attr.Type{
			"create": types.StringType,
			"read":   types.StringType,
			"update": types.StringType,
			"delete": types.StringType,
		})},
	}

	plan := tfsdk.Plan{Schema: schemaResp.Schema}
	if diags := plan.Set(ctx, &planData); diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}

	// The prior state, as it was actually created (bandwidth 1024, before the
	// plan above raised it to 2048): Update now reads req.State to decide
	// whether it can skip the PATCH, so it must be populated here just like
	// the real protocol always populates it from the prior apply.
	stateData := planData
	stateData.ProvisionedBandwidthInMbps = types.Int64Value(1024)
	stateData.ProvisionedIops = types.Int64Value(10240)
	state := tfsdk.State{Schema: schemaResp.Schema}
	if diags := state.Set(ctx, &stateData); diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}

	updateResp := &fwresource.UpdateResponse{State: tfsdk.State{Schema: schemaResp.Schema, Raw: plan.Raw}}
	r.Update(ctx, fwresource.UpdateRequest{Plan: plan, State: state}, updateResp)

	// Update must not remove the resource from state on a 404: that would make
	// Terraform Core itself report a provider-bug-shaped "Provider produced
	// inconsistent result after apply" instead of an explanation, since Update
	// isn't allowed to remove the resource from state. It reports an explicit
	// error naming the real cause instead.
	if !updateResp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic")
	}
	requireContains(t, updateResp.Diagnostics[0].Detail(), "deleted outside Terraform")
}

// TestStoragePoolImportStateNilClients exercises ImportState before Configure has
// ever run — e.g. an import block whose provider configuration itself isn't known
// yet (subscription_id or an authentication attribute still unresolved) — where
// r.clients is nil. Reading r.clients.subscriptionID unguarded would panic instead of
// returning a diagnostic.
func TestStoragePoolImportStateNilClients(t *testing.T) {
	r := &StoragePoolResource{}
	resp := &fwresource.ImportStateResponse{}

	r.ImportState(context.Background(), fwresource.ImportStateRequest{
		ID: "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1",
	}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic instead of a panic")
	}
	requireContains(t, resp.Diagnostics[0].Detail(), "provider configuration contains values that are not yet known")
}

// TestStoragePoolResourceIDCasingPreserved covers what armids.IDType exists for on
// this resource: ARM may echo a resource ID back cased differently from the way the
// practitioner wrote it, and that must not read as drift. The fake upper-cases every
// ID it returns here, so a byte-for-byte comparison would plan a change to
// reservation_id and vnet_injection.subnet_id and show virtual_network_id moving.
// It also covers the two shapes worth proving separately, since neither is obvious
// from the framework's documentation: a Computed attribute (virtual_network_id) and
// attributes nested inside a list block (vnet_injection).
func TestStoragePoolResourceIDCasingPreserved(t *testing.T) {
	factories, store := newTestProviderFactories()

	config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }
}
`, testResourceGroup, testReservationID(), testSubnetID("snet1"))

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					// Make the stored pool echo its IDs back upper-cased, as ARM can.
					store.mutatePool(testResourceGroup, "pool1", func(p *armpurestorageblock.StoragePool) {
						if p.Properties == nil {
							return
						}
						if p.Properties.ReservationResourceID != nil {
							p.Properties.ReservationResourceID = to.Ptr(strings.ToUpper(*p.Properties.ReservationResourceID))
						}
						if p.Properties.VnetInjection != nil {
							if p.Properties.VnetInjection.SubnetID != nil {
								p.Properties.VnetInjection.SubnetID = to.Ptr(strings.ToUpper(*p.Properties.VnetInjection.SubnetID))
							}
							if p.Properties.VnetInjection.VnetID != nil {
								p.Properties.VnetInjection.VnetID = to.Ptr(strings.ToUpper(*p.Properties.VnetInjection.VnetID))
							}
						}
					})
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: false,
			},
		},
	})
}

// TestStoragePoolResourceConfigCasingChange is TestStoragePoolResourceIDCasingPreserved
// from the other side: the practitioner respells a value in config while the API keeps
// its own. A case-only edit must plan nothing, neither a replace nor an in-place update
// left behind by a Computed attribute the edit made unknown.
func TestStoragePoolResourceConfigCasingChange(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	configWith := func(rg, reservationID, subnetID string) string {
		return providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }
}
`, rg, reservationID, subnetID)
	}
	orig := configWith(testResourceGroup, testReservationID(), testSubnetID("snet1"))

	for name, cfg := range map[string]string{
		"resource group": configWith(strings.ToUpper(testResourceGroup), testReservationID(), testSubnetID("snet1")),
		"reservation id": configWith(testResourceGroup, strings.ToUpper(testReservationID()), testSubnetID("snet1")),
		"subnet id":      configWith(testResourceGroup, testReservationID(), strings.ToUpper(testSubnetID("snet1"))),
	} {
		t.Run(name, func(t *testing.T) {
			factories, _ := newTestProviderFactories()
			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: orig},
					{Config: cfg, PlanOnly: true},
				},
			})
		})
	}
}
