// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azcorefake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	psbfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock/fake"
	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/config"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

const testStoragePoolName = "pool1"

func testStoragePoolIDForVG() string {
	return armids.StoragePoolID{
		ResourceGroupID: armids.ResourceGroupID{
			SubscriptionID:    testSubscriptionID,
			ResourceGroupName: testResourceGroup,
		},
		StoragePoolName: testStoragePoolName,
	}.FullID()
}

// newTestVolumeGroupTransport builds fresh fake stores for storage pools, volume
// groups and recoverable volume groups, wires them behind one fake.ServerFactory
// transport, and seeds a storage pool into the pool store — a volume group's Create
// GETs its parent pool, so every test needs one to already exist, exactly as if it
// had been created outside this test run (imported or managed elsewhere).
func newTestVolumeGroupTransport() (*fakeStoragePoolStore, *fakeVolumeGroupStore, *arm.ClientOptions) {
	poolStore := newFakeStoragePoolStore(testSubscriptionID)
	vgStore := newFakeVolumeGroupStore(testSubscriptionID)
	seedTestStoragePool(poolStore)

	// The provider's Clients hands every client the same *arm.ClientOptions
	// (see Configure in provider.go), so one shared transport has to dispatch
	// by client type — fake.ServerFactoryTransport does exactly that. It is rebuilt
	// after a failed call, which the generated poller tracker needs to forget it.
	transport := &selfHealingTransport{build: func() policy.Transporter {
		return psbfake.NewServerFactoryTransport(&psbfake.ServerFactory{
			StoragePoolsServer:            poolStore.newStoragePoolsServer(),
			VolumeGroupsServer:            vgStore.newVolumeGroupsServer(),
			RecoverableVolumeGroupsServer: vgStore.newRecoverableVolumeGroupsServer(),
		})
	}}

	return poolStore, vgStore, &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: transport}}
}

// newTestVolumeGroupProviderFactories wraps newTestVolumeGroupTransport's fakes in an
// isolated provider instance, for tests driving the resource through terraform.
func newTestVolumeGroupProviderFactories() (
	map[string]func() (tfprotov6.ProviderServer, error), *fakeStoragePoolStore, *fakeVolumeGroupStore,
) {
	poolStore, vgStore, opts := newTestVolumeGroupTransport()
	p := newProvider("test", fixedCredentialFactory(&azcorefake.TokenCredential{}), opts)()

	factories := map[string]func() (tfprotov6.ProviderServer, error){
		"everpure": providerserver.NewProtocol6WithError(p),
	}

	return factories, poolStore, vgStore
}

// seedTestStoragePool inserts a minimal storage pool directly into the fake store, so
// volume group tests don't have to also drive a full pool create through terraform.
func seedTestStoragePool(store *fakeStoragePoolStore) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.pools[poolKey(testResourceGroup, testStoragePoolName)] = &armpurestorageblock.StoragePool{
		ID:       to.Ptr(testStoragePoolIDForVG()),
		Name:     to.Ptr(testStoragePoolName),
		Location: to.Ptr("eastus2"),
		Properties: &armpurestorageblock.StoragePoolProperties{
			DataRetentionPeriod: to.Ptr(int64(3600)),
		},
	}
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return tm
}

