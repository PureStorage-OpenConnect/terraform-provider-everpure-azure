// SPDX-License-Identifier: Apache-2.0

package provider

// everpure_azure_storage_pool has no input blocks, so the dynamic-block-unknown-list
// regression covered by TestStoragePoolResourceDynamicPlatformConsoleBlock (see
// storage_pool_integration_test.go) does not apply here: nothing in this data source's
// schema is a block a config could wrap in a dynamic block. The storage pool is also the
// one of the three data sources with no parent ID to validate — it is looked up by
// resource_group_name alone, already scoped to the provider's own subscription — so,
// unlike the volume group and volume data sources, there is no "parent in another
// subscription" case here.

import (
	"fmt"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-testing/compare"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

func TestStoragePoolDataSource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	t.Run("looks up a pool managed by a resource in the same configuration", func(t *testing.T) {
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

data "everpure_azure_storage_pool" "test" {
  name                = everpure_azure_storage_pool.test.name
  resource_group_name = everpure_azure_storage_pool.test.resource_group_name
}
`, testResourceGroup, testReservationID(), subnetID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					ConfigStateChecks: []statecheck.StateCheck{
						statecheck.CompareValuePairs(
							"everpure_azure_storage_pool.test", tfjsonpath.New("id"),
							"data.everpure_azure_storage_pool.test", tfjsonpath.New("id"),
							compare.ValuesSame(),
						),
						statecheck.CompareValuePairs(
							"everpure_azure_storage_pool.test", tfjsonpath.New("location"),
							"data.everpure_azure_storage_pool.test", tfjsonpath.New("location"),
							compare.ValuesSame(),
						),
						statecheck.CompareValuePairs(
							"everpure_azure_storage_pool.test", tfjsonpath.New("zone"),
							"data.everpure_azure_storage_pool.test", tfjsonpath.New("zone"),
							compare.ValuesSame(),
						),
						statecheck.CompareValuePairs(
							"everpure_azure_storage_pool.test", tfjsonpath.New("reservation_id"),
							"data.everpure_azure_storage_pool.test", tfjsonpath.New("reservation_id"),
							compare.ValuesSame(),
						),
						statecheck.CompareValuePairs(
							"everpure_azure_storage_pool.test", tfjsonpath.New("provisioned_bandwidth_in_mbps"),
							"data.everpure_azure_storage_pool.test", tfjsonpath.New("provisioned_bandwidth_in_mbps"),
							compare.ValuesSame(),
						),
						statecheck.CompareValuePairs(
							"everpure_azure_storage_pool.test", tfjsonpath.New("provisioned_iops"),
							"data.everpure_azure_storage_pool.test", tfjsonpath.New("provisioned_iops"),
							compare.ValuesSame(),
						),
						statecheck.CompareValuePairs(
							"everpure_azure_storage_pool.test", tfjsonpath.New("data_retention_period_in_seconds"),
							"data.everpure_azure_storage_pool.test", tfjsonpath.New("data_retention_period_in_seconds"),
							compare.ValuesSame(),
						),
						statecheck.CompareValuePairs(
							"everpure_azure_storage_pool.test", tfjsonpath.New("internal_id"),
							"data.everpure_azure_storage_pool.test", tfjsonpath.New("internal_id"),
							compare.ValuesSame(),
						),
						statecheck.CompareValuePairs(
							"everpure_azure_storage_pool.test", tfjsonpath.New("tags"),
							"data.everpure_azure_storage_pool.test", tfjsonpath.New("tags"),
							compare.ValuesSame(),
						),
						statecheck.CompareValuePairs(
							"everpure_azure_storage_pool.test", tfjsonpath.New("vnet_injection"),
							"data.everpure_azure_storage_pool.test", tfjsonpath.New("vnet_injection"),
							compare.ValuesSame(),
						),
						statecheck.CompareValuePairs(
							"everpure_azure_storage_pool.test", tfjsonpath.New("platform_console"),
							"data.everpure_azure_storage_pool.test", tfjsonpath.New("platform_console"),
							compare.ValuesSame(),
						),
					},
				},
			},
		})
	})

	t.Run("looks up a pool seeded directly (not managed by any resource in this configuration)", func(t *testing.T) {
		factories, store := newTestProviderFactories()

		subnetID := testSubnetID("snet1")
		store.mu.Lock()
		store.pools[poolKey(testResourceGroup, "seeded-pool")] = &armpurestorageblock.StoragePool{
			ID:       to.Ptr(store.fakePoolID(testResourceGroup, "seeded-pool")),
			Name:     to.Ptr("seeded-pool"),
			Location: to.Ptr("eastus2"),
			Properties: &armpurestorageblock.StoragePoolProperties{
				AvailabilityZone:             to.Ptr("2"),
				ProvisionedBandwidthMbPerSec: to.Ptr(int64(2048)),
				ProvisionedIops:              to.Ptr(int64(20480)),
				ReservationResourceID:        to.Ptr(testReservationID()),
				DataRetentionPeriod:          to.Ptr(int64(7200)),
				StoragePoolInternalID:        to.Ptr("internal-seeded-pool"),
				// The API requires vnetInjection on every real pool, so a
				// realistic fixture always carries one; only platform_console
				// varies between "with" and "without" in these tests.
				VnetInjection: &armpurestorageblock.VnetInjection{
					SubnetID: to.Ptr(subnetID),
					VnetID:   to.Ptr(testVnetID()),
				},
			},
		}
		store.mu.Unlock()

		config := providerBlock() + fmt.Sprintf(`
data "everpure_azure_storage_pool" "test" {
  name                = "seeded-pool"
  resource_group_name = %[1]q
}
`, testResourceGroup)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pool.test", "zone", "2"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pool.test", "provisioned_bandwidth_in_mbps", "2048"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pool.test", "internal_id", "internal-seeded-pool"),
						resource.TestCheckNoResourceAttr("data.everpure_azure_storage_pool.test", "tags.%"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pool.test", "vnet_injection.#", "1"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pool.test", "vnet_injection.0.subnet_id", subnetID),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pool.test", "platform_console.#", "0"),
					),
				},
			},
		})
	})

	t.Run("returns tags as null when the pool has none, and as a map when it does", func(t *testing.T) {
		factories, store := newTestProviderFactories()

		seed := func(name string, tags map[string]*string) {
			store.mu.Lock()
			defer store.mu.Unlock()
			store.pools[poolKey(testResourceGroup, name)] = &armpurestorageblock.StoragePool{
				ID:       to.Ptr(store.fakePoolID(testResourceGroup, name)),
				Name:     to.Ptr(name),
				Location: to.Ptr("eastus2"),
				Tags:     tags,
				Properties: &armpurestorageblock.StoragePoolProperties{
					AvailabilityZone:             to.Ptr("1"),
					ProvisionedBandwidthMbPerSec: to.Ptr(int64(1024)),
					ReservationResourceID:        to.Ptr(testReservationID()),
					VnetInjection: &armpurestorageblock.VnetInjection{
						SubnetID: to.Ptr(testSubnetID("snet1")),
						VnetID:   to.Ptr(testVnetID()),
					},
				},
			}
		}
		seed("no-tags-pool", nil)
		seed("tagged-pool", map[string]*string{"environment": to.Ptr("prod")})

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_storage_pool" "no_tags" {
  name                = "no-tags-pool"
  resource_group_name = %[1]q
}

data "everpure_azure_storage_pool" "tagged" {
  name                = "tagged-pool"
  resource_group_name = %[1]q
}
`, testResourceGroup),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckNoResourceAttr("data.everpure_azure_storage_pool.no_tags", "tags.%"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pool.tagged", "tags.%", "1"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pool.tagged", "tags.environment", "prod"),
					),
				},
			},
		})
	})

	t.Run("errors when no pool matches the name", func(t *testing.T) {
		factories, _ := newTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_storage_pool" "test" {
  name                = "does-not-exist"
  resource_group_name = %[1]q
}
`, testResourceGroup),
					ExpectError: diagRegexp(operation{action: actionRetrieve, kind: kindStoragePool, name: "does-not-exist"}.fail(newNotFoundError(kindStoragePool, armids.StoragePoolID{
						ResourceGroupID: armids.ResourceGroupID{SubscriptionID: testSubscriptionID, ResourceGroupName: testResourceGroup},
						StoragePoolName: "does-not-exist",
					}.FullID(), &kindResourceGroup, apiFailure{}))),
				},
			},
		})
	})

	// The fake's Get now folds name casing case-insensitively (mirroring ARM's own
	// behavior for storage pools), so this config's differently-cased name still finds
	// the seeded pool below. What this test actually exists to prove is what happens
	// next: applyStoragePool maps Name from the API response (the pool's own stored
	// casing), so without the data source restoring it to what was configured, this
	// apply would fail with "Provider produced invalid object" (name is Required, not
	// Computed) the moment the two disagreed.
	t.Run("succeeds when the configured name differs in case from the pool's own, and keeps the configured spelling", func(t *testing.T) {
		factories, store := newTestProviderFactories()

		store.mu.Lock()
		store.pools[poolKey(testResourceGroup, "cased-pool")] = &armpurestorageblock.StoragePool{
			ID:       to.Ptr(store.fakePoolID(testResourceGroup, "cased-pool")),
			Name:     to.Ptr("cased-pool"),
			Location: to.Ptr("eastus2"),
			Properties: &armpurestorageblock.StoragePoolProperties{
				AvailabilityZone:             to.Ptr("1"),
				ProvisionedBandwidthMbPerSec: to.Ptr(int64(1024)),
				ReservationResourceID:        to.Ptr(testReservationID()),
				VnetInjection: &armpurestorageblock.VnetInjection{
					SubnetID: to.Ptr(testSubnetID("snet1")),
					VnetID:   to.Ptr(testVnetID()),
				},
			},
		}
		store.mu.Unlock()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_storage_pool" "test" {
  name                = "CASED-POOL"
  resource_group_name = %[1]q
}
`, testResourceGroup),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pool.test", "name", "CASED-POOL"),
						resource.TestCheckResourceAttrSet("data.everpure_azure_storage_pool.test", "id"),
					),
				},
			},
		})
	})
}
