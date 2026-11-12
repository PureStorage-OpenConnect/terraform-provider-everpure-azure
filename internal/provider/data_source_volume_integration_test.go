// SPDX-License-Identifier: Apache-2.0

package provider

// everpure_azure_volume has no input blocks, so the dynamic-block-unknown-list
// regression covered by the volume resource's own tests (see volume_integration_test.go)
// does not apply here: nothing in this data source's schema is a block a config could
// wrap in a dynamic block. There is also no "case-differing name" test here, unlike the
// storage pool and volume group data sources: volume names do not fold on lookup
// (confirmed live, unlike pools and volume groups), so the fake's Get deliberately stays
// an exact-match lookup for volumes and a differently-cased name simply 404s, exactly as
// real ARM does.

import (
	"fmt"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-testing/compare"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

func TestVolumeDataSource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	t.Run("looks up a volume managed by a resource in the same configuration", func(t *testing.T) {
		factories, _ := newTestVolumeProviderFactories()

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 5
}

data "everpure_azure_volume" "test" {
  name            = everpure_azure_volume.test.name
  volume_group_id = everpure_azure_volume.test.volume_group_id
}
`, testVolumeGroupID("vg1"))

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					ConfigStateChecks: []statecheck.StateCheck{
						statecheck.CompareValuePairs(
							"everpure_azure_volume.test", tfjsonpath.New("id"),
							"data.everpure_azure_volume.test", tfjsonpath.New("id"),
							compare.ValuesSame(),
						),
						statecheck.CompareValuePairs(
							"everpure_azure_volume.test", tfjsonpath.New("provisioned_size_in_bytes"),
							"data.everpure_azure_volume.test", tfjsonpath.New("provisioned_size_in_bytes"),
							compare.ValuesSame(),
						),
						statecheck.CompareValuePairs(
							"everpure_azure_volume.test", tfjsonpath.New("serial_number"),
							"data.everpure_azure_volume.test", tfjsonpath.New("serial_number"),
							compare.ValuesSame(),
						),
					},
				},
			},
		})
	})

	t.Run("looks up a volume seeded directly (not managed by any resource in this configuration)", func(t *testing.T) {
		factories, store := newTestVolumeProviderFactories()

		store.mu.Lock()
		store.volumes[volumeKey(testResourceGroup, testStoragePoolName, "vg1", "seeded-vol")] = &armpurestorageblock.Volume{
			ID:   to.Ptr(store.fakeVolumeID(testResourceGroup, testStoragePoolName, "vg1", "seeded-vol")),
			Name: to.Ptr("seeded-vol"),
			Properties: &armpurestorageblock.AzureVolumeProperties{
				ProvisionedSize: to.Ptr(int64(1073741824)),
				SerialNumber:    to.Ptr(store.nextSerialNumber()),
				Space:           fakeVolumeSpace(),
			},
		}
		store.mu.Unlock()

		config := providerBlock() + fmt.Sprintf(`
data "everpure_azure_volume" "test" {
  name            = "seeded-vol"
  volume_group_id = %[1]q
}
`, testVolumeGroupID("vg1"))

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_volume.test", "provisioned_size_in_bytes", "1073741824"),
						resource.TestCheckResourceAttrSet("data.everpure_azure_volume.test", "serial_number"),
						resource.TestCheckResourceAttrSet("data.everpure_azure_volume.test", "id"),
						resource.TestCheckResourceAttr("data.everpure_azure_volume.test", "space.total_used_in_bytes", "8192"),
						resource.TestCheckResourceAttr("data.everpure_azure_volume.test", "space.unique_in_bytes", "4096"),
						resource.TestCheckResourceAttr("data.everpure_azure_volume.test", "space.snapshots_in_bytes", "2048"),
						resource.TestCheckResourceAttr("data.everpure_azure_volume.test", "space.shared_in_bytes", "1024"),
					),
				},
			},
		})
	})

	t.Run("errors when no volume matches the name", func(t *testing.T) {
		factories, _ := newTestVolumeProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_volume" "test" {
  name            = "does-not-exist"
  volume_group_id = %[1]q
}
`, testVolumeGroupID("vg1")),
					ExpectError: diagRegexp(operation{action: actionRetrieve, kind: kindVolume, name: "does-not-exist"}.fail(
						newNotFoundError(kindVolume, testVolumeSourceID("vg1", "does-not-exist"), &kindVolumeGroup, apiFailure{}))),
				},
			},
		})
	})

	t.Run("errors when volume_group_id names another subscription", func(t *testing.T) {
		factories, _ := newTestVolumeProviderFactories()

		otherSubscriptionVolumeGroupID := armids.VolumeGroupID{
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
data "everpure_azure_volume" "test" {
  name            = "vol1"
  volume_group_id = %[1]q
}
`, otherSubscriptionVolumeGroupID),
					ExpectError: subscriptionMismatchRegexp(kindVolumeGroup, otherSubscriptionVolumeGroupID),
				},
			},
		})
	})

	t.Run("looking up a soft-deleted volume's timestamped name errors instead of returning it", func(t *testing.T) {
		factories, store := newTestVolumeProviderFactories()

		destroyedAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		store.seedDestroyedVolume(testResourceGroup, testStoragePoolName, "vg1", "vol1-20260901000000", "vol1", destroyedAt)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_volume" "test" {
  name            = "vol1-20260901000000"
  volume_group_id = %[1]q
}
`, testVolumeGroupID("vg1")),
					ExpectError: diagRegexp(operation{action: actionRetrieve, kind: kindVolume, name: "vol1-20260901000000"}.fail(
						newVolumeSoftDeletedError("vol1-20260901000000", "vol1", store.fakeVolumeID(testResourceGroup, testStoragePoolName, "vg1", "vol1-20260901000000"), "2026-10-01T00:00:00Z"))),
				},
			},
		})
	})

	t.Run("looking up the original name after a soft delete is not found", func(t *testing.T) {
		factories, store := newTestVolumeProviderFactories()

		destroyedAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		// Only the destroyed twin exists — the original name was freed by the
		// (simulated) soft delete and nothing occupies it anymore.
		store.seedDestroyedVolume(testResourceGroup, testStoragePoolName, "vg1", "vol1-20260901000000", "vol1", destroyedAt)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_volume" "test" {
  name            = "vol1"
  volume_group_id = %[1]q
}
`, testVolumeGroupID("vg1")),
					ExpectError: diagRegexp(operation{action: actionRetrieve, kind: kindVolume, name: "vol1"}.fail(
						newNotFoundError(kindVolume, testVolumeSourceID("vg1", "vol1"), &kindVolumeGroup, apiFailure{}))),
				},
			},
		})
	})
}