func TestVolumeGroupResource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	t.Run("creates a minimal (blank) volume group", func(t *testing.T) {
		factories, _, _ := newTestVolumeGroupProviderFactories()

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name            = "vg1"
  storage_pool_id = %[1]q
}
`, testStoragePoolIDForVG())

		unprotected := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "name", "vg1"),
						resource.TestCheckResourceAttrSet("everpure_azure_volume_group.test", "id"),
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "source_type", "none"),
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "deletion_protection", "true"),
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "eradicate_on_destroy", "false"),
						resource.TestCheckNoResourceAttr("everpure_azure_volume_group.test", "bandwidth_limit_in_mbps"),
						resource.TestCheckNoResourceAttr("everpure_azure_volume_group.test", "iops_limit"),
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "snapshot_protection_policy.#", "0"),
					),
				},
				// deletion_protection defaults to true; unprotect so the test
				// harness's own post-test destroy can succeed.
				{Config: unprotected},
			},
		})
	})

	t.Run("creates a fully configured volume group and preserves source attributes across Read", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()

		// Seed a source volume group so source_volume_group_id refers to something
		// real; the fake doesn't validate it, but the ID shape must be valid.
		sourceVGID := fmt.Sprintf(
			"/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/storagePools/%s/volumeGroups/source-vg",
			testSubscriptionID, testResourceGroup, testStoragePoolName,
		)

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                    = "vg1"
  storage_pool_id         = %[1]q
  deletion_protection     = false
  source_type             = "volumeGroup"
  source_volume_group_id  = %[2]q
  bandwidth_limit_in_mbps = 500
  iops_limit              = 1000

  snapshot_protection_policy {
    frequency = "PT1H"
    retention = "P7D"
  }

  tags = {
    environment = "test"
  }
}
`, testStoragePoolIDForVG(), sourceVGID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "source_type", "volumeGroup"),
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "source_volume_group_id", sourceVGID),
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "bandwidth_limit_in_mbps", "500"),
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "iops_limit", "1000"),
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "snapshot_protection_policy.0.frequency", "PT1H"),
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "snapshot_protection_policy.0.retention", "P7D"),
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "tags.environment", "test"),
						func(*terraform.State) error {
							vg, ok := vgStore.groups[vgKey(testResourceGroup, testStoragePoolName, "vg1")]
							if !ok {
								return errors.New("expected the fake to have a vg1 volume group")
							}
							if vg.Properties.SourceType == nil || *vg.Properties.SourceType != armpurestorageblock.VolumeGroupSourceTypeVolumeGroup {
								return fmt.Errorf("got sourceType %v, want volumeGroup", vg.Properties.SourceType)
							}
							if vg.Properties.SourceVolumeGroupResourceID == nil || *vg.Properties.SourceVolumeGroupResourceID != sourceVGID {
								return fmt.Errorf("got sourceVolumeGroupResourceId %v, want %s", vg.Properties.SourceVolumeGroupResourceID, sourceVGID)
							}
							return nil
						},
					),
				},
				{
					// Read must never overwrite source_type/source_volume_group_id:
					// the fake, like the real API, never returns them.
					Config:   config,
					PlanOnly: true,
				},
			},
		})
	})

	// The service authorizes a snapshot restore against the snapshot's parent volume
	// group and rejects a create that doesn't name it, so the provider derives that ID
	// from source_snapshot_id rather than asking for it a second time in configuration.
	t.Run("a snapshot source sends the snapshot's parent volume group alongside the snapshot", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()

		sourceVGID := fmt.Sprintf(
			"/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/storagePools/%s/volumeGroups/source-vg",
			testSubscriptionID, testResourceGroup, testStoragePoolName,
		)
		sourceSnapshotID := sourceVGID + "/snapshots/snap1"

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false
  source_type         = "snapshot"
  source_snapshot_id  = %[2]q
}
`, testStoragePoolIDForVG(), sourceSnapshotID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "source_snapshot_id", sourceSnapshotID),
						// The derived ID goes on the wire only: it is not an
						// attribute, so state keeps source_volume_group_id null.
						resource.TestCheckNoResourceAttr("everpure_azure_volume_group.test", "source_volume_group_id"),
						func(*terraform.State) error {
							vg, ok := vgStore.groups[vgKey(testResourceGroup, testStoragePoolName, "vg1")]
							if !ok {
								return errors.New("expected the fake to have a vg1 volume group")
							}
							if vg.Properties.SourceType == nil || *vg.Properties.SourceType != armpurestorageblock.VolumeGroupSourceTypeSnapshot {
								return fmt.Errorf("got sourceType %v, want snapshot", vg.Properties.SourceType)
							}
							if vg.Properties.SourceSnapshotResourceID == nil || *vg.Properties.SourceSnapshotResourceID != sourceSnapshotID {
								return fmt.Errorf("got sourceSnapshotResourceId %v, want %s", vg.Properties.SourceSnapshotResourceID, sourceSnapshotID)
							}
							if vg.Properties.SourceVolumeGroupResourceID == nil || *vg.Properties.SourceVolumeGroupResourceID != sourceVGID {
								return fmt.Errorf("got sourceVolumeGroupResourceId %v, want %s", vg.Properties.SourceVolumeGroupResourceID, sourceVGID)
							}
							return nil
						},
					),
				},
				{
					// The derived ID must not leak into state and provoke a diff:
					// the API never returns any of the source fields.
					Config:   config,
					PlanOnly: true,
				},
			},
		})
	})

	t.Run("accepts an unknown source_volume_group_id at plan time, the standard cross-resource reference pattern", func(t *testing.T) {
		factories, _, _ := newTestVolumeGroupProviderFactories()

		// "other" doesn't exist yet on the first plan, so its id is unknown when
		// "test"'s ValidateConfig runs — exactly the shape of
		// source_volume_group_id = everpure_azure_volume_group.other.id in the
		// example file, and it must not be rejected as "missing".
		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "other" {
  name                = "vg-other"
  storage_pool_id     = %[1]q
  deletion_protection = false
}

resource "everpure_azure_volume_group" "test" {
  name                    = "vg1"
  storage_pool_id         = %[1]q
  deletion_protection     = false
  source_type             = "volumeGroup"
  source_volume_group_id  = everpure_azure_volume_group.other.id
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttrSet("everpure_azure_volume_group.test", "source_volume_group_id"),
						resource.TestCheckResourceAttrPair(
							"everpure_azure_volume_group.test", "source_volume_group_id",
							"everpure_azure_volume_group.other", "id",
						),
					),
				},
			},
		})
	})

	t.Run("create succeeds when the RP stamps an extra tag onto the create response", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()
		vgStore.stampExtraTagOnCreate = true

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false

  tags = {
    environment = "test"
  }
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					// If Create mapped the response's tags into state instead of
					// keeping the plan's, this apply would fail with "Provider
					// produced inconsistent result after apply" as soon as the
					// fake's stamped tag showed up alongside the planned one.
					// The stamped tag genuinely exists server-side, though, so
					// the test harness's own post-apply refresh plan correctly
					// shows it as drift — Read's job, not Create's.
					Config:             config,
					ExpectNonEmptyPlan: true,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "tags.%", "1"),
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "tags.environment", "test"),
					),
				},
			},
		})
	})

	t.Run("updates tags, limits and snapshot_protection_policy in place", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()

		base := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                    = "vg1"
  storage_pool_id         = %[1]q
  deletion_protection     = false
  bandwidth_limit_in_mbps = 500

  tags = {
    environment = "test"
  }
}
`, testStoragePoolIDForVG())

		updated := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                    = "vg1"
  storage_pool_id         = %[1]q
  deletion_protection     = false
  bandwidth_limit_in_mbps = 1000
  iops_limit              = 2000

  snapshot_protection_policy {
    frequency = "PT1H"
    retention = "P7D"
  }

  tags = {
    environment = "prod"
  }
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: base},
				{
					Config: updated,
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction("everpure_azure_volume_group.test", plancheck.ResourceActionUpdate),
						},
					},
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "bandwidth_limit_in_mbps", "1000"),
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "iops_limit", "2000"),
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "snapshot_protection_policy.0.frequency", "PT1H"),
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "tags.environment", "prod"),
					),
				},
			},
		})

		if vgStore.lastUpdate == nil {
			t.Fatal("expected a captured update")
		}
		if vgStore.updateCallCount != 2 {
			t.Fatalf("got %d update calls, want 2: one properties PATCH and one tags-only PATCH", vgStore.updateCallCount)
		}
		if vgStore.lastUpdate.Properties == nil || vgStore.lastUpdate.Tags != nil {
			t.Fatalf("got last PATCH %+v, want a body with properties only", vgStore.lastUpdate)
		}
	})

	t.Run("changing only tags sends one tags-only PATCH and leaves a clean plan", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()

		cfg := func(env string) string {
			return providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false

  tags = {
    environment = %[2]q
  }
}
`, testStoragePoolIDForVG(), env)
		}

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: cfg("test")},
				{
					Config: cfg("prod"),
					Check:  resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "tags.environment", "prod"),
				},
			},
		})

		if vgStore.updateCallCount != 1 {
			t.Fatalf("got %d update calls, want exactly 1 for a tags-only change", vgStore.updateCallCount)
		}
		if vgStore.lastUpdate == nil || vgStore.lastUpdate.Properties != nil || len(vgStore.lastUpdate.Tags) != 1 {
			t.Fatalf("got PATCH %+v, want a body with tags only", vgStore.lastUpdate)
		}
	})

	t.Run("changing only deletion_protection or eradicate_on_destroy sends no PATCH", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()

		base := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                    = "vg1"
  storage_pool_id         = %[1]q
  deletion_protection     = true
  eradicate_on_destroy    = false
  bandwidth_limit_in_mbps = 500

  tags = {
    environment = "test"
  }
}
`, testStoragePoolIDForVG())

		flagsChanged := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                    = "vg1"
  storage_pool_id         = %[1]q
  deletion_protection     = false
  eradicate_on_destroy    = true
  bandwidth_limit_in_mbps = 500

  tags = {
    environment = "test"
  }
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: base},
				{
					Config: flagsChanged,
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction("everpure_azure_volume_group.test", plancheck.ResourceActionUpdate),
						},
					},
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "deletion_protection", "false"),
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "eradicate_on_destroy", "true"),
					),
				},
			},
		})

		if vgStore.updateCallCount != 0 {
			t.Fatalf("got %d update calls, want 0: flipping only provider-only attributes must send no PATCH", vgStore.updateCallCount)
		}
	})

	t.Run("changing only timeouts sends no PATCH", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()

		base := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                    = "vg1"
  storage_pool_id         = %[1]q
  deletion_protection     = false
  bandwidth_limit_in_mbps = 500
}
`, testStoragePoolIDForVG())

		timeoutsChanged := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                    = "vg1"
  storage_pool_id         = %[1]q
  deletion_protection     = false
  bandwidth_limit_in_mbps = 500

  timeouts {
    update = "45m"
  }
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: base},
				{
					Config: timeoutsChanged,
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction("everpure_azure_volume_group.test", plancheck.ResourceActionUpdate),
						},
					},
					Check: resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "timeouts.update", "45m"),
				},
			},
		})

		if vgStore.updateCallCount != 0 {
			t.Fatalf("got %d update calls, want 0: a timeouts-only change is provider-only config and must send no PATCH", vgStore.updateCallCount)
		}
	})

	t.Run("removing snapshot_protection_policy PATCHes an explicit empty object", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()

		withPolicy := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false

  snapshot_protection_policy {
    frequency = "PT1H"
    retention = "P7D"
  }
}
`, testStoragePoolIDForVG())

		withoutPolicy := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: withPolicy},
				{
					Config: withoutPolicy,
					Check:  resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "snapshot_protection_policy.#", "0"),
				},
			},
		})

		if vgStore.lastUpdate == nil || vgStore.lastUpdate.Properties == nil {
			t.Fatal("expected a captured update with properties")
		}
		pp := vgStore.lastUpdate.Properties.ProtectionParameters
		if pp == nil {
			t.Fatal("expected a non-nil ProtectionParameters in the PATCH body")
		}
		if pp.Frequency != nil || pp.Retention != nil {
			t.Fatalf("got %+v, want both fields nil (an explicit empty object)", pp)
		}
	})

	t.Run("removing a set limit sends an explicit clear and reads back as absent", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()

		withLimit := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                    = "vg1"
  storage_pool_id         = %[1]q
  deletion_protection     = false
  bandwidth_limit_in_mbps = 500
}
`, testStoragePoolIDForVG())

		withoutLimit := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: withLimit},
				{
					Config: withoutLimit,
					Check:  resource.TestCheckNoResourceAttr("everpure_azure_volume_group.test", "bandwidth_limit_in_mbps"),
				},
			},
		})

		if vgStore.lastUpdate == nil || vgStore.lastUpdate.Properties == nil || vgStore.lastUpdate.Properties.PerformanceParameters == nil {
			t.Fatal("expected a captured update with performance parameters")
		}
		bw := vgStore.lastUpdate.Properties.PerformanceParameters.BandwidthLimitMbPerSec
		if bw == nil || *bw != 0 {
			t.Fatalf("BandwidthLimitMbPerSec = %v, want ptr to 0 (explicit clear)", bw)
		}
	})

	t.Run("name change forces replacement", func(t *testing.T) {
		factories, _, _ := newTestVolumeGroupProviderFactories()

		configWithName := func(name string) string {
			return providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = %[1]q
  storage_pool_id     = %[2]q
  deletion_protection = false
}
`, name, testStoragePoolIDForVG())
		}

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: configWithName("vg1")},
				{
					Config: configWithName("vg2"),
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction("everpure_azure_volume_group.test", plancheck.ResourceActionReplace),
						},
					},
				},
			},
		})
	})

	t.Run("source_type change forces replacement", func(t *testing.T) {
		factories, _, _ := newTestVolumeGroupProviderFactories()
		sourceSnapshotID := fmt.Sprintf(
			"/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/storagePools/%s/volumeGroups/vg0/snapshots/snap1",
			testSubscriptionID, testResourceGroup, testStoragePoolName,
		)

		none := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false
}
`, testStoragePoolIDForVG())

		fromSnapshot := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false
  source_type         = "snapshot"
  source_snapshot_id  = %[2]q
}
`, testStoragePoolIDForVG(), sourceSnapshotID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: none},
				{
					Config: fromSnapshot,
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction("everpure_azure_volume_group.test", plancheck.ResourceActionReplace),
						},
					},
				},
			},
		})
	})

	t.Run("blocks a destroy plan while deletion_protection is true", func(t *testing.T) {
		factories, _, _ := newTestVolumeGroupProviderFactories()

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name            = "vg1"
  storage_pool_id = %[1]q
}
`, testStoragePoolIDForVG())

		unprotected := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: config},
				{
					// An empty config (besides the provider block) removes the
					// resource, which plans a destroy.
					Config:      providerBlock(),
					ExpectError: deletionProtectedRegexp(kindVolumeGroup),
				},
				// Unprotect so the test harness's own post-test destroy, which
				// would otherwise hit the same block, can succeed.
				{Config: unprotected},
			},
		})
	})

	t.Run("blocks a replace plan while deletion_protection is true", func(t *testing.T) {
		factories, _, _ := newTestVolumeGroupProviderFactories()

		configWithName := func(name string) string {
			return providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name            = %[1]q
  storage_pool_id = %[2]q
}
`, name, testStoragePoolIDForVG())
		}
		unprotectedConfigWithName := func(name string) string {
			return providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = %[1]q
  storage_pool_id     = %[2]q
  deletion_protection = false
}
`, name, testStoragePoolIDForVG())
		}

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: configWithName("vg1")},
				{
					// The error names the attribute whose change triggered the
					// replacement ("name" here), so two immutable attributes
					// changing at once produces two distinct messages rather
					// than identical duplicates.
					Config:      configWithName("vg2"),
					ExpectError: deletionProtectedReplaceRegexp(kindVolumeGroup, "name"),
				},
				// Unprotect the still-vg1 resource so the test harness's own
				// post-test destroy, which would otherwise hit the same block,
				// can succeed.
				{Config: unprotectedConfigWithName("vg1")},
			},
		})
	})

	t.Run("blocks a replace plan for a storage_pool_id change while deletion_protection is true", func(t *testing.T) {
		factories, poolStore, _ := newTestVolumeGroupProviderFactories()

		// A second pool, so the config below can move the volume group onto it —
		// storage_pool_id is immutable, so that alone forces a replace, isolating
		// which attribute's own guard fires (unlike source_type, whose change is
		// always coupled to one of the source_*_id attributes also changing).
		const otherPoolName = "pool2"
		otherPoolID := armids.StoragePoolID{
			ResourceGroupID: armids.ResourceGroupID{
				SubscriptionID:    testSubscriptionID,
				ResourceGroupName: testResourceGroup,
			},
			StoragePoolName: otherPoolName,
		}.FullID()
		poolStore.mu.Lock()
		poolStore.pools[poolKey(testResourceGroup, otherPoolName)] = &armpurestorageblock.StoragePool{
			ID:       to.Ptr(otherPoolID),
			Name:     to.Ptr(otherPoolName),
			Location: to.Ptr("eastus2"),
			Properties: &armpurestorageblock.StoragePoolProperties{
				DataRetentionPeriod: to.Ptr(int64(3600)),
			},
		}
		poolStore.mu.Unlock()

		configWithPool := func(poolID string) string {
			return providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name            = "vg1"
  storage_pool_id = %[1]q
}
`, poolID)
		}
		unprotectedConfigWithPool := func(poolID string) string {
			return providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false
}
`, poolID)
		}

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: configWithPool(testStoragePoolIDForVG())},
				{
					// The error must name "storage_pool_id", the attribute
					// whose change triggered the replacement.
					Config:      configWithPool(otherPoolID),
					ExpectError: deletionProtectedReplaceRegexp(kindVolumeGroup, "storage_pool_id"),
				},
				// Unprotect the still-on-the-original-pool "vg1" so the test
				// harness's own post-test destroy, which would otherwise hit
				// the same block, can succeed.
				{Config: unprotectedConfigWithPool(testStoragePoolIDForVG())},
			},
		})
	})

	t.Run("destroys with deletion_protection = false and eradicate_on_destroy = false, leaving the twin untouched", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                 = "vg1"
  storage_pool_id      = %[1]q
  deletion_protection  = false
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: config},
			},
		})

		twins := vgStore.recoverable[vgPoolKey(testResourceGroup, testStoragePoolName)]
		if len(twins) != 1 {
			t.Fatalf("got %d recoverable twins, want 1 (soft delete, not eradicated)", len(twins))
		}
	})

	t.Run("destroys with eradicate_on_destroy = true and eradicates only the twin this delete just created, ignoring decoys", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()

		// Decoys, both already present before this test's delete ever runs, so
		// set-difference selection excludes them regardless of name or age: one
		// under a different name, and one that already shares vg1's own
		// previousName (e.g. from an earlier delete/recreate cycle).
		vgStore.seedRecoverableTwin(testResourceGroup, testStoragePoolName, "other-vg-20260101000000", "other-vg", mustParseTime(t, "2026-01-01T00:00:00Z"))
		vgStore.seedRecoverableTwin(testResourceGroup, testStoragePoolName, "vg1-20260101000000", "vg1", mustParseTime(t, "2026-01-01T00:00:00Z"))

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                  = "vg1"
  storage_pool_id       = %[1]q
  deletion_protection   = false
  eradicate_on_destroy  = true
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: config},
			},
		})

		twins := vgStore.recoverable[vgPoolKey(testResourceGroup, testStoragePoolName)]
		if len(twins) != 2 {
			t.Fatalf("got %d recoverable twins, want 2 (both decoys; the freshly created twin was eradicated)", len(twins))
		}
		for _, twin := range twins {
			if twin.Name == nil {
				t.Fatal("expected twin.Name to be set")
			}
			if *twin.Name != "other-vg-20260101000000" && *twin.Name != "vg1-20260101000000" {
				t.Fatalf("unexpected twin remaining: %s", *twin.Name)
			}
		}
	})

	t.Run("destroys with eradicate_on_destroy = true and warns, not errors, when no twin matches", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()
		// Simulates auto-eradication (or someone else) having already won the
		// race by the time this destroy's eradicate step looks for a twin.
		vgStore.skipTwinOnDelete = true

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                  = "vg1"
  storage_pool_id       = %[1]q
  deletion_protection   = false
  eradicate_on_destroy  = true
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: config},
				{
					// Removing the resource from config plans and applies a
					// destroy; if the missing twin surfaced as a hard error
					// instead of a warning, this step would fail.
					Config: providerBlock(),
				},
			},
		})
	})

	t.Run("imports a volume group by its full ARM ID", func(t *testing.T) {
		factories, _, _ := newTestVolumeGroupProviderFactories()

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name            = "vg1"
  storage_pool_id = %[1]q
}
`, testStoragePoolIDForVG())

		unprotected := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: config},
				{
					// Import resets deletion_protection to its own default
					// (true), regardless of what a prior instance had — it has
					// no API-side memory of it — so this step's implicit
					// pre-import apply and the import itself both keep the
					// default, letting ImportStateVerify compare like for like.
					Config:            config,
					ResourceName:      "everpure_azure_volume_group.test",
					ImportState:       true,
					ImportStateVerify: true,
					ImportStateId: fmt.Sprintf(
						"/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/storagePools/%s/volumeGroups/vg1",
						testSubscriptionID, testResourceGroup, testStoragePoolName,
					),
				},
				// Unprotect so the test harness's own post-test destroy can succeed.
				{Config: unprotected},
			},
		})
	})

	// Import-shaped: ImportState composes storage_pool_id via
	// parsed.StoragePool().FullID(), which always renders ARM's canonical keyword
	// segment casing, regardless of what casing the import ID itself used. So a
	// config that spells storage_pool_id with different (but ARM-equivalent) keyword
	// casing than the canonical form must still plan no changes after import — a
	// missing plan modifier here would show a spurious replace on the very first
	// plan. Only an import-shaped test catches this: an apply-then-plan test can't,
	// because by then state already holds the config's own spelling.
	t.Run("an imported volume group plans no changes when config spells storage_pool_id with different casing", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()

		// Nothing here ever applied this volume group through Terraform: it's seeded
		// straight into the store, already carrying no protection policy, exactly as
		// if it had been created outside Terraform before this import.
		vgStore.mu.Lock()
		vgStore.groups[vgKey(testResourceGroup, testStoragePoolName, "vg1")] = &armpurestorageblock.VolumeGroup{
			ID:   to.Ptr(vgStore.fakeVGID(testResourceGroup, testStoragePoolName, "vg1")),
			Name: to.Ptr("vg1"),
			Properties: &armpurestorageblock.VolumeGroupProperties{
				ProvisioningState:     to.Ptr(armpurestorageblock.ProvisioningStateSucceeded),
				StoragePoolInternalID: to.Ptr("internal-" + testStoragePoolName),
				VolumeGroupInternalID: to.Ptr("internal-vg1"),
			},
		}
		vgStore.mu.Unlock()

		// The practitioner's own config spells storage_pool_id with different, but
		// ARM-equivalent, keyword casing; the import ID's own keyword segments are
		// non-canonically cased too, as composing one by hand from the same config
		// would naturally produce.
		mangledPoolID := fmt.Sprintf(
			"/Subscriptions/%s/ResourceGroups/%s/providers/purestorage.block/StoragePools/%s",
			testSubscriptionID, testResourceGroup, testStoragePoolName,
		)
		mangledVGID := fmt.Sprintf(
			"/Subscriptions/%s/ResourceGroups/%s/providers/purestorage.block/StoragePools/%s/VolumeGroups/vg1",
			testSubscriptionID, testResourceGroup, testStoragePoolName,
		)

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name            = "vg1"
  storage_pool_id = %[1]q
}
`, mangledPoolID)

		unprotected := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false
}
`, mangledPoolID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config:             config,
					ResourceName:       "everpure_azure_volume_group.test",
					ImportState:        true,
					ImportStatePersist: true,
					ImportStateVerify:  false,
					ImportStateId:      mangledVGID,
				},
				{
					// Expected behaviour: config and imported state spell
					// storage_pool_id differently but ARM-equivalently, so this
					// plans no changes.
					Config:   config,
					PlanOnly: true,
				},
				// Unprotect so the test harness's own post-test destroy can succeed;
				// import gives deletion_protection its schema default (true).
				{Config: unprotected},
			},
		})
	})

	t.Run("shows drift and plans an update when tags change out of band", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: config},
				{
					PreConfig: func() {
						vgStore.mutateVolumeGroup(testResourceGroup, testStoragePoolName, "vg1", func(vg *armpurestorageblock.VolumeGroup) {
							vg.Tags = map[string]*string{"drift": to.Ptr("unexpected")}
						})
					},
					Config:             config,
					PlanOnly:           true,
					ExpectNonEmptyPlan: true,
				},
			},
		})
	})

	t.Run("plans a recreate when the volume group has been deleted out of band", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: config},
				{
					PreConfig: func() {
						vgStore.removeVolumeGroup(testResourceGroup, testStoragePoolName, "vg1")
					},
					Config:             config,
					PlanOnly:           true,
					ExpectNonEmptyPlan: true,
				},
			},
		})
	})

	t.Run("a duration canonicalized differently by the API produces an empty plan", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false

  snapshot_protection_policy {
    frequency = "PT60M"
    retention = "P7D"
  }
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: config},
				{
					PreConfig: func() {
						// The API canonicalizes PT60M to PT1H on read.
						vgStore.mutateVolumeGroup(testResourceGroup, testStoragePoolName, "vg1", func(vg *armpurestorageblock.VolumeGroup) {
							vg.Properties.ProtectionParameters.Frequency = to.Ptr("PT1H")
						})
					},
					Config:   config,
					PlanOnly: true,
				},
			},
		})
	})

	t.Run("rejects invalid configuration at plan time", func(t *testing.T) {
		sourceSnapshotIDForTable := fmt.Sprintf(
			"/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/storagePools/%s/volumeGroups/vg1/snapshots/snap1",
			testSubscriptionID, testResourceGroup, testStoragePoolName,
		)
		sourceRecoverableVolumeGroupIDForTable := fmt.Sprintf(
			"/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/storagePools/%s/recoverableVolumeGroups/rvg1",
			testSubscriptionID, testResourceGroup, testStoragePoolName,
		)

		tests := []struct {
			name          string
			resourceBlock string
			wantErr       string
		}{
			{
				name: "name too short",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name            = "a"
  storage_pool_id = %[1]q
}
`, testStoragePoolIDForVG()),
				wantErr: "2-40 characters",
			},
			{
				name: "malformed storage_pool_id",
				resourceBlock: `
resource "everpure_azure_volume_group" "test" {
  name            = "vg1"
  storage_pool_id = "not-a-resource-id"
}
`,
				wantErr: "PureStorage.Block/storagePools",
			},
			{
				name: "bandwidth_limit_in_mbps of 0 is rejected",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                    = "vg1"
  storage_pool_id         = %[1]q
  bandwidth_limit_in_mbps = 0
}
`, testStoragePoolIDForVG()),
				wantErr: "at least 1",
			},
			{
				name: "iops_limit of 99 is rejected",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name            = "vg1"
  storage_pool_id = %[1]q
  iops_limit      = 99
}
`, testStoragePoolIDForVG()),
				wantErr: "at least 100",
			},
			{
				name: "P1W duration is rejected",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name            = "vg1"
  storage_pool_id = %[1]q

  snapshot_protection_policy {
    frequency = "P1W"
    retention = "P7D"
  }
}
`, testStoragePoolIDForVG()),
				wantErr: `days\s+or\s+smaller`,
			},
			{
				name: "source_volume_group_id set while source_type is none",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                    = "vg1"
  storage_pool_id         = %[1]q
  source_volume_group_id  = %[1]q
}
`, testStoragePoolIDForVG()),
				wantErr: "must not be set",
			},
			{
				name: "source_type = volumeGroup without source_volume_group_id",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name            = "vg1"
  storage_pool_id = %[1]q
  source_type     = "volumeGroup"
}
`, testStoragePoolIDForVG()),
				wantErr: "is required",
			},
			{
				name: "invalid source_type",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name            = "vg1"
  storage_pool_id = %[1]q
  source_type     = "bogus"
}
`, testStoragePoolIDForVG()),
				wantErr: "value must be one of",
			},
			{
				name: "source_type = snapshot without source_snapshot_id",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name            = "vg1"
  storage_pool_id = %[1]q
  source_type     = "snapshot"
}
`, testStoragePoolIDForVG()),
				wantErr: "is required",
			},
			{
				name: "source_snapshot_id set while source_type is none",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  source_snapshot_id  = %[2]q
}
`, testStoragePoolIDForVG(), sourceSnapshotIDForTable),
				wantErr: "must not be set",
			},
			{
				name: "source_type = recoverableVolumeGroup without source_recoverable_volume_group_id",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name            = "vg1"
  storage_pool_id = %[1]q
  source_type     = "recoverableVolumeGroup"
}
`, testStoragePoolIDForVG()),
				wantErr: "is required",
			},
			{
				name: "source_recoverable_volume_group_id set while source_type is none",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                                = "vg1"
  storage_pool_id                     = %[1]q
  source_recoverable_volume_group_id  = %[2]q
}
`, testStoragePoolIDForVG(), sourceRecoverableVolumeGroupIDForTable),
				wantErr: "must not be set",
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				factories, _, _ := newTestVolumeGroupProviderFactories()
				resource.Test(t, resource.TestCase{
					IsUnitTest:               true,
					ProtoV6ProviderFactories: factories,
					Steps: []resource.TestStep{
						{
							Config:      providerBlock() + tt.resourceBlock,
							ExpectError: regexp.MustCompile(tt.wantErr),
						},
					},
				})
			})
		}
	})

	t.Run("an imported volume group plans no changes when config spells the durations differently", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()

		// Nothing here ever applied this volume group through Terraform: it's
		// seeded straight into the store, already carrying the API's canonical
		// spelling, exactly as if it had been created outside Terraform (or by an
		// earlier apply in a different working directory) before this import.
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg1", "PT1H", "P2D")

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name            = "vg1"
  storage_pool_id = %[1]q

  snapshot_protection_policy {
    frequency = "PT60M"
    retention = "PT48H"
  }
}
`, testStoragePoolIDForVG())

		unprotected := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false

  snapshot_protection_policy {
    frequency = "PT60M"
    retention = "PT48H"
  }
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config:             config,
					ResourceName:       "everpure_azure_volume_group.test",
					ImportState:        true,
					ImportStatePersist: true,
					ImportStateVerify:  false,
					ImportStateId: fmt.Sprintf(
						"/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/storagePools/%s/volumeGroups/vg1",
						testSubscriptionID, testResourceGroup, testStoragePoolName,
					),
				},
				{
					// Expected behaviour: config and imported state spell the same
					// duration differently but semantic equality should still see
					// them as equal, so this plans no changes.
					Config:   config,
					PlanOnly: true,
				},
				// Unprotect so the test harness's own post-test destroy can succeed;
				// import gives deletion_protection its schema default (true).
				{Config: unprotected},
			},
		})
	})
}

