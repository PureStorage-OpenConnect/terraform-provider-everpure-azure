// SPDX-License-Identifier: Apache-2.0

package provider

// everpure_azure_snapshot has no input blocks, so the dynamic-block-unknown-list
// regression covered elsewhere in this package does not apply here.

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/compare"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

func TestSnapshotDataSource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	t.Run("looks up a snapshot managed by a resource in the same configuration", func(t *testing.T) {
		factories, _ := newTestSnapshotProviderFactories()

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_snapshot" "test" {
  name                = "snap1"
  volume_group_id     = %[1]q
  deletion_protection = false
}

data "everpure_azure_snapshot" "test" {
  name            = everpure_azure_snapshot.test.name
  volume_group_id = everpure_azure_snapshot.test.volume_group_id
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
							"everpure_azure_snapshot.test", tfjsonpath.New("id"),
							"data.everpure_azure_snapshot.test", tfjsonpath.New("id"),
							compare.ValuesSame(),
						),
						statecheck.CompareValuePairs(
							"everpure_azure_snapshot.test", tfjsonpath.New("created_at"),
							"data.everpure_azure_snapshot.test", tfjsonpath.New("created_at"),
							compare.ValuesSame(),
						),
						// Element by element, not the whole list: the data source's elements
						// also carry space, which the resource's do not. The list length is
						// compared in Check, as the paths here cannot address a count.
						statecheck.CompareValuePairs(
							"everpure_azure_snapshot.test", tfjsonpath.New("volume_snapshots").AtSliceIndex(0).AtMapKey("volume_name"),
							"data.everpure_azure_snapshot.test", tfjsonpath.New("volume_snapshots").AtSliceIndex(0).AtMapKey("volume_name"),
							compare.ValuesSame(),
						),
						statecheck.CompareValuePairs(
							"everpure_azure_snapshot.test", tfjsonpath.New("volume_snapshots").AtSliceIndex(0).AtMapKey("serial_number"),
							"data.everpure_azure_snapshot.test", tfjsonpath.New("volume_snapshots").AtSliceIndex(0).AtMapKey("serial_number"),
							compare.ValuesSame(),
						),
						statecheck.CompareValuePairs(
							"everpure_azure_snapshot.test", tfjsonpath.New("volume_snapshots").AtSliceIndex(0).AtMapKey("provisioned_size_in_bytes"),
							"data.everpure_azure_snapshot.test", tfjsonpath.New("volume_snapshots").AtSliceIndex(0).AtMapKey("provisioned_size_in_bytes"),
							compare.ValuesSame(),
						),
					},
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttrPair("everpure_azure_snapshot.test", "volume_snapshots.#", "data.everpure_azure_snapshot.test", "volume_snapshots.#"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshot.test", "created_by_policy", "false"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshot.test", "space.total_used_in_bytes", "4096"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshot.test", "space.unique_in_bytes", "2048"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshot.test", "space.snapshots_in_bytes", "1024"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshot.test", "space.shared_in_bytes", "512"),
					),
				},
			},
		})
	})

	t.Run("looks up a snapshot taken by the snapshot schedule, by its all-digit name", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "17", mustParseTime(t, "2026-09-01T12:00:00Z"), true)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_snapshot" "test" {
  name            = "17"
  volume_group_id = %[1]q
}
`, testVolumeGroupID("vg1")),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_snapshot.test", "id", testVolumeGroupSnapshotID("vg1", "17")),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshot.test", "created_by_policy", "true"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshot.test", "created_at", "2026-09-01T12:00:00Z"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshot.test", "volume_snapshots.#", "1"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshot.test", "volume_snapshots.0.volume_name", "member-vol"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshot.test", "volume_snapshots.0.space.total_used_in_bytes", "300"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshot.test", "volume_snapshots.0.space.unique_in_bytes", "200"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshot.test", "volume_snapshots.0.space.snapshots_in_bytes", "100"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshot.test", "volume_snapshots.0.space.shared_in_bytes", "50"),
					),
				},
			},
		})
	})

	t.Run("errors when no snapshot matches the name", func(t *testing.T) {
		factories, _ := newTestSnapshotProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_snapshot" "test" {
  name            = "does-not-exist"
  volume_group_id = %[1]q
}
`, testVolumeGroupID("vg1")),
					ExpectError: diagRegexp(operation{action: actionRetrieve, kind: kindSnapshot, name: "does-not-exist"}.fail(
						newNotFoundError(kindSnapshot, testVolumeGroupSnapshotID("vg1", "does-not-exist"), &kindVolumeGroup, apiFailure{}))),
				},
			},
		})
	})

	t.Run("errors when volume_group_id names another subscription", func(t *testing.T) {
		factories, _ := newTestSnapshotProviderFactories()

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
data "everpure_azure_snapshot" "test" {
  name            = "snap1"
  volume_group_id = %[1]q
}
`, otherSubscriptionVolumeGroupID),
					ExpectError: subscriptionMismatchRegexp(kindVolumeGroup, otherSubscriptionVolumeGroupID),
				},
			},
		})
	})

	t.Run("looking up a soft-deleted snapshot's timestamped name errors instead of returning it", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		store.seedDestroyedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap1-20260901000000", "snap1", mustParseTime(t, "2026-09-01T00:00:00Z"), false)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_snapshot" "test" {
  name            = "snap1-20260901000000"
  volume_group_id = %[1]q
}
`, testVolumeGroupID("vg1")),
					ExpectError: diagRegexp(operation{action: actionRetrieve, kind: kindSnapshot, name: "snap1-20260901000000"}.fail(
						newSnapshotSoftDeletedError("snap1-20260901000000", "snap1", store.fakeSnapshotID(testResourceGroup, testStoragePoolName, "vg1", "snap1-20260901000000"), "2026-10-01T00:00:00Z", false))),
				},
			},
		})
	})

	t.Run("looking up the original name after a soft delete is not found", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		// Only the destroyed twin exists: the original name was freed by the
		// (simulated) soft delete.
		store.seedDestroyedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap1-20260901000000", "snap1", mustParseTime(t, "2026-09-01T00:00:00Z"), false)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_snapshot" "test" {
  name            = "snap1"
  volume_group_id = %[1]q
}
`, testVolumeGroupID("vg1")),
					ExpectError: diagRegexp(operation{action: actionRetrieve, kind: kindSnapshot, name: "snap1"}.fail(
						newNotFoundError(kindSnapshot, testVolumeGroupSnapshotID("vg1", "snap1"), &kindVolumeGroup, apiFailure{}))),
				},
			},
		})
	})

	t.Run("rejects names outside the path pattern at plan time", func(t *testing.T) {
		for _, name := range []string{"-snap", "snap_1", "snap-"} {
			t.Run(name, func(t *testing.T) {
				factories, _ := newTestSnapshotProviderFactories()
				resource.Test(t, resource.TestCase{
					IsUnitTest:               true,
					ProtoV6ProviderFactories: factories,
					Steps: []resource.TestStep{{
						Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_snapshot" "test" {
  name            = %[1]q
  volume_group_id = %[2]q
}
`, name, testVolumeGroupID("vg1")),
						ExpectError: regexp.MustCompile(`must be 1-64`),
					}},
				})
			})
		}
	})
}
