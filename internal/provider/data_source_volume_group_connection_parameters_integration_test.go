// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

const testVGConnectionParamsDataSourceAddr = "data.everpure_azure_volume_group_connection_parameters.test"

// TestVolumeGroupConnectionParametersDataSource has no dynamic-block case: the data
// source's schema has no blocks, and its one input, volume_group_id, is a scalar.
func TestVolumeGroupConnectionParametersDataSource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	t.Run("happy path: endpoints seeded out of order come back sorted", func(t *testing.T) {
		factories, poolStore, vgStore := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg1", "PT1H", "P7D")
		vgStore.setConnectionParameters(testResourceGroup, testStoragePoolName, "vg1", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.5", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3261),
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
		})

		vgID := testVGID(testStoragePoolName, "vg1")
		config := providerBlock() + fmt.Sprintf(`
data "everpure_azure_volume_group_connection_parameters" "test" {
  volume_group_id = %[1]q
}
`, vgID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(testVGConnectionParamsDataSourceAddr, "iscsi_endpoints.#", "3"),
						resource.TestCheckResourceAttr(testVGConnectionParamsDataSourceAddr, "iscsi_endpoints.0.ip", "10.0.1.4"),
						resource.TestCheckResourceAttr(testVGConnectionParamsDataSourceAddr, "iscsi_endpoints.0.port", "3260"),
						resource.TestCheckResourceAttr(testVGConnectionParamsDataSourceAddr, "iscsi_endpoints.1.ip", "10.0.1.4"),
						resource.TestCheckResourceAttr(testVGConnectionParamsDataSourceAddr, "iscsi_endpoints.1.port", "3261"),
						resource.TestCheckResourceAttr(testVGConnectionParamsDataSourceAddr, "iscsi_endpoints.2.ip", "10.0.1.5"),
						resource.TestCheckResourceAttr(testVGConnectionParamsDataSourceAddr, "iscsi_endpoints.2.iqn", "iqn.2010-01.com.purestorage.psc:aaa-vg1"),
					),
				},
			},
		})
	})

	t.Run("chained from a volume group resource in the same configuration (volume_group_id unknown at plan)", func(t *testing.T) {
		factories, poolStore, vgStore := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)
		// The volume group is named literally in config below, so its connection
		// parameters can be seeded before the resource creates it: the fake's
		// connectionParameters map is independent of its groups map, and is keyed by
		// name alone (see fakeVolumeGroupStore's own doc comment).
		vgStore.setConnectionParameters(testResourceGroup, testStoragePoolName, "chained-vg", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-chained-vg", 3260),
		})

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "chained-vg"
  storage_pool_id     = %[1]q
  deletion_protection = false
}

data "everpure_azure_volume_group_connection_parameters" "test" {
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
						resource.TestCheckResourceAttr(testVGConnectionParamsDataSourceAddr, "iscsi_endpoints.#", "1"),
						resource.TestCheckResourceAttr(testVGConnectionParamsDataSourceAddr, "iscsi_endpoints.0.ip", "10.0.1.4"),
					),
				},
			},
		})
	})

	t.Run("errors with this data source's own wording when the volume group doesn't exist", func(t *testing.T) {
		factories, poolStore, _ := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)

		vgID := testVGID(testStoragePoolName, "does-not-exist")
		config := providerBlock() + fmt.Sprintf(`
data "everpure_azure_volume_group_connection_parameters" "test" {
  volume_group_id = %[1]q
}
`, vgID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config:      config,
					ExpectError: diagRegexp(operation{action: actionRetrieve, kind: kindVolumeGroupConnectionParameters, unnamed: true}.fail(newNotFoundError(kindVolumeGroup, vgID, nil, apiFailure{}))),
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

		config := providerBlock() + fmt.Sprintf(`
data "everpure_azure_volume_group_connection_parameters" "test" {
  volume_group_id = %[1]q
}
`, otherSubscriptionVGID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config:      config,
					ExpectError: subscriptionMismatchRegexp(kindVolumeGroup, otherSubscriptionVGID),
				},
			},
		})
	})

	t.Run("errors at validation when volume_group_id names a recoverable volume group", func(t *testing.T) {
		factories, poolStore, vgStore := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)

		recoverableID := vgStore.fakeRecoverableVGID(testResourceGroup, testStoragePoolName, "vg1-20260101000000")
		config := providerBlock() + fmt.Sprintf(`
data "everpure_azure_volume_group_connection_parameters" "test" {
  volume_group_id = %[1]q
}
`, recoverableID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config:      config,
					ExpectError: regexp.MustCompile(`(?s)must be the full Azure Resource Manager ID of a\s+volume group`),
				},
			},
		})
	})

	t.Run("zero endpoints: an empty list, and apply still succeeds (a warning, not an error)", func(t *testing.T) {
		factories, poolStore, vgStore := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)
		vgStore.setConnectionParameters(testResourceGroup, testStoragePoolName, "vg-empty", nil)

		vgID := testVGID(testStoragePoolName, "vg-empty")
		config := providerBlock() + fmt.Sprintf(`
data "everpure_azure_volume_group_connection_parameters" "test" {
  volume_group_id = %[1]q
}
`, vgID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check:  resource.TestCheckResourceAttr(testVGConnectionParamsDataSourceAddr, "iscsi_endpoints.#", "0"),
				},
			},
		})
	})
}