// TestVolumeGroupResourceDynamicSnapshotProtectionPolicyBlock is a regression test
// for a dynamic block whose for_each derives from a Terraform variable rather than a
// literal list: Terraform core then reports the whole snapshot_protection_policy list
// as unknown while validating the config, even before the for_each value itself has
// been resolved. That crashed every plan and validate against this pattern (see
// blockElements in block_elements.go) until
// snapshot_protection_policy became a types.List, which can represent "unknown".
// Every other test config in this file uses a literal block, so none of them would
// have caught this: the crash only fires when block generation goes through
// "dynamic". resource.Test always plans before it applies, so a config that merely
// applies successfully here is already proof plan didn't crash first.
func TestVolumeGroupResourceDynamicSnapshotProtectionPolicyBlock(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	cfg := providerBlock() + fmt.Sprintf(`
variable "policies" {
  type = list(object({
    frequency = string
    retention = string
  }))
  default = []
}

resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false

  dynamic "snapshot_protection_policy" {
    for_each = var.policies
    content {
      frequency = snapshot_protection_policy.value.frequency
      retention = snapshot_protection_policy.value.retention
    }
  }
}
`, testStoragePoolIDForVG())

	t.Run("one element: plan and apply succeed, the policy lands in state", func(t *testing.T) {
		factories, _, _ := newTestVolumeGroupProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: cfg,
					ConfigVariables: config.Variables{
						"policies": config.ListVariable(config.ObjectVariable(map[string]config.Variable{
							"frequency": config.StringVariable("PT1H"),
							"retention": config.StringVariable("P7D"),
						})),
					},
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "snapshot_protection_policy.#", "1"),
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "snapshot_protection_policy.0.frequency", "PT1H"),
						resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "snapshot_protection_policy.0.retention", "P7D"),
					),
				},
			},
		})
	})

	t.Run("empty list (the variable's default): applies clean with no block", func(t *testing.T) {
		factories, _, _ := newTestVolumeGroupProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: cfg,
					Check:  resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "snapshot_protection_policy.#", "0"),
				},
			},
		})
	})
}

