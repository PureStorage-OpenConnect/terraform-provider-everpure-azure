// SPDX-License-Identifier: Apache-2.0

package provider

// everpure_azure_volumes has no input blocks, so the dynamic-block-unknown-list
// regression covered elsewhere in this package does not apply here: nothing in this
// data source's schema is a block a config could wrap in a dynamic block.

import (
	"fmt"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

func TestVolumesDataSource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	t.Run("an empty volume group gives an empty list", func(t *testing.T) {
		factories, _ := newTestVolumeProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_volumes" "test" {
  volume_group_id = %[1]q
}
`, testVolumeGroupID("vg1")),
					Check: resource.TestCheckResourceAttr("data.everpure_azure_volumes.test", "volumes.#", "0"),
				},
			},
		})
	})

	t.Run("lists volumes sorted by id, over a multi-page response", func(t *testing.T) {
		factories, store := newTestVolumeProviderFactories()
		store.seedVolume(testResourceGroup, testStoragePoolName, "vg1", "vol-c", 1073741824)
		store.seedVolume(testResourceGroup, testStoragePoolName, "vg1", "vol-a", 1073741824)
		store.seedVolume(testResourceGroup, testStoragePoolName, "vg1", "vol-b", 1073741824)
		store.mu.Lock()
		store.pageSize = 1
		store.mu.Unlock()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_volumes" "test" {
  volume_group_id = %[1]q
}
`, testVolumeGroupID("vg1")),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_volumes.test", "volumes.#", "3"),
						resource.TestCheckResourceAttr("data.everpure_azure_volumes.test", "volumes.0.name", "vol-a"),
						resource.TestCheckResourceAttr("data.everpure_azure_volumes.test", "volumes.1.name", "vol-b"),
						resource.TestCheckResourceAttr("data.everpure_azure_volumes.test", "volumes.2.name", "vol-c"),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_volumes.test", "volumes.0.volume_group_id", testVolumeGroupID("vg1"),
						),
						resource.TestCheckNoResourceAttr("data.everpure_azure_volumes.test", "volumes.0.soft_deletion"),
						resource.TestCheckResourceAttr("data.everpure_azure_volumes.test", "volumes.0.space.total_used_in_bytes", "8192"),
						resource.TestCheckResourceAttr("data.everpure_azure_volumes.test", "volumes.0.space.shared_in_bytes", "1024"),
					),
				},
			},
		})
	})

	t.Run("split ownership: the volume's own attributes match the resource that manages it", func(t *testing.T) {
		factories, _ := newTestVolumeProviderFactories()

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                     = "vol1"
  volume_group_id          = %[1]q
  provisioned_size_in_gib  = 10
  deletion_protection      = false
}

data "everpure_azure_volumes" "test" {
  volume_group_id = everpure_azure_volume.test.volume_group_id
}
`, testVolumeGroupID("vg1"))

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_volumes.test", "volumes.#", "1"),
						resource.TestCheckResourceAttrPair(
							"data.everpure_azure_volumes.test", "volumes.0.id",
							"everpure_azure_volume.test", "id",
						),
						resource.TestCheckResourceAttrPair(
							"data.everpure_azure_volumes.test", "volumes.0.serial_number",
							"everpure_azure_volume.test", "serial_number",
						),
						resource.TestCheckResourceAttrPair(
							"data.everpure_azure_volumes.test", "volumes.0.provisioned_size_in_bytes",
							"everpure_azure_volume.test", "provisioned_size_in_bytes",
						),
					),
				},
			},
		})
	})

	t.Run("excludes a soft-deleted twin by default, and includes it with soft_deletion populated when asked", func(t *testing.T) {
		factories, store := newTestVolumeProviderFactories()
		store.seedVolume(testResourceGroup, testStoragePoolName, "vg1", "vol1", 1073741824)
		destroyedAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		store.seedDestroyedVolume(testResourceGroup, testStoragePoolName, "vg1", "vol1-20260901000000", "vol1", destroyedAt)

		// Two separate steps, not two data sources in one config: the fake SDK's list
		// pager keeps per-URL state, and two data sources issuing a list against the
		// identical URL in the same apply race that state (seen as an intermittent
		// "fake paged response is empty" failure) — a generated-SDK-fake limitation,
		// not something either data source's own Read causes.
		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_volumes" "test" {
  volume_group_id = %[1]q
}
`, testVolumeGroupID("vg1")),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_volumes.test", "volumes.#", "1"),
						resource.TestCheckResourceAttr("data.everpure_azure_volumes.test", "volumes.0.name", "vol1"),
						resource.TestCheckNoResourceAttr("data.everpure_azure_volumes.test", "volumes.0.soft_deletion"),
					),
				},
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_volumes" "test" {
  volume_group_id   = %[1]q
  include_destroyed = true
}
`, testVolumeGroupID("vg1")),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_volumes.test", "volumes.#", "2"),
						resource.TestCheckResourceAttr("data.everpure_azure_volumes.test", "volumes.0.name", "vol1"),
						resource.TestCheckNoResourceAttr("data.everpure_azure_volumes.test", "volumes.0.soft_deletion"),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_volumes.test", "volumes.1.name", "vol1-20260901000000",
						),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_volumes.test", "volumes.1.soft_deletion.previous_name", "vol1",
						),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_volumes.test", "volumes.1.soft_deletion.destroyed_at",
							destroyedAt.Format(time.RFC3339),
						),
					),
				},
			},
		})
	})

	t.Run("errors when volume_group_id names another subscription", func(t *testing.T) {
		factories, _ := newTestVolumeProviderFactories()

		otherSubscriptionVGID := armids.VolumeGroupID{
			ResourceGroupID: armids.ResourceGroupID{
				SubscriptionID:    "22222222-2222-2222-2222-222222222222",
				ResourceGroupName: testResourceGroup,
			},
			StoragePoolName: testStoragePoolName,
			VolumeGroupName: "vg1",
		}.FullID()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_volumes" "test" {
  volume_group_id = %[1]q
}
`, otherSubscriptionVGID),
					ExpectError: subscriptionMismatchRegexp(kindVolumeGroup, otherSubscriptionVGID),
				},
			},
		})
	})

	t.Run("errors when the volume group itself doesn't exist", func(t *testing.T) {
		factories, store := newTestVolumeProviderFactories()
		store.markVolumeGroupMissing(testResourceGroup, testStoragePoolName, "vg1")

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_volumes" "test" {
  volume_group_id = %[1]q
}
`, testVolumeGroupID("vg1")),
					ExpectError: diagRegexp(operation{action: actionList, kind: kindVolumes, unnamed: true}.fail(
						newNotFoundError(kindVolumeGroup, testVolumeGroupID("vg1"), nil, apiFailure{}))),
				},
			},
		})
	})
}
