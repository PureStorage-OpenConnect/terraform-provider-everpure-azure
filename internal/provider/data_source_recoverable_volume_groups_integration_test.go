// SPDX-License-Identifier: Apache-2.0

package provider

// everpure_azure_recoverable_volume_groups has no input blocks, so the
// dynamic-block-unknown-list regression covered elsewhere in this package does not
// apply here: nothing in this data source's schema is a block a config could wrap in a
// dynamic block.

import (
	"fmt"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

func TestRecoverableVolumeGroupsDataSource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	t.Run("an empty pool gives an empty list", func(t *testing.T) {
		factories, poolStore, _ := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_recoverable_volume_groups" "test" {
  storage_pool_id = %[1]q
}
`, testStoragePoolIDForVG()),
					Check: resource.TestCheckResourceAttr(
						"data.everpure_azure_recoverable_volume_groups.test", "recoverable_volume_groups.#", "0",
					),
				},
			},
		})
	})

	t.Run("lists several twins sharing a previous_name, sorted by id, with their QoS and protection values, over a multi-page response", func(t *testing.T) {
		factories, poolStore, vgStore := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)

		olderDestroyedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		newerDestroyedAt := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		vgStore.seedRecoverableTwin(testResourceGroup, testStoragePoolName, "vg1-20260101000000", "vg1", olderDestroyedAt)
		vgStore.seedRecoverableTwin(testResourceGroup, testStoragePoolName, "vg1-20260601000000", "vg1", newerDestroyedAt)

		// seedRecoverableTwin only fills in soft_deletion; set QoS and protection
		// directly on both seeded twins so the test can also assert those map through.
		vgStore.mu.Lock()
		for _, twin := range vgStore.recoverable[vgPoolKey(testResourceGroup, testStoragePoolName)] {
			twin.Properties.PerformanceParameters = &armpurestorageblock.PerformanceParameters{
				BandwidthLimitMbPerSec: to.Ptr(int64(500)),
				IopsLimit:              to.Ptr(int64(5000)),
			}
			twin.Properties.ProtectionParameters = &armpurestorageblock.ProtectionParameters{
				Frequency: to.Ptr("PT1H"),
				Retention: to.Ptr("P7D"),
			}
		}
		vgStore.pageSize = 1
		vgStore.mu.Unlock()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_recoverable_volume_groups" "test" {
  storage_pool_id = %[1]q
}
`, testStoragePoolIDForVG()),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(
							"data.everpure_azure_recoverable_volume_groups.test", "recoverable_volume_groups.#", "2",
						),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_recoverable_volume_groups.test",
							"recoverable_volume_groups.0.name", "vg1-20260101000000",
						),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_recoverable_volume_groups.test",
							"recoverable_volume_groups.1.name", "vg1-20260601000000",
						),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_recoverable_volume_groups.test",
							"recoverable_volume_groups.0.soft_deletion.previous_name", "vg1",
						),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_recoverable_volume_groups.test",
							"recoverable_volume_groups.1.soft_deletion.previous_name", "vg1",
						),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_recoverable_volume_groups.test",
							"recoverable_volume_groups.0.soft_deletion.destroyed_at", olderDestroyedAt.Format(time.RFC3339),
						),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_recoverable_volume_groups.test",
							"recoverable_volume_groups.1.soft_deletion.destroyed_at", newerDestroyedAt.Format(time.RFC3339),
						),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_recoverable_volume_groups.test",
							"recoverable_volume_groups.0.storage_pool_id", testStoragePoolIDForVG(),
						),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_recoverable_volume_groups.test",
							"recoverable_volume_groups.0.bandwidth_limit_in_mbps", "500",
						),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_recoverable_volume_groups.test",
							"recoverable_volume_groups.0.iops_limit", "5000",
						),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_recoverable_volume_groups.test",
							"recoverable_volume_groups.0.snapshot_protection_policy.0.frequency", "PT1H",
						),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_recoverable_volume_groups.test",
							"recoverable_volume_groups.0.snapshot_protection_policy.0.retention", "P7D",
						),
					),
				},
			},
		})
	})

	// "Split ownership" here can't be one config with both a live resource and the data
	// source (unlike the other three plural data sources): a recoverable volume group
	// only ever exists because a everpure_azure_volume_group resource was
	// soft-deleted, and reading this data source in the same apply that destroys the
	// resource that produced its twin has no ordering guarantee — Terraform is free to
	// read an unrelated data source before, during or after a resource destroy within
	// one apply. Three separate steps sidestep that race instead: step 1 creates the
	// resource; step 2 removes it from config, which destroys it (a plain soft delete,
	// since eradicate_on_destroy defaults to false) — its own data source read here may
	// or may not see the fresh twin, and nothing is asserted about it; step 3 reapplies
	// the identical (resource-free) config as its own separate `terraform apply`, by
	// which point step 2's destroy has fully finished, so its data source read is
	// guaranteed to see the twin.
	t.Run("finds the twin a volume group resource's own soft delete just left behind", func(t *testing.T) {
		factories, poolStore, _ := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)

		withResource := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                    = "vg1"
  storage_pool_id         = %[1]q
  deletion_protection     = false
  bandwidth_limit_in_mbps = 500
  iops_limit              = 5000

  snapshot_protection_policy {
    frequency = "PT1H"
    retention = "P7D"
  }
}