// TestVolumeGroupImportStateNilClients exercises ImportState before Configure has
// ever run — e.g. an import block whose provider configuration itself isn't known
// yet (subscription_id or an authentication attribute still unresolved) — where
// r.clients is nil. Reading r.clients.subscriptionID unguarded would panic instead of
// returning a diagnostic.
func TestVolumeGroupImportStateNilClients(t *testing.T) {
	r := &VolumeGroupResource{}
	resp := &fwresource.ImportStateResponse{}

	r.ImportState(context.Background(), fwresource.ImportStateRequest{
		ID: fmt.Sprintf(
			"/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/storagePools/%s/volumeGroups/vg1",
			testSubscriptionID, testResourceGroup, testStoragePoolName,
		),
	}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic instead of a panic")
	}
	requireContains(t, resp.Diagnostics[0].Detail(), "provider configuration contains values that are not yet known")
}

// TestVolumeGroupUpdateNotFound drives VolumeGroupResource.Update directly
// (bypassing Terraform's own refresh-before-apply, which would normally catch an
// out-of-band delete during Read and plan a recreate instead of an update) to
// exercise the race where the volume group is deleted between the refresh that
// produced the plan and the PATCH that applies it.
func TestVolumeGroupUpdateNotFound(t *testing.T) {
	ctx := context.Background()

	_, vgStore, opts := newTestVolumeGroupTransport()

	vgClient, err := armpurestorageblock.NewVolumeGroupsClient(testSubscriptionID, &azcorefake.TokenCredential{}, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	poller, err := vgClient.BeginCreate(ctx, testResourceGroup, testStoragePoolName, "vg1", armpurestorageblock.VolumeGroup{
		Location: to.Ptr("eastus2"),
		Properties: &armpurestorageblock.VolumeGroupProperties{
			SourceType: to.Ptr(armpurestorageblock.VolumeGroupSourceTypeNone),
		},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	vgStore.removeVolumeGroup(testResourceGroup, testStoragePoolName, "vg1")

	r := &VolumeGroupResource{}
	var configureResp fwresource.ConfigureResponse
	r.Configure(ctx, fwresource.ConfigureRequest{ProviderData: &Clients{
		cred:           &azcorefake.TokenCredential{},
		opts:           opts,
		subscriptionID: testSubscriptionID,
	}}, &configureResp)
	if configureResp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", configureResp.Diagnostics)
	}

	var schemaResp fwresource.SchemaResponse
	r.Schema(ctx, fwresource.SchemaRequest{}, &schemaResp)

	planData := VolumeGroupResourceModel{
		ID:                             armids.IDValue{StringValue: types.StringValue(vgStore.fakeVGID(testResourceGroup, testStoragePoolName, "vg1"))},
		Name:                           types.StringValue("vg1"),
		StoragePoolID:                  armids.IDValue{StringValue: types.StringValue(testStoragePoolIDForVG())},
		SourceType:                     types.StringValue(volumeGroupSourceTypeNone),
		SourceVolumeGroupID:            armids.IDValue{StringValue: types.StringNull()},
		SourceSnapshotID:               armids.IDValue{StringValue: types.StringNull()},
		SourceRecoverableVolumeGroupID: armids.IDValue{StringValue: types.StringNull()},
		// Differs from the actual created state (null, see BeginCreate above)
		// so this plan carries a real API-backed change: otherwise Update's own
		// no-op skip (plan == state on every API-backed attribute) would fire
		// before ever reaching the BeginUpdate call this test means to exercise.
		BandwidthLimitInMbps:     types.Int64Value(500),
		IopsLimit:                types.Int64Null(),
		Tags:                     types.MapNull(types.StringType),
		DeletionProtection:       types.BoolValue(false),
		EradicateOnDestroy:       types.BoolValue(false),
		InternalID:               types.StringValue("internal-vg1"),
		SnapshotProtectionPolicy: types.ListNull(snapshotProtectionPolicyObjectType),
		Timeouts: timeouts.Value{Object: types.ObjectNull(map[string]attr.Type{
			"create": types.StringType,
			"read":   types.StringType,
			"update": types.StringType,
			"delete": types.StringType,
		})},
	}

	plan := tfsdk.Plan{Schema: schemaResp.Schema}
	if diags := plan.Set(ctx, &planData); diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}

	// The prior state, as it was actually created above (no bandwidth limit):
	// Update now reads req.State to decide whether it can skip the PATCH, so
	// it must be populated here just like the real protocol always populates
	// it from the prior apply.
	stateData := planData
	stateData.BandwidthLimitInMbps = types.Int64Null()
	state := tfsdk.State{Schema: schemaResp.Schema}
	if diags := state.Set(ctx, &stateData); diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}

	updateResp := &fwresource.UpdateResponse{State: tfsdk.State{Schema: schemaResp.Schema, Raw: plan.Raw}}
	r.Update(ctx, fwresource.UpdateRequest{Plan: plan, State: state}, updateResp)

	// Update must not remove the resource from state on a 404 (that would make
	// Terraform Core itself report a provider-bug-shaped "Provider produced
	// inconsistent result after apply"); it must report an explicit error instead.
	if !updateResp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic")
	}
	requireContains(t, updateResp.Diagnostics[0].Detail(), "deleted outside Terraform")
}

