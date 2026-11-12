// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

const testVolumeGroupStatusDataSourceAddr = "data.everpure_azure_volume_group_status.test"

func seededVolumeGroupStatus() armpurestorageblock.VolumeGroupStatus {
	return armpurestorageblock.VolumeGroupStatus{
		ConnectedHostCount: to.Ptr(int32(2)),
		Space:              &armpurestorageblock.Space{TotalUsed: to.Ptr(int64(10)), Unique: to.Ptr(int64(4)), Snapshots: to.Ptr(int64(3)), Shared: to.Ptr(int64(3))},
	}
}

// TestVolumeGroupStatusDataSource has no dynamic-block case: the data source's schema
// has no blocks, and its one input, volume_group_id, is a scalar.
func TestVolumeGroupStatusDataSource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	statusConfig := func(vgID string) string {
		return providerBlock() + fmt.Sprintf(`
data "everpure_azure_volume_group_status" "test" {
  volume_group_id = %[1]q
}
`, vgID)
	}

	t.Run("happy path: seeded count and space come back", func(t *testing.T) {
		factories, poolStore, vgStore := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg1", "PT1H", "P7D")
		vgStore.setStatus(testResourceGroup, testStoragePoolName, "vg1", seededVolumeGroupStatus())

		vgID := testVGID(testStoragePoolName, "vg1")
		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: statusConfig(vgID),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(testVolumeGroupStatusDataSourceAddr, "volume_group_id", vgID),
						resource.TestCheckResourceAttr(testVolumeGroupStatusDataSourceAddr, "max_connected_hosts_per_volume", "2"),
						resource.TestCheckResourceAttr(testVolumeGroupStatusDataSourceAddr, "space.total_used_in_bytes", "10"),
						resource.TestCheckResourceAttr(testVolumeGroupStatusDataSourceAddr, "space.unique_in_bytes", "4"),
						resource.TestCheckResourceAttr(testVolumeGroupStatusDataSourceAddr, "space.snapshots_in_bytes", "3"),
						resource.TestCheckResourceAttr(testVolumeGroupStatusDataSourceAddr, "space.shared_in_bytes", "3"),
					),
				},
			},
		})
	})

	t.Run("an existing volume group with nothing seeded reads zeros", func(t *testing.T) {
		factories, poolStore, vgStore := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg1", "PT1H", "P7D")

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: statusConfig(testVGID(testStoragePoolName, "vg1")),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(testVolumeGroupStatusDataSourceAddr, "max_connected_hosts_per_volume", "0"),
						resource.TestCheckResourceAttr(testVolumeGroupStatusDataSourceAddr, "space.total_used_in_bytes", "0"),
					),
				},
			},
		})
	})

	t.Run("chained from a volume group resource in the same configuration (volume_group_id unknown at plan)", func(t *testing.T) {
		factories, poolStore, vgStore := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)
		// The status store is independent of the groups map, so it can be seeded
		// before the resource creates the volume group.
		vgStore.setStatus(testResourceGroup, testStoragePoolName, "chained-vg", seededVolumeGroupStatus())

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "chained-vg"
  storage_pool_id     = %[1]q
  deletion_protection = false
}