data "everpure_azure_recoverable_volume_groups" "test" {
  storage_pool_id = %[1]q
}
`, testStoragePoolIDForVG())

		withoutResource := providerBlock() + fmt.Sprintf(`
data "everpure_azure_recoverable_volume_groups" "test" {
  storage_pool_id = %[1]q
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: withResource,
					Check: resource.TestCheckResourceAttr(
						"data.everpure_azure_recoverable_volume_groups.test", "recoverable_volume_groups.#", "0",
					),
				},
				{
					// The resource's removal destroys it (soft delete); this step's own
					// data source read is racy against that destroy, so nothing here is
					// asserted about its result.
					Config: withoutResource,
				},
				{
					// A separate, later `terraform apply` of the identical config: the
					// destroy above has long since finished, so this read is guaranteed
					// to see the twin it left behind.
					Config: withoutResource,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(
							"data.everpure_azure_recoverable_volume_groups.test", "recoverable_volume_groups.#", "1",
						),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_recoverable_volume_groups.test",
							"recoverable_volume_groups.0.soft_deletion.previous_name", "vg1",
						),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_recoverable_volume_groups.test",
							"recoverable_volume_groups.0.bandwidth_limit_in_mbps", "500",
						),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_recoverable_volume_groups.test",
							"recoverable_volume_groups.0.iops_limit", "5000",
						),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_recoverable_volume_groups.test",
							"recoverable_volume_groups.0.snapshot_protection_policy.0.frequency", "PT1H",
						),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_recoverable_volume_groups.test",
							"recoverable_volume_groups.0.snapshot_protection_policy.0.retention", "P7D",
						),
					),
				},
			},
		})
	})

	t.Run("errors when storage_pool_id names another subscription", func(t *testing.T) {
		factories, poolStore, _ := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)

		otherSubscriptionPoolID := armids.StoragePoolID{
			ResourceGroupID: armids.ResourceGroupID{
				SubscriptionID:    "22222222-2222-2222-2222-222222222222",
				ResourceGroupName: testResourceGroup,
			},
			StoragePoolName: testStoragePoolName,
		}.FullID()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_recoverable_volume_groups" "test" {
  storage_pool_id = %[1]q
}
`, otherSubscriptionPoolID),
					ExpectError: subscriptionMismatchRegexp(kindStoragePool, otherSubscriptionPoolID),
				},
			},
		})
	})

	t.Run("errors when the storage pool itself doesn't exist", func(t *testing.T) {
		factories, poolStore, vgStore := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)
		vgStore.markStoragePoolMissing(testResourceGroup, testStoragePoolName)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_recoverable_volume_groups" "test" {
  storage_pool_id = %[1]q
}
`, testStoragePoolIDForVG()),
					ExpectError: diagRegexp(operation{action: actionList, kind: kindRecoverableVolumeGroups, unnamed: true}.fail(newNotFoundError(kindStoragePool, testStoragePoolIDForVG(), nil, apiFailure{}))),
				},
			},
		})
	})
}