// TestVolumeGroupCreateMissingSourceAttributeRecheck drives VolumeGroupResource.Create
// directly (the same hand-built-request approach as TestVolumeGroupUpdateNotFound) to
// exercise Create's own recheck of missingSourceAttribute, not just the helper
// function in isolation. The scenario it guards: source_volume_group_id is unknown
// when ValidateConfig runs — a reference to another resource's id, not yet created,
// the standard create-from-source pattern — so ValidateConfig lets it through; by the
// time Create reads the plan that value has resolved, and here it resolved to null.
// Without Create's own recheck, toVolumeGroup would send the API an empty-string
// resource ID instead of this attribute error.
func TestVolumeGroupCreateMissingSourceAttributeRecheck(t *testing.T) {
	ctx := context.Background()

	_, _, opts := newTestVolumeGroupTransport()

	r := &VolumeGroupResource{}
	var configureResp fwresource.ConfigureResponse
	r.Configure(ctx, fwresource.ConfigureRequest{ProviderData: &Clients{
		cred:           &azcorefake.TokenCredential{},
		opts:           opts,
		subscriptionID: testSubscriptionID,
	}}, &configureResp)
	if configureResp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", configureResp.Diagnostics)
	}

	var schemaResp fwresource.SchemaResponse
	r.Schema(ctx, fwresource.SchemaRequest{}, &schemaResp)

	planData := VolumeGroupResourceModel{
		ID:                             armids.IDValue{StringValue: types.StringUnknown()},
		Name:                           types.StringValue("vg1"),
		StoragePoolID:                  armids.IDValue{StringValue: types.StringValue(testStoragePoolIDForVG())},
		SourceType:                     types.StringValue(volumeGroupSourceTypeVolumeGroup),
		SourceVolumeGroupID:            armids.IDValue{StringValue: types.StringNull()},
		SourceSnapshotID:               armids.IDValue{StringValue: types.StringNull()},
		SourceRecoverableVolumeGroupID: armids.IDValue{StringValue: types.StringNull()},
		BandwidthLimitInMbps:           types.Int64Null(),
		IopsLimit:                      types.Int64Null(),
		Tags:                           types.MapNull(types.StringType),
		DeletionProtection:             types.BoolValue(true),
		EradicateOnDestroy:             types.BoolValue(false),
		InternalID:                     types.StringUnknown(),
		SnapshotProtectionPolicy:       types.ListNull(snapshotProtectionPolicyObjectType),
		Timeouts: timeouts.Value{Object: types.ObjectNull(map[string]attr.Type{
			"create": types.StringType,
			"read":   types.StringType,
			"update": types.StringType,
			"delete": types.StringType,
		})},
	}

	plan := tfsdk.Plan{Schema: schemaResp.Schema}
	if diags := plan.Set(ctx, &planData); diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}

	createResp := &fwresource.CreateResponse{}
	r.Create(ctx, fwresource.CreateRequest{Plan: plan}, createResp)

	if !createResp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic")
	}
	requireContains(t, createResp.Diagnostics[0].Detail(),
		`source_volume_group_id is required when source_type = "volumeGroup"`)
}