data "everpure_azure_volume_group_status" "test" {
  volume_group_id = everpure_azure_volume_group.test.id
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(testVolumeGroupStatusDataSourceAddr, "max_connected_hosts_per_volume", "2"),
						resource.TestCheckResourceAttr(testVolumeGroupStatusDataSourceAddr, "space.total_used_in_bytes", "10"),
					),
				},
			},
		})
	})

	t.Run("scoped inside a check block: a passing assert and a failing assert both leave the run successful", func(t *testing.T) {
		factories, poolStore, vgStore := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg1", "PT1H", "P7D")
		vgStore.setStatus(testResourceGroup, testStoragePoolName, "vg1", seededVolumeGroupStatus())

		vgID := testVGID(testStoragePoolName, "vg1")
		checkConfig := func(hostsAbove, usedBelow int) string {
			return providerBlock() + fmt.Sprintf(`
check "volume_group_status" {
  data "everpure_azure_volume_group_status" "test" {
    volume_group_id = %[1]q
  }

  assert {
    condition     = data.everpure_azure_volume_group_status.test.max_connected_hosts_per_volume > %[2]d
    error_message = "volume group is not connected to enough hosts"
  }

  assert {
    condition     = data.everpure_azure_volume_group_status.test.space.total_used_in_bytes < %[3]d
    error_message = "volume group uses too much space"
  }
}
`, vgID, hostsAbove, usedBelow)
		}

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				// A data source scoped to a check block is never stored in state, so
				// every plan re-reads it during apply and shows a pending read.
				{Config: checkConfig(0, 100), ExpectNonEmptyPlan: true},
				// Two hosts is not above 5 and 10 bytes is not below 5: both fail.
				{Config: checkConfig(5, 5), ExpectNonEmptyPlan: true},
			},
		})
	})

	t.Run("API errors inside a check block are downgraded to warnings", func(t *testing.T) {
		tests := []struct {
			name       string
			httpStatus int // 0 leaves the volume group missing, so the fake answers 404 volume_group_not_found
			code       string
		}{
			{name: "volume group not found"},
			{name: "pool not ready (412)", httpStatus: 412, code: "storage_pool_not_ready"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				factories, poolStore, vgStore := newTestVolumeGroupProviderFactories()
				seedTestStoragePool(poolStore)
				if tt.httpStatus != 0 {
					vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg1", "PT1H", "P7D")
					vgStore.setStatusError(testResourceGroup, testStoragePoolName, "vg1", tt.httpStatus, tt.code)
				}

				config := providerBlock() + fmt.Sprintf(`
check "volume_group_status" {
  data "everpure_azure_volume_group_status" "test" {
    volume_group_id = %[1]q
  }

  assert {
    condition     = data.everpure_azure_volume_group_status.test.max_connected_hosts_per_volume > 0
    error_message = "volume group is not connected to any host"
  }
}
`, testVGID(testStoragePoolName, "vg1"))

				resource.Test(t, resource.TestCase{
					IsUnitTest:               true,
					ProtoV6ProviderFactories: factories,
					Steps:                    []resource.TestStep{{Config: config, ExpectNonEmptyPlan: true}},
				})
			})
		}
	})

	t.Run("404 classification on the body's error code", func(t *testing.T) {
		tests := []struct {
			name     string
			code     string // "" leaves the volume group missing, so the fake answers volume_group_not_found
			wantPool bool
		}{
			{"volume group not found", "", false},
			{"unknown code", "some_other_code", false},
			{"pool not found", "storage_pool_not_found", true},
			{"pool deleted", "storage_pool_deleted", true},
			{"pool unreachable", "storage_pool_unreachable", true},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				factories, poolStore, vgStore := newTestVolumeGroupProviderFactories()
				seedTestStoragePool(poolStore)
				if tt.code != "" {
					vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg1", "PT1H", "P7D")
					vgStore.setStatusError(testResourceGroup, testStoragePoolName, "vg1", 404, tt.code)
				}

				vgID := testVGID(testStoragePoolName, "vg1")
				op := operation{action: actionRetrieve, kind: kindVolumeGroupStatus, unnamed: true}
				want := op.fail(newNotFoundError(kindVolumeGroup, vgID, nil, apiFailure{}))
				if tt.wantPool {
					want = op.fail(newStoragePoolUnavailableError(testStoragePoolIDForVG(), vgID, apiFailure{}))
				}

				resource.Test(t, resource.TestCase{
					IsUnitTest:               true,
					ProtoV6ProviderFactories: factories,
					Steps: []resource.TestStep{
						{
							Config:      statusConfig(vgID),
							ExpectError: diagRegexp(want),
						},
					},
				})
			})
		}
	})

	t.Run("a 412 from a pool that is not ready is an API error", func(t *testing.T) {
		factories, poolStore, vgStore := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg1", "PT1H", "P7D")
		vgStore.setStatusError(testResourceGroup, testStoragePoolName, "vg1", 412, "storage_pool_not_ready")

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config:      statusConfig(testVGID(testStoragePoolName, "vg1")),
					ExpectError: regexp.MustCompile("(?s)" + summaryRegexp(operation{action: actionRetrieve, kind: kindVolumeGroupStatus, unnamed: true}.fail(detailError{})).String() + `.*storage_pool_not_ready`),
				},
			},
		})
	})

	t.Run("errors when volume_group_id names another subscription", func(t *testing.T) {
		factories, poolStore, _ := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)

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
					Config:      statusConfig(otherSubscriptionVGID),
					ExpectError: subscriptionMismatchRegexp(kindVolumeGroup, otherSubscriptionVGID),
				},
			},
		})
	})

	t.Run("errors at validation when volume_group_id is not a volume group ID", func(t *testing.T) {
		factories, _, _ := newTestVolumeGroupProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config:      statusConfig("not-an-id"),
					ExpectError: regexp.MustCompile(`(?s)must be the full Azure Resource Manager ID of a\s+volume\s+group`),
				},
			},
		})
	})
}
