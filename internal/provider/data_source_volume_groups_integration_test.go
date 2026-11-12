// SPDX-License-Identifier: Apache-2.0

package provider

// everpure_azure_volume_groups has no input blocks, so the dynamic-block-unknown-list
// regression covered elsewhere (e.g. TestVolumeGroupResourceDynamicSnapshotProtectionPolicyBlock
// in volume_group_integration_test.go) does not apply here: nothing in this data
// source's schema is a block a config could wrap in a dynamic block.

import (
	"fmt"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

func TestVolumeGroupsDataSource(t *testing.T) {
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
data "everpure_azure_volume_groups" "test" {
  storage_pool_id = %[1]q
}
`, testStoragePoolIDForVG()),
					Check: resource.TestCheckResourceAttr("data.everpure_azure_volume_groups.test", "volume_groups.#", "0"),
				},
			},
		})
	})

	t.Run("keeps a group with no properties, and drops one with no id, without failing the list", func(t *testing.T) {
		factories, poolStore, vgStore := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg-a", "PT1H", "P7D")
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg-b", "PT1H", "P7D")
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg-c", "PT1H", "P7D")
		vgStore.mutateVolumeGroup(testResourceGroup, testStoragePoolName, "vg-b", func(g *armpurestorageblock.VolumeGroup) { g.Properties = nil })
		vgStore.mutateVolumeGroup(testResourceGroup, testStoragePoolName, "vg-c", func(g *armpurestorageblock.VolumeGroup) { g.ID = nil })

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_volume_groups" "test" {
  storage_pool_id = %[1]q
}
`, testStoragePoolIDForVG()),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_volume_groups.test", "volume_groups.#", "2"),
						resource.TestCheckResourceAttr("data.everpure_azure_volume_groups.test", "volume_groups.0.name", "vg-a"),
						resource.TestCheckResourceAttr("data.everpure_azure_volume_groups.test", "volume_groups.0.snapshot_protection_policy.#", "1"),
						resource.TestCheckResourceAttr("data.everpure_azure_volume_groups.test", "volume_groups.1.name", "vg-b"),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_volume_groups.test", "volume_groups.1.storage_pool_id", testStoragePoolIDForVG(),
						),
						resource.TestCheckResourceAttr("data.everpure_azure_volume_groups.test", "volume_groups.1.snapshot_protection_policy.#", "0"),
						resource.TestCheckNoResourceAttr("data.everpure_azure_volume_groups.test", "volume_groups.1.iops_limit"),
					),
				},
			},
		})
	})

	t.Run("lists volume groups sorted by id, over a multi-page response", func(t *testing.T) {
		factories, poolStore, vgStore := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg-c", "PT1H", "P7D")
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg-a", "PT1H", "P7D")
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg-b", "PT1H", "P7D")
		vgStore.mu.Lock()
		vgStore.pageSize = 1
		vgStore.mu.Unlock()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_volume_groups" "test" {
  storage_pool_id = %[1]q
}
`, testStoragePoolIDForVG()),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_volume_groups.test", "volume_groups.#", "3"),
						resource.TestCheckResourceAttr("data.everpure_azure_volume_groups.test", "volume_groups.0.name", "vg-a"),
						resource.TestCheckResourceAttr("data.everpure_azure_volume_groups.test", "volume_groups.1.name", "vg-b"),
						resource.TestCheckResourceAttr("data.everpure_azure_volume_groups.test", "volume_groups.2.name", "vg-c"),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_volume_groups.test", "volume_groups.0.storage_pool_id", testStoragePoolIDForVG(),
						),
					),
				},
			},
		})
	})

	t.Run("split ownership: the group's own attributes match the resource that manages it", func(t *testing.T) {
		factories, poolStore, _ := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false
}

data "everpure_azure_volume_groups" "test" {
  storage_pool_id = everpure_azure_volume_group.test.storage_pool_id
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_volume_groups.test", "volume_groups.#", "1"),
						resource.TestCheckResourceAttrPair(
							"data.everpure_azure_volume_groups.test", "volume_groups.0.id",
							"everpure_azure_volume_group.test", "id",
						),
						resource.TestCheckResourceAttrPair(
							"data.everpure_azure_volume_groups.test", "volume_groups.0.internal_id",
							"everpure_azure_volume_group.test", "internal_id",
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
data "everpure_azure_volume_groups" "test" {
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
data "everpure_azure_volume_groups" "test" {
  storage_pool_id = %[1]q
}
`, testStoragePoolIDForVG()),
					ExpectError: diagRegexp(operation{action: actionList, kind: kindVolumeGroups, unnamed: true}.fail(newNotFoundError(kindStoragePool, testStoragePoolIDForVG(), nil, apiFailure{}))),
				},
			},
		})
	})
}
