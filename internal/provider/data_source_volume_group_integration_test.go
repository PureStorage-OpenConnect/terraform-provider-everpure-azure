// SPDX-License-Identifier: Apache-2.0

package provider

// everpure_azure_volume_group has no input blocks, so the dynamic-block-unknown-list
// regression covered by the volume group resource's own dynamic-block tests (see
// volume_group_integration_test.go) does not apply here: nothing in this data source's
// schema is a block a config could wrap in a dynamic block.

import (
	"fmt"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-testing/compare"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

func TestVolumeGroupDataSource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	t.Run("looks up a volume group managed by a resource in the same configuration", func(t *testing.T) {
		factories, _, _ := newTestVolumeGroupProviderFactories()

		config := providerBlock() + fmt.Sprintf(`
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

  tags = {
    environment = "test"
  }
}

data "everpure_azure_volume_group" "test" {
  name            = everpure_azure_volume_group.test.name
  storage_pool_id = everpure_azure_volume_group.test.storage_pool_id
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					ConfigStateChecks: []statecheck.StateCheck{
						statecheck.CompareValuePairs(
							"everpure_azure_volume_group.test", tfjsonpath.New("id"),
							"data.everpure_azure_volume_group.test", tfjsonpath.New("id"),
							compare.ValuesSame(),
						),
						statecheck.CompareValuePairs(
							"everpure_azure_volume_group.test", tfjsonpath.New("bandwidth_limit_in_mbps"),
							"data.everpure_azure_volume_group.test", tfjsonpath.New("bandwidth_limit_in_mbps"),
							compare.ValuesSame(),
						),
						statecheck.CompareValuePairs(
							"everpure_azure_volume_group.test", tfjsonpath.New("iops_limit"),
							"data.everpure_azure_volume_group.test", tfjsonpath.New("iops_limit"),
							compare.ValuesSame(),
						),
						statecheck.CompareValuePairs(
							"everpure_azure_volume_group.test", tfjsonpath.New("tags"),
							"data.everpure_azure_volume_group.test", tfjsonpath.New("tags"),
							compare.ValuesSame(),
						),
						statecheck.CompareValuePairs(
							"everpure_azure_volume_group.test", tfjsonpath.New("internal_id"),
							"data.everpure_azure_volume_group.test", tfjsonpath.New("internal_id"),
							compare.ValuesSame(),
						),
						statecheck.CompareValuePairs(
							"everpure_azure_volume_group.test", tfjsonpath.New("snapshot_protection_policy"),
							"data.everpure_azure_volume_group.test", tfjsonpath.New("snapshot_protection_policy"),
							compare.ValuesSame(),
						),
					},
				},
			},
		})
	})

	t.Run("looks up a volume group seeded directly (not managed by any resource in this configuration)", func(t *testing.T) {
		factories, poolStore, vgStore := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "seeded-vg", "PT1H", "P7D")

		config := providerBlock() + fmt.Sprintf(`
data "everpure_azure_volume_group" "test" {
  name            = "seeded-vg"
  storage_pool_id = %[1]q
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_volume_group.test", "internal_id", "internal-seeded-vg"),
						resource.TestCheckResourceAttr("data.everpure_azure_volume_group.test", "snapshot_protection_policy.#", "1"),
						resource.TestCheckResourceAttr("data.everpure_azure_volume_group.test", "snapshot_protection_policy.0.frequency", "PT1H"),
						resource.TestCheckResourceAttr("data.everpure_azure_volume_group.test", "snapshot_protection_policy.0.retention", "P7D"),
						resource.TestCheckNoResourceAttr("data.everpure_azure_volume_group.test", "bandwidth_limit_in_mbps"),
						resource.TestCheckNoResourceAttr("data.everpure_azure_volume_group.test", "iops_limit"),
						resource.TestCheckNoResourceAttr("data.everpure_azure_volume_group.test", "tags.%"),
					),
				},
			},
		})
	})

	t.Run("without a snapshot_protection_policy, the list is empty", func(t *testing.T) {
		factories, poolStore, vgStore := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "no-policy-vg", "PT1H", "P7D")
		// seedVolumeGroup always sets ProtectionParameters; simulate "no schedule" the
		// way a real GET would report it (nil), matching applyVolumeGroup's own
		// contract (protectionParametersFromAPI treats nil as "no policy").
		vgStore.mutateVolumeGroup(testResourceGroup, testStoragePoolName, "no-policy-vg", func(vg *armpurestorageblock.VolumeGroup) {
			vg.Properties.ProtectionParameters = nil
		})

		config := providerBlock() + fmt.Sprintf(`
data "everpure_azure_volume_group" "test" {
  name            = "no-policy-vg"
  storage_pool_id = %[1]q
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check:  resource.TestCheckResourceAttr("data.everpure_azure_volume_group.test", "snapshot_protection_policy.#", "0"),
				},
			},
		})
	})

	t.Run("errors when no volume group matches the name", func(t *testing.T) {
		factories, poolStore, _ := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_volume_group" "test" {
  name            = "does-not-exist"
  storage_pool_id = %[1]q
}
`, testStoragePoolIDForVG()),
					ExpectError: diagRegexp(operation{action: actionRetrieve, kind: kindVolumeGroup, name: "does-not-exist"}.fail(newNotFoundError(kindVolumeGroup, testStoragePoolIDForVG()+"/volumeGroups/does-not-exist", &kindStoragePool, apiFailure{}))),
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
data "everpure_azure_volume_group" "test" {
  name            = "vg1"
  storage_pool_id = %[1]q
}
`, otherSubscriptionPoolID),
					ExpectError: subscriptionMismatchRegexp(kindStoragePool, otherSubscriptionPoolID),
				},
			},
		})
	})

	// The fake's Get now folds name casing case-insensitively (mirroring ARM's own
	// behavior for volume groups), so this config's differently-cased name still finds
	// the seeded group below. What this test actually exists to prove is what happens
	// next: applyVolumeGroup maps Name from the API response (the group's own stored
	// casing), so without the data source restoring it to what was configured, this
	// apply would fail with "Provider produced invalid object" (name is Required, not
	// Computed) the moment the two disagreed.
	t.Run("succeeds when the configured name differs in case from the group's own, and keeps the configured spelling", func(t *testing.T) {
		factories, poolStore, vgStore := newTestVolumeGroupProviderFactories()
		seedTestStoragePool(poolStore)
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "cased-vg", "PT1H", "P7D")

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_volume_group" "test" {
  name            = "CASED-VG"
  storage_pool_id = %[1]q
}
`, testStoragePoolIDForVG()),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_volume_group.test", "name", "CASED-VG"),
						resource.TestCheckResourceAttrSet("data.everpure_azure_volume_group.test", "id"),
					),
				},
			},
		})
	})
}