// TestVolumeGroupResourceImportOtherSubscription covers the subscription guard
// on import. The provider's clients are pinned to its configured subscription,
// so importing an ID from another one would otherwise surface later as a
// misleading "not found" from ARM rather than an actionable error.
func TestVolumeGroupResourceImportOtherSubscription(t *testing.T) {
	factories, _, _ := newTestVolumeGroupProviderFactories()

	config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false
}
`, testStoragePoolIDForVG())

	importID := fmt.Sprintf(
		"/subscriptions/22222222-2222-2222-2222-222222222222/resourceGroups/%s/providers/"+
			"PureStorage.Block/storagePools/%s/volumeGroups/vg1",
		testResourceGroup, testStoragePoolName,
	)
	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{Config: config},
			{
				Config:        config,
				ResourceName:  "everpure_azure_volume_group.test",
				ImportState:   true,
				ImportStateId: importID,
				ExpectError:   subscriptionMismatchRegexp(kindVolumeGroup, importID),
			},
		},
	})
}

// newTestVolumeGroupDeleteResource builds a VolumeGroupResource wired to fresh fake
// stores through a direct Configure call (bypassing the provider server entirely), for
// tests that drive Delete directly rather than through terraform-plugin-testing. Calling
// Delete directly is what lets these tests seed a live volume group whose stored Name
// deliberately differs in case from the map key it's looked up by, and control exactly
// which single call fails, without a full plan/apply's own refreshes getting in the way.
func newTestVolumeGroupDeleteResource(t *testing.T) (*VolumeGroupResource, *fakeVolumeGroupStore) {
	t.Helper()

	_, vgStore, opts := newTestVolumeGroupTransport()

	r := &VolumeGroupResource{}
	var configureResp fwresource.ConfigureResponse
	r.Configure(context.Background(), fwresource.ConfigureRequest{ProviderData: &Clients{
		cred:           &azcorefake.TokenCredential{},
		opts:           opts,
		subscriptionID: testSubscriptionID,
	}}, &configureResp)
	if configureResp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", configureResp.Diagnostics)
	}

	return r, vgStore
}

// newTestVolumeGroupDeleteState builds a tfsdk.State for VolumeGroupResource.Delete,
// naming volume group "vg1" on the shared test storage pool, with deletion_protection
// false and eradicate_on_destroy true (every test using it wants to reach the
// eradication path), and applying overrides on top.
func newTestVolumeGroupDeleteState(t *testing.T, r *VolumeGroupResource, overrides func(*VolumeGroupResourceModel)) tfsdk.State {
	t.Helper()

	ctx := context.Background()
	var schemaResp fwresource.SchemaResponse
	r.Schema(ctx, fwresource.SchemaRequest{}, &schemaResp)

	data := VolumeGroupResourceModel{
		ID:                             armids.IDValue{StringValue: types.StringValue(testStoragePoolIDForVG() + "/volumeGroups/vg1")},
		Name:                           types.StringValue("vg1"),
		StoragePoolID:                  armids.IDValue{StringValue: types.StringValue(testStoragePoolIDForVG())},
		SourceType:                     types.StringValue(volumeGroupSourceTypeNone),
		SourceVolumeGroupID:            armids.IDValue{StringValue: types.StringNull()},
		SourceSnapshotID:               armids.IDValue{StringValue: types.StringNull()},
		SourceRecoverableVolumeGroupID: armids.IDValue{StringValue: types.StringNull()},
		BandwidthLimitInMbps:           types.Int64Null(),
		IopsLimit:                      types.Int64Null(),
		Tags:                           types.MapNull(types.StringType),
		DeletionProtection:             types.BoolValue(false),
		EradicateOnDestroy:             types.BoolValue(true),
		InternalID:                     types.StringValue("internal-vg1"),
		SnapshotProtectionPolicy:       types.ListNull(snapshotProtectionPolicyObjectType),
		Timeouts: timeouts.Value{Object: types.ObjectNull(map[string]attr.Type{
			"create": types.StringType,
			"read":   types.StringType,
			"update": types.StringType,
			"delete": types.StringType,
		})},
	}
	if overrides != nil {
		overrides(&data)
	}

	state := tfsdk.State{Schema: schemaResp.Schema}
	if diags := state.Set(ctx, &data); diags.HasError() {
		t.Fatalf("unexpected diagnostics building state: %v", diags)
	}
	return state
}

// TestVolumeGroupDeleteEradicatesDespiteCaseDivergence proves the fix for the bug where
// eradicate_on_destroy silently skipped eradication whenever state's copy of the name
// differed in letter case from the server's own stored spelling (e.g. after importing
// with a different case than the server actually holds — Read never corrects it). The
// seeded entry's stored Name ("Vg1") deliberately differs in case from both the map key
// it's looked up by and from state's own name (both "vg1"); Delete's pre-delete GET
// (added by this fix) learns "Vg1" from the seeded entry, and the fake's BeginDelete
// (fixed alongside it, see fakeVolumeGroupStore's own comment) renames using that same
// stored spelling on soft delete, exactly like a real backend would.
func TestVolumeGroupDeleteEradicatesDespiteCaseDivergence(t *testing.T) {
	r, vgStore := newTestVolumeGroupDeleteResource(t)

	key := vgKey(testResourceGroup, testStoragePoolName, "vg1")
	vgStore.groups[key] = &armpurestorageblock.VolumeGroup{
		ID:   to.Ptr(vgStore.fakeVGID(testResourceGroup, testStoragePoolName, "Vg1")),
		Name: to.Ptr("Vg1"),
		Properties: &armpurestorageblock.VolumeGroupProperties{
			ProvisioningState:     to.Ptr(armpurestorageblock.ProvisioningStateSucceeded),
			StoragePoolInternalID: to.Ptr("internal-" + testStoragePoolName),
			VolumeGroupInternalID: to.Ptr("internal-vg1"),
		},
	}

	state := newTestVolumeGroupDeleteState(t, r, func(data *VolumeGroupResourceModel) {
		data.Name = types.StringValue("vg1")
	})

	deleteResp := &fwresource.DeleteResponse{}
	r.Delete(context.Background(), fwresource.DeleteRequest{State: state}, deleteResp)

	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", deleteResp.Diagnostics)
	}
	if len(deleteResp.Diagnostics) != 0 {
		t.Fatalf("expected no diagnostics at all (a warning would mean eradication was skipped), got %v", deleteResp.Diagnostics)
	}

	twins := vgStore.recoverable[vgPoolKey(testResourceGroup, testStoragePoolName)]
	if len(twins) != 0 {
		t.Fatalf("got %d recoverable twins left, want 0 (the twin must have been eradicated): %+v", len(twins), twins)
	}
}

// TestVolumeGroupDeletePreGetFailureFallsBackAndWarns proves that a failure on the
// pre-delete GET this fix adds (e.g. a transient error, nothing to do with whether the
// volume group actually exists) doesn't abort the destroy: it falls back to state's own
// name for the eventual match, and if that fallback still can't find a twin (simulated
// here with skipTwinOnDelete, standing in for auto-eradication having already won the
// race), the outcome is the same pre-existing warning as any other unmatched twin, not
// an error.
func TestVolumeGroupDeletePreGetFailureFallsBackAndWarns(t *testing.T) {
	r, vgStore := newTestVolumeGroupDeleteResource(t)

	vgStore.groups[vgKey(testResourceGroup, testStoragePoolName, "vg1")] = &armpurestorageblock.VolumeGroup{
		ID:   to.Ptr(vgStore.fakeVGID(testResourceGroup, testStoragePoolName, "vg1")),
		Name: to.Ptr("vg1"),
		Properties: &armpurestorageblock.VolumeGroupProperties{
			ProvisioningState:     to.Ptr(armpurestorageblock.ProvisioningStateSucceeded),
			StoragePoolInternalID: to.Ptr("internal-" + testStoragePoolName),
			VolumeGroupInternalID: to.Ptr("internal-vg1"),
		},
	}
	vgStore.failGetOnce = true
	vgStore.skipTwinOnDelete = true

	state := newTestVolumeGroupDeleteState(t, r, nil)

	deleteResp := &fwresource.DeleteResponse{}
	r.Delete(context.Background(), fwresource.DeleteRequest{State: state}, deleteResp)

	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("expected no error diagnostics (a failed pre-delete GET must not abort the destroy), got %v", deleteResp.Diagnostics)
	}
	if len(deleteResp.Diagnostics) != 1 {
		t.Fatalf("expected exactly one warning, got %v", deleteResp.Diagnostics)
	}
	want := recoverableVolumeGroupNotFoundWarning("vg1")
	if !deleteResp.Diagnostics.Contains(want) {
		t.Fatalf("diagnostics = %v, want %v", deleteResp.Diagnostics, want)
	}

	// The destroy itself must still have completed despite the failed GET.
	if _, ok := vgStore.groups[vgKey(testResourceGroup, testStoragePoolName, "vg1")]; ok {
		t.Fatal("expected the volume group to be gone from the live store")
	}
}

// TestVolumeGroupFailedCreateReplacementIsNotBlockedByDeletionProtection covers a create whose
// poll fails after Azure finished creating the object: the tainted volume group survives refresh,
// so its replacement really runs Delete, which must not be refused by the default
// deletion_protection = true.
func TestVolumeGroupFailedCreateReplacementIsNotBlockedByDeletionProtection(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	factories, _, store := newTestVolumeGroupProviderFactories()
	store.failCreatePollKeepObject = true

	config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name            = "vg1"
  storage_pool_id = %q
}
`, testStoragePoolIDForVG())
	unprotected := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %q
  deletion_protection = false
}
`, testStoragePoolIDForVG())
	const addr = "everpure_azure_volume_group.test"

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: summaryRegexp(operation{action: actionCreate, kind: kindVolumeGroup, name: "vg1"}.fail(detailError{})),
			},
			{
				PreConfig: func() {
					store.mu.Lock()
					defer store.mu.Unlock()
					store.failCreatePollKeepObject = false
				},
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "id", testStoragePoolIDForVG()+"/volumeGroups/vg1"),
					resource.TestCheckResourceAttr(addr, "deletion_protection", "true"),
				),
			},
			{Config: config, PlanOnly: true},
			// The resource is protected again, so the final destroy needs this step.
			{
				Config: unprotected,
				Check: func(*terraform.State) error {
					store.mu.Lock()
					defer store.mu.Unlock()
					if store.createCallCount != 2 || store.deleteCallCount != 1 {
						return fmt.Errorf("got %d creates and %d deletes, want 2 and 1 (the failed create, its replacement, and the tainted object's delete)", store.createCallCount, store.deleteCallCount)
					}
					return nil
				},
			},
		},
	})
}

// TestVolumeGroupResourceConfigCasingChange: a case-only respelling of storage_pool_id
// in config must plan nothing.
func TestVolumeGroupResourceConfigCasingChange(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	configWith := func(poolID string) string {
		return providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false
}
`, poolID)
	}

	factories, _, _ := newTestVolumeGroupProviderFactories()
	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{Config: configWith(testStoragePoolIDForVG())},
			{Config: configWith(strings.ToUpper(testStoragePoolIDForVG())), PlanOnly: true},
		},
	})
}
