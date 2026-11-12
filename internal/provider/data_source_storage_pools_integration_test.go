// SPDX-License-Identifier: Apache-2.0

package provider

// everpure_azure_storage_pools has no input blocks, so the dynamic-block-unknown-list
// regression covered by TestStoragePoolResourceDynamicPlatformConsoleBlock (see
// storage_pool_integration_test.go) does not apply here. It is also the one plural data
// source with no parent ID to validate — pools are scoped by an optional
// resource_group_name (or the whole subscription) rather than a parent resource — so,
// unlike the other three, there is no "parent in another subscription" or "missing
// parent" case here: an empty or nonexistent resource group is just an empty list.

import (
	"fmt"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func seedTestPool(store *fakeStoragePoolStore, resourceGroupName, name string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.pools[poolKey(resourceGroupName, name)] = &armpurestorageblock.StoragePool{
		ID:       to.Ptr(store.fakePoolID(resourceGroupName, name)),
		Name:     to.Ptr(name),
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
}

func TestStoragePoolsDataSource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	t.Run("an empty resource group gives an empty list", func(t *testing.T) {
		factories, _ := newTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_storage_pools" "test" {
  resource_group_name = %[1]q
}
`, testResourceGroup),
					Check: resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.test", "storage_pools.#", "0"),
				},
			},
		})
	})

	t.Run("an empty subscription (resource_group_name omitted) gives an empty list", func(t *testing.T) {
		factories, _ := newTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + `
data "everpure_azure_storage_pools" "test" {}
`,
					Check: resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.test", "storage_pools.#", "0"),
				},
			},
		})
	})

	t.Run("keeps a pool with no properties, and drops one with no id, without failing the list", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		seedTestPool(store, testResourceGroup, "pool-a")
		seedTestPool(store, testResourceGroup, "pool-b")
		seedTestPool(store, testResourceGroup, "pool-c")
		store.mutatePool(testResourceGroup, "pool-b", func(p *armpurestorageblock.StoragePool) { p.Properties = nil })
		store.mutatePool(testResourceGroup, "pool-c", func(p *armpurestorageblock.StoragePool) { p.ID = nil })

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_storage_pools" "test" {
  resource_group_name = %[1]q
}
`, testResourceGroup),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.test", "storage_pools.#", "2"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.test", "storage_pools.0.name", "pool-a"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.test", "storage_pools.0.zone", "1"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.test", "storage_pools.1.name", "pool-b"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.test", "storage_pools.1.location", "eastus2"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.test", "storage_pools.1.resource_group_name", testResourceGroup),
						resource.TestCheckNoResourceAttr("data.everpure_azure_storage_pools.test", "storage_pools.1.zone"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.test", "storage_pools.1.vnet_injection.#", "0"),
					),
				},
			},
		})
	})

	t.Run("lists pools across two resource groups, sorted by id, over a multi-page response", func(t *testing.T) {
		factories, store := newTestProviderFactories()

		seedTestPool(store, testResourceGroup, "pool-c")
		seedTestPool(store, testResourceGroup, "pool-a")
		seedTestPool(store, "rg-other", "pool-b")
		store.mu.Lock()
		store.pageSize = 1 // forces the pager loop to drain more than one page
		store.mu.Unlock()

		// The subscription-wide list orders by the pools' full ARM IDs, which put the
		// resource group name ahead of the pool name ("resourceGroups/rg-other/..."
		// sorts before "resourceGroups/rg-test/..." since "other" < "test"), so
		// pool-b (in rg-other) sorts ahead of both rg-test pools even though its own
		// name is alphabetically between them.
		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_storage_pools" "scoped" {
  resource_group_name = %[1]q
}

data "everpure_azure_storage_pools" "all" {}
`, testResourceGroup),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.scoped", "storage_pools.#", "2"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.scoped", "storage_pools.0.name", "pool-a"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.scoped", "storage_pools.1.name", "pool-c"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.scoped", "storage_pools.0.resource_group_name", testResourceGroup),

						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.all", "storage_pools.#", "3"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.all", "storage_pools.0.name", "pool-b"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.all", "storage_pools.0.resource_group_name", "rg-other"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.all", "storage_pools.1.name", "pool-a"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.all", "storage_pools.1.resource_group_name", testResourceGroup),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.all", "storage_pools.2.name", "pool-c"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.all", "storage_pools.2.resource_group_name", testResourceGroup),
					),
				},
			},
		})
	})

	t.Run("subscription-wide, two same-named pools in different resource groups sort stably by ID", func(t *testing.T) {
		// Two resource groups can each legally hold a pool named "shared-pool"; a
		// subscription-wide list then sees both under the same name, so the
		// resource-group segment of their (distinct) full ARM IDs ("rg-aaa" <
		// "rg-bbb") is what makes the order deterministic, rather than whatever the
		// (map-backed, so unordered) fake happens to return.
		factories, store := newTestProviderFactories()
		seedTestPool(store, "rg-aaa", "shared-pool")
		seedTestPool(store, "rg-bbb", "shared-pool")

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + `
data "everpure_azure_storage_pools" "test" {}
`,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.test", "storage_pools.#", "2"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.test", "storage_pools.0.name", "shared-pool"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.test", "storage_pools.0.resource_group_name", "rg-aaa"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.test", "storage_pools.1.name", "shared-pool"),
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.test", "storage_pools.1.resource_group_name", "rg-bbb"),
					),
				},
			},
		})
	})

	t.Run("split ownership: the pool's own attributes match the resource that manages it", func(t *testing.T) {
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

data "everpure_azure_storage_pools" "test" {
  resource_group_name = everpure_azure_storage_pool.test.resource_group_name
}
`, testResourceGroup, testReservationID(), subnetID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_storage_pools.test", "storage_pools.#", "1"),
						resource.TestCheckResourceAttrPair(
							"data.everpure_azure_storage_pools.test", "storage_pools.0.id",
							"everpure_azure_storage_pool.test", "id",
						),
						resource.TestCheckResourceAttrPair(
							"data.everpure_azure_storage_pools.test", "storage_pools.0.internal_id",
							"everpure_azure_storage_pool.test", "internal_id",
						),
						resource.TestCheckResourceAttrPair(
							"data.everpure_azure_storage_pools.test", "storage_pools.0.provisioned_bandwidth_in_mbps",
							"everpure_azure_storage_pool.test", "provisioned_bandwidth_in_mbps",
						),
					),
				},
			},
		})
	})
}
