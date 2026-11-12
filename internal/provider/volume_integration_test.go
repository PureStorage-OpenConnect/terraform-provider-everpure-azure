// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
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
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
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

// newTestVolumeProviderFactories builds an isolated provider instance wired to a fresh
// in-memory fakeVolumeStore, so each test gets its own backing store. A volume needs no
// parent storage pool or volume group seeded anywhere: Create never fetches either, so
// the fake only ever has to stand in for VolumesClient.
func newTestVolumeProviderFactories() (map[string]func() (tfprotov6.ProviderServer, error), *fakeVolumeStore) {
	store := newFakeVolumeStore(testSubscriptionID)
	transport := &selfHealingTransport{build: func() policy.Transporter {
		srv := store.newVolumesServer()
		return psbfake.NewVolumesServerTransport(&srv)
	}}

	opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: transport}}
	p := newProvider("test", fixedCredentialFactory(&azcorefake.TokenCredential{}), opts)()

	return map[string]func() (tfprotov6.ProviderServer, error){
		"everpure": providerserver.NewProtocol6WithError(p),
	}, store
}

// testVolumeGroupID builds a well-formed volume group ARM ID under the shared test
// subscription/resource group/storage pool, for volume_group_id and for any source
// attribute that must resolve to a real-looking parent. Volume tests never need this
// volume group to actually exist in any fake store: Create parses it from the string
// alone and never fetches it.
func testVolumeGroupID(volumeGroupName string) string {
	return armids.VolumeGroupID{
		ResourceGroupID: armids.ResourceGroupID{
			SubscriptionID:    testSubscriptionID,
			ResourceGroupName: testResourceGroup,
		},
		StoragePoolName: testStoragePoolName,
		VolumeGroupName: volumeGroupName,
	}.FullID()
}

// testVolumeSourceID builds a well-formed volume ARM ID for use as a source_volume_id
// or source_recoverable_volume_id value. It never needs to name a volume that actually
// exists in the fake store: the provider's own guards only check the ID's shape and its
// parent, never existence.
func testVolumeSourceID(volumeGroupName, name string) string {
	return testVolumeGroupID(volumeGroupName) + "/volumes/" + name
}

// testVolumeGroupSnapshotID builds a well-formed volume group snapshot ARM ID, for
// source_snapshot.id.
func testVolumeGroupSnapshotID(volumeGroupName, snapshotName string) string {
	return testVolumeGroupID(volumeGroupName) + "/snapshots/" + snapshotName
}

// name40 is exactly 40 characters, the longest name create itself accepts. Soft-deleting
// it produces a 55-character twin ("<name40>-<14-digit timestamp>"), exercising the
// addressing path for a twin whose name create would itself reject as too long.
const name40 = "v" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + "1"

func init() {
	if len(name40) != 40 {
		panic(fmt.Sprintf("name40 must be exactly 40 characters, got %d", len(name40)))
	}
}

// volumesUnderPrefix returns every entry store currently holds whose key starts with
// prefix (as built by volumeKey with an empty name), for tests that need to inspect the
// backing store directly rather than through Terraform state — e.g. confirming exactly
// which soft-deleted twins survived an eradication.
func volumesUnderPrefix(store *fakeVolumeStore, resourceGroupName, storagePoolName, volumeGroupName string) []*armpurestorageblock.Volume {
	store.mu.Lock()
	defer store.mu.Unlock()

	prefix := volumeKey(resourceGroupName, storagePoolName, volumeGroupName, "")
	var out []*armpurestorageblock.Volume
	for k, v := range store.volumes {
		if strings.HasPrefix(k, prefix) {
			out = append(out, v)
		}
	}
	return out
}

func TestVolumeResource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	t.Run("creates a minimal volume with source_type none", func(t *testing.T) {
		factories, store := newTestVolumeProviderFactories()

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg1"))

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_volume.test", "name", "vol1"),
						resource.TestCheckResourceAttrSet("everpure_azure_volume.test", "id"),
						resource.TestCheckResourceAttr("everpure_azure_volume.test", "source_type", "none"),
						resource.TestCheckResourceAttr("everpure_azure_volume.test", "provisioned_size_in_bytes", strconv.Itoa(5*bytesPerGiB)),
						resource.TestCheckResourceAttrSet("everpure_azure_volume.test", "serial_number"),
						resource.TestCheckNoResourceAttr("everpure_azure_volume.test", "source_volume_id"),
						resource.TestCheckNoResourceAttr("everpure_azure_volume.test", "source_serial_number"),
						resource.TestCheckNoResourceAttr("everpure_azure_volume.test", "source_recoverable_volume_id"),
						resource.TestCheckResourceAttr("everpure_azure_volume.test", "source_snapshot.#", "0"),
					),
				},
			},
		})

		if store.lastCreate == nil || store.lastCreate.Properties == nil || store.lastCreate.Properties.SourceType == nil {
			t.Fatal("expected a captured create body")
		}
		if *store.lastCreate.Properties.SourceType != armpurestorageblock.VolumeSourceTypeNone {
			// The volume group's own create validation rejects an explicit "none", so it
			// omits the field; the volume API accepts "none" explicitly, so the mapping
			// must send it as-is rather than copying that omission.
			t.Fatalf("got sourceType %v, want an explicit %q", *store.lastCreate.Properties.SourceType, armpurestorageblock.VolumeSourceTypeNone)
		}
	})

	t.Run("creates from each source type: right sourceType, right source field(s), the derived sourceVolumeGroupResourceId reaching the create body, no provisionedSize, and everything survives Read", func(t *testing.T) {
		t.Run("volume", func(t *testing.T) {
			factories, store := newTestVolumeProviderFactories()
			sourceVolID := testVolumeSourceID("vg-source", "source-vol")

			config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                = "vol1"
  volume_group_id     = %[1]q
  deletion_protection = false
  source_type         = "volume"
  source_volume_id    = %[2]q
}
`, testVolumeGroupID("vg1"), sourceVolID)

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config: config,
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr("everpure_azure_volume.test", "source_type", "volume"),
							resource.TestCheckResourceAttr("everpure_azure_volume.test", "source_volume_id", sourceVolID),
							resource.TestCheckResourceAttrSet("everpure_azure_volume.test", "provisioned_size_in_bytes"),
						),
					},
					{
						// A second, unchanged plan proves Read preserved every source
						// input from state rather than losing it: the fake, like the
						// real GET, never returns any source* field.
						Config:   config,
						PlanOnly: true,
					},
				},
			})

			if store.lastCreate == nil || store.lastCreate.Properties == nil {
				t.Fatal("expected a captured create body")
			}
			props := store.lastCreate.Properties
			if props.SourceType == nil || *props.SourceType != armpurestorageblock.VolumeSourceTypeVolume {
				t.Fatalf("got sourceType %v, want volume", props.SourceType)
			}
			if props.SourceVolumeResourceID == nil || *props.SourceVolumeResourceID != sourceVolID {
				t.Fatalf("got sourceVolumeResourceId %v, want %s", props.SourceVolumeResourceID, sourceVolID)
			}
			if props.SourceVolumeGroupResourceID == nil || *props.SourceVolumeGroupResourceID != testVolumeGroupID("vg-source") {
				t.Fatalf("got sourceVolumeGroupResourceId %v, want %s", props.SourceVolumeGroupResourceID, testVolumeGroupID("vg-source"))
			}
			if props.ProvisionedSize != nil {
				t.Fatalf("got provisionedSize %v, want nil: a clone's size is inherited from its source", *props.ProvisionedSize)
			}
		})

		t.Run("serialNumber", func(t *testing.T) {
			factories, store := newTestVolumeProviderFactories()

			config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                  = "vol1"
  volume_group_id       = %[1]q
  deletion_protection   = false
  source_type           = "serialNumber"
  source_serial_number  = "SOMEARRAYSERIAL00000001"
}
`, testVolumeGroupID("vg1"))

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config: config,
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr("everpure_azure_volume.test", "source_type", "serialNumber"),
							resource.TestCheckResourceAttr("everpure_azure_volume.test", "source_serial_number", "SOMEARRAYSERIAL00000001"),
							resource.TestCheckResourceAttrSet("everpure_azure_volume.test", "provisioned_size_in_bytes"),
						),
					},
					{Config: config, PlanOnly: true},
				},
			})

			props := store.lastCreate.Properties
			if props.SourceType == nil || *props.SourceType != armpurestorageblock.VolumeSourceTypeSerialNumber {
				t.Fatalf("got sourceType %v, want serialNumber", props.SourceType)
			}
			if props.SourceSerialNumber == nil || *props.SourceSerialNumber != "SOMEARRAYSERIAL00000001" {
				t.Fatalf("got sourceSerialNumber %v, want SOMEARRAYSERIAL00000001", props.SourceSerialNumber)
			}
			if props.SourceVolumeGroupResourceID != nil {
				t.Fatalf("got sourceVolumeGroupResourceId %v, want nil: serialNumber has no source volume group", *props.SourceVolumeGroupResourceID)
			}
			if props.ProvisionedSize != nil {
				t.Fatalf("got provisionedSize %v, want nil", *props.ProvisionedSize)
			}
		})

		t.Run("snapshot", func(t *testing.T) {
			factories, store := newTestVolumeProviderFactories()
			snapshotID := testVolumeGroupSnapshotID("vg-source", "snap1")

			config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                = "vol1"
  volume_group_id     = %[1]q
  deletion_protection = false
  source_type         = "snapshot"

  source_snapshot {
    id          = %[2]q
    volume_name = "member-vol"
  }
}
`, testVolumeGroupID("vg1"), snapshotID)

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config: config,
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr("everpure_azure_volume.test", "source_type", "snapshot"),
							resource.TestCheckResourceAttr("everpure_azure_volume.test", "source_snapshot.#", "1"),
							resource.TestCheckResourceAttr("everpure_azure_volume.test", "source_snapshot.0.id", snapshotID),
							resource.TestCheckResourceAttr("everpure_azure_volume.test", "source_snapshot.0.volume_name", "member-vol"),
							resource.TestCheckResourceAttrSet("everpure_azure_volume.test", "provisioned_size_in_bytes"),
						),
					},
					{Config: config, PlanOnly: true},
				},
			})

			props := store.lastCreate.Properties
			if props.SourceType == nil || *props.SourceType != armpurestorageblock.VolumeSourceTypeSnapshot {
				t.Fatalf("got sourceType %v, want snapshot", props.SourceType)
			}
			if props.SourceVolumeSnapshot == nil {
				t.Fatal("expected sourceVolumeSnapshot to be set")
			}
			if props.SourceVolumeSnapshot.VolumeGroupSnapshotResourceID == nil || *props.SourceVolumeSnapshot.VolumeGroupSnapshotResourceID != snapshotID {
				t.Fatalf("got volumeGroupSnapshotResourceId %v, want %s", props.SourceVolumeSnapshot.VolumeGroupSnapshotResourceID, snapshotID)
			}
			if props.SourceVolumeSnapshot.VolumeSnapshotName == nil || *props.SourceVolumeSnapshot.VolumeSnapshotName != "member-vol" {
				t.Fatalf("got volumeSnapshotName %v, want member-vol", props.SourceVolumeSnapshot.VolumeSnapshotName)
			}
			if props.SourceVolumeGroupResourceID == nil || *props.SourceVolumeGroupResourceID != testVolumeGroupID("vg-source") {
				t.Fatalf("got sourceVolumeGroupResourceId %v, want %s", props.SourceVolumeGroupResourceID, testVolumeGroupID("vg-source"))
			}
			if props.ProvisionedSize != nil {
				t.Fatalf("got provisionedSize %v, want nil", *props.ProvisionedSize)
			}
		})

		t.Run("recoverableVolume", func(t *testing.T) {
			factories, store := newTestVolumeProviderFactories()
			recoverableID := testVolumeSourceID("vg1", "vol1-20260101000000")

			config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                          = "vol1"
  volume_group_id               = %[1]q
  deletion_protection           = false
  source_type                   = "recoverableVolume"
  source_recoverable_volume_id  = %[2]q
}
`, testVolumeGroupID("vg1"), recoverableID)

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config: config,
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr("everpure_azure_volume.test", "source_type", "recoverableVolume"),
							resource.TestCheckResourceAttr("everpure_azure_volume.test", "source_recoverable_volume_id", recoverableID),
							resource.TestCheckResourceAttrSet("everpure_azure_volume.test", "provisioned_size_in_bytes"),
						),
					},
					{Config: config, PlanOnly: true},
				},
			})

			props := store.lastCreate.Properties
			if props.SourceType == nil || *props.SourceType != armpurestorageblock.VolumeSourceTypeRecoverableVolume {
				t.Fatalf("got sourceType %v, want recoverableVolume", props.SourceType)
			}
			if props.SourceRecoverableVolumeResourceID == nil || *props.SourceRecoverableVolumeResourceID != recoverableID {
				t.Fatalf("got sourceRecoverableVolumeResourceId %v, want %s", props.SourceRecoverableVolumeResourceID, recoverableID)
			}
			if props.SourceVolumeGroupResourceID != nil {
				t.Fatalf("got sourceVolumeGroupResourceId %v, want nil: recoverableVolume has no source volume group", *props.SourceVolumeGroupResourceID)
			}
			if props.ProvisionedSize != nil {
				t.Fatalf("got provisionedSize %v, want nil", *props.ProvisionedSize)
			}
		})
	})

	t.Run("grows the size via a gib config: one PATCH carrying the exact byte count, then a second apply is empty", func(t *testing.T) {
		factories, store := newTestVolumeProviderFactories()

		base := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 1
}
`, testVolumeGroupID("vg1"))

		grown := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 2
}
`, testVolumeGroupID("vg1"))

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: base},
				{
					Config: grown,
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction("everpure_azure_volume.test", plancheck.ResourceActionUpdate),
						},
					},
					Check: resource.TestCheckResourceAttr("everpure_azure_volume.test", "provisioned_size_in_bytes", strconv.Itoa(2*bytesPerGiB)),
				},
				{
					// Converges: re-applying the same gib value plans no change at all.
					Config:   grown,
					PlanOnly: true,
				},
			},
		})

		if store.updateCallCount != 1 {
			t.Fatalf("got %d update calls, want exactly 1 for a single grow", store.updateCallCount)
		}
		if store.lastUpdate == nil || store.lastUpdate.Properties == nil || store.lastUpdate.Properties.ProvisionedSize == nil {
			t.Fatal("expected a captured PATCH body with provisionedSize set")
		}
		if got := *store.lastUpdate.Properties.ProvisionedSize; got != 2*bytesPerGiB {
			t.Fatalf("PATCH provisionedSize = %d, want %d", got, int64(2*bytesPerGiB))
		}
	})

	t.Run("changing only deletion_protection or eradicate_on_destroy sends no PATCH", func(t *testing.T) {
		factories, store := newTestVolumeProviderFactories()

		base := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = true
  eradicate_on_destroy    = false
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg1"))

		flagsChanged := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  eradicate_on_destroy    = true
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg1"))

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: base},
				{
					Config: flagsChanged,
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction("everpure_azure_volume.test", plancheck.ResourceActionUpdate),
						},
					},
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_volume.test", "deletion_protection", "false"),
						resource.TestCheckResourceAttr("everpure_azure_volume.test", "eradicate_on_destroy", "true"),
					),
				},
			},
		})

		if store.updateCallCount != 0 {
			t.Fatalf("got %d update calls, want 0: flipping only provider-only attributes must send no PATCH", store.updateCallCount)
		}
	})

	t.Run("changing only timeouts sends no PATCH", func(t *testing.T) {
		factories, store := newTestVolumeProviderFactories()

		base := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg1"))

		timeoutsChanged := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 5

  timeouts {
    update = "45m"
  }
}
`, testVolumeGroupID("vg1"))

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: base},
				{
					Config: timeoutsChanged,
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction("everpure_azure_volume.test", plancheck.ResourceActionUpdate),
						},
					},
					Check: resource.TestCheckResourceAttr("everpure_azure_volume.test", "timeouts.update", "45m"),
				},
			},
		})

		if store.updateCallCount != 0 {
			t.Fatalf("got %d update calls, want 0: a timeouts-only change is provider-only config and must send no PATCH", store.updateCallCount)
		}
	})

	t.Run("shrinking is rejected at plan time", func(t *testing.T) {
		t.Run("bytes config", func(t *testing.T) {
			factories, _ := newTestVolumeProviderFactories()

			base := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                      = "vol1"
  volume_group_id           = %[1]q
  deletion_protection       = false
  provisioned_size_in_bytes = %[2]d
}
`, testVolumeGroupID("vg1"), int64(2*bytesPerGiB))

			shrunk := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                      = "vol1"
  volume_group_id           = %[1]q
  deletion_protection       = false
  provisioned_size_in_bytes = %[2]d
}
`, testVolumeGroupID("vg1"), int64(bytesPerGiB))

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: base},
					{
						Config: shrunk,
						ExpectError: diagRegexp(diag.WithPath(path.Root("provisioned_size_in_bytes"), invalidAttributeValueDiagnostic(
							newVolumeSizeDecreaseError(bytesPerGiB, 2*bytesPerGiB)))),
					},
				},
			})
		})

		t.Run("gib config", func(t *testing.T) {
			factories, _ := newTestVolumeProviderFactories()

			base := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 2
}
`, testVolumeGroupID("vg1"))

			shrunk := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 1
}
`, testVolumeGroupID("vg1"))

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: base},
					{
						Config: shrunk,
						ExpectError: diagRegexp(diag.WithPath(path.Root("provisioned_size_in_gib"), invalidAttributeValueDiagnostic(
							newVolumeSizeDecreaseError(bytesPerGiB, 2*bytesPerGiB)))),
					},
				},
			})
		})
	})

	// TestVolumeResource/config-removal_sequence is the regression coverage for the
	// hazard ModifyPlan's own doc comment calls "the dangerous line in this resource":
	// resolveVolumeSizeInBytes must leave provisioned_size_in_bytes exactly as state
	// carried it forward when provisioned_size_in_gib is removed from config, rather
	// than reading the now-null gib as zero and planning a shrink to nothing. Both
	// variants add a gib line (one PATCH), then delete that line; the removal itself
	// must plan as an update (provisioned_size_in_gib genuinely changes, null to
	// value), send no PATCH, and leave the volume's size exactly where the last real
	// resize left it.
	t.Run("config-removal sequence: removing provisioned_size_in_gib after setting it sends no PATCH and keeps the size", func(t *testing.T) {
		t.Run("on a clone created with no size", func(t *testing.T) {
			factories, store := newTestVolumeProviderFactories()
			sourceVolID := testVolumeSourceID("vg-source", "source-vol")

			noSize := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                 = "vol1"
  volume_group_id      = %[1]q
  deletion_protection  = false
  source_type          = "volume"
  source_volume_id     = %[2]q
}
`, testVolumeGroupID("vg1"), sourceVolID)

			withGib := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                     = "vol1"
  volume_group_id          = %[1]q
  deletion_protection      = false
  source_type              = "volume"
  source_volume_id         = %[2]q
  provisioned_size_in_gib  = 20
}
`, testVolumeGroupID("vg1"), sourceVolID)

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: noSize},
					{
						// The clone's fake-inherited size starts out null, so adding a
						// gib value here is itself a resize: exactly one PATCH.
						Config: withGib,
						Check:  resource.TestCheckResourceAttr("everpure_azure_volume.test", "provisioned_size_in_bytes", strconv.Itoa(20*bytesPerGiB)),
					},
					{
						Config: noSize,
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{
								plancheck.ExpectResourceAction("everpure_azure_volume.test", plancheck.ResourceActionUpdate),
							},
						},
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckNoResourceAttr("everpure_azure_volume.test", "provisioned_size_in_gib"),
							resource.TestCheckResourceAttr("everpure_azure_volume.test", "provisioned_size_in_bytes", strconv.Itoa(20*bytesPerGiB)),
						),
					},
				},
			})

			if store.updateCallCount != 1 {
				t.Fatalf("got %d update calls, want exactly 1 (the add, not the removal)", store.updateCallCount)
			}
		})

		t.Run("on a blank volume created with a size", func(t *testing.T) {
			factories, store := newTestVolumeProviderFactories()

			initial := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                     = "vol1"
  volume_group_id          = %[1]q
  deletion_protection      = false
  provisioned_size_in_gib  = 10
}
`, testVolumeGroupID("vg1"))

			grown := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                     = "vol1"
  volume_group_id          = %[1]q
  deletion_protection      = false
  provisioned_size_in_gib  = 20
}
`, testVolumeGroupID("vg1"))

			// Legal to drop afterwards: presence is a create-only rule, so a
			// provisioned_size_in_none-mentioning config is valid on update even
			// though it would be rejected at create.
			dropped := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                 = "vol1"
  volume_group_id      = %[1]q
  deletion_protection  = false
}
`, testVolumeGroupID("vg1"))

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: initial},
					{
						Config: grown,
						Check:  resource.TestCheckResourceAttr("everpure_azure_volume.test", "provisioned_size_in_bytes", strconv.Itoa(20*bytesPerGiB)),
					},
					{
						Config: dropped,
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{
								plancheck.ExpectResourceAction("everpure_azure_volume.test", plancheck.ResourceActionUpdate),
							},
						},
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckNoResourceAttr("everpure_azure_volume.test", "provisioned_size_in_gib"),
							resource.TestCheckResourceAttr("everpure_azure_volume.test", "provisioned_size_in_bytes", strconv.Itoa(20*bytesPerGiB)),
						),
					},
				},
			})

			if store.updateCallCount != 1 {
				t.Fatalf("got %d update calls, want exactly 1 (10 -> 20 GiB; the removal must send nothing)", store.updateCallCount)
			}
		})
	})

	t.Run("forces replacement", func(t *testing.T) {
		tests := []struct {
			name    string
			base    string
			changed string
		}{
			{
				name: "name change",
				base: providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg1")),
				changed: providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol2"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg1")),
			},
			{
				name: "volume_group_id change",
				base: providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg1")),
				changed: providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg2")),
			},
			{
				name: "source_type change",
				base: providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg1")),
				changed: providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                 = "vol1"
  volume_group_id      = %[1]q
  deletion_protection  = false
  source_type          = "volume"
  source_volume_id     = %[2]q
}
`, testVolumeGroupID("vg1"), testVolumeSourceID("vg-source", "source-vol")),
			},
			{
				name: "source input change (same source_type)",
				base: providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                 = "vol1"
  volume_group_id      = %[1]q
  deletion_protection  = false
  source_type          = "volume"
  source_volume_id     = %[2]q
}
`, testVolumeGroupID("vg1"), testVolumeSourceID("vg-source", "source-vol-a")),
				changed: providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                 = "vol1"
  volume_group_id      = %[1]q
  deletion_protection  = false
  source_type          = "volume"
  source_volume_id     = %[2]q
}
`, testVolumeGroupID("vg1"), testVolumeSourceID("vg-source", "source-vol-b")),
			},
			{
				name: "source_snapshot block change",
				base: providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                 = "vol1"
  volume_group_id      = %[1]q
  deletion_protection  = false
  source_type          = "snapshot"

  source_snapshot {
    id          = %[2]q
    volume_name = "member-a"
  }
}
`, testVolumeGroupID("vg1"), testVolumeGroupSnapshotID("vg-source", "snap1")),
				changed: providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                 = "vol1"
  volume_group_id      = %[1]q
  deletion_protection  = false
  source_type          = "snapshot"

  source_snapshot {
    id          = %[2]q
    volume_name = "member-b"
  }
}
`, testVolumeGroupID("vg1"), testVolumeGroupSnapshotID("vg-source", "snap1")),
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				factories, _ := newTestVolumeProviderFactories()

				resource.Test(t, resource.TestCase{
					IsUnitTest:               true,
					ProtoV6ProviderFactories: factories,
					Steps: []resource.TestStep{
						{Config: tt.base},
						{
							Config: tt.changed,
							ConfigPlanChecks: resource.ConfigPlanChecks{
								PreApply: []plancheck.PlanCheck{
									plancheck.ExpectResourceAction("everpure_azure_volume.test", plancheck.ResourceActionReplace),
								},
							},
						},
					},
				})
			})
		}
	})

	t.Run("deletion_protection default (true) blocks both a destroy plan and a replace-forcing plan", func(t *testing.T) {
		t.Run("destroy", func(t *testing.T) {
			factories, _ := newTestVolumeProviderFactories()

			config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg1"))

			unprotected := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg1"))

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: config},
					{
						Config:      providerBlock(),
						ExpectError: deletionProtectedRegexp(kindVolume),
					},
					// Unprotect so the test harness's own post-test destroy, which
					// would otherwise hit the same block, can succeed.
					{Config: unprotected},
				},
			})
		})

		t.Run("replace", func(t *testing.T) {
			factories, _ := newTestVolumeProviderFactories()

			configWithName := func(name string) string {
				return providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = %[1]q
  volume_group_id         = %[2]q
  provisioned_size_in_gib = 5
}
`, name, testVolumeGroupID("vg1"))
			}
			unprotectedConfigWithName := func(name string) string {
				return providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = %[1]q
  volume_group_id         = %[2]q
  deletion_protection     = false
  provisioned_size_in_gib = 5
}
`, name, testVolumeGroupID("vg1"))
			}

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: configWithName("vol1")},
					{
						Config:      configWithName("vol2"),
						ExpectError: deletionProtectedReplaceRegexp(kindVolume, "name"),
					},
					{Config: unprotectedConfigWithName("vol1")},
				},
			})
		})
	})

	t.Run("destroys with eradicate_on_destroy = false, leaving the renamed entry", func(t *testing.T) {
		factories, store := newTestVolumeProviderFactories()

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg1"))

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: config},
			},
		})

		twins := volumesUnderPrefix(store, testResourceGroup, testStoragePoolName, "vg1")
		if len(twins) != 1 {
			t.Fatalf("got %d entries left in the volume group, want 1 (soft-deleted, not eradicated)", len(twins))
		}
		if twins[0].Properties == nil || twins[0].Properties.SoftDeletion == nil ||
			twins[0].Properties.SoftDeletion.PreviousName == nil || *twins[0].Properties.SoftDeletion.PreviousName != "vol1" {
			t.Fatalf("expected the surviving entry to be vol1's renamed twin, got %+v", twins[0])
		}
	})

	t.Run("destroys with eradicate_on_destroy = true", func(t *testing.T) {
		t.Run("eradicates only the twin this delete just created, ignoring decoys, using a 40-character name", func(t *testing.T) {
			factories, store := newTestVolumeProviderFactories()

			// Both decoys already exist before this test's delete ever runs, so
			// set-difference selection (see newVolumes in resource_volume.go) excludes
			// them regardless of name or age: one under a wholly different
			// previousName, and one that already shares this volume's own
			// previousName (e.g. left over from an earlier delete/recreate cycle).
			store.seedDestroyedVolume(testResourceGroup, testStoragePoolName, "vg1", "other-vol-20260101000000", "other-vol", mustParseTime(t, "2026-01-01T00:00:00Z"))
			store.seedDestroyedVolume(testResourceGroup, testStoragePoolName, "vg1", name40+"-20260101000000", name40, mustParseTime(t, "2026-01-01T00:00:00Z"))

			config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                     = %[1]q
  volume_group_id          = %[2]q
  deletion_protection      = false
  eradicate_on_destroy     = true
  provisioned_size_in_gib  = 5
}
`, name40, testVolumeGroupID("vg1"))

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: config},
				},
			})

			twins := volumesUnderPrefix(store, testResourceGroup, testStoragePoolName, "vg1")
			if len(twins) != 2 {
				t.Fatalf("got %d entries left, want 2 (both decoys; the freshly created 55-character twin was eradicated)", len(twins))
			}
			for _, twin := range twins {
				if twin.Name == nil {
					t.Fatal("expected twin.Name to be set")
				}
				if *twin.Name != "other-vol-20260101000000" && *twin.Name != name40+"-20260101000000" {
					t.Fatalf("unexpected entry remaining: %s", *twin.Name)
				}
			}
		})

		t.Run("warns, not errors, when no twin matches", func(t *testing.T) {
			factories, store := newTestVolumeProviderFactories()
			// Simulates auto-eradication (or someone else) having already won the
			// race by the time this destroy's eradicate step looks for a twin.
			store.skipTwinOnDelete = true

			config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                     = "vol1"
  volume_group_id          = %[1]q
  deletion_protection      = false
  eradicate_on_destroy     = true
  provisioned_size_in_gib  = 5
}
`, testVolumeGroupID("vg1"))

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
	})

	t.Run("imports a volume by its full ARM ID", func(t *testing.T) {
		factories, _ := newTestVolumeProviderFactories()

		// provisioned_size_in_bytes, not the gib sugar attribute: gib is config-only
		// and Read never populates it, so an import (which starts from a blank slate
		// for every attribute ImportState doesn't set explicitly) would otherwise
		// mismatch ImportStateVerify's comparison against the pre-import apply.
		//
		// deletion_protection is left at its schema default (true) rather than set to
		// false: ImportState always resets it to true regardless of what a prior
		// instance had, so leaving it at the default here is what lets the pre-import
		// apply and the imported state agree.
		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                       = "vol1"
  volume_group_id            = %[1]q
  provisioned_size_in_bytes  = %[2]d
}
`, testVolumeGroupID("vg1"), int64(bytesPerGiB))

		unprotected := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                       = "vol1"
  volume_group_id            = %[1]q
  deletion_protection        = false
  provisioned_size_in_bytes  = %[2]d
}
`, testVolumeGroupID("vg1"), int64(bytesPerGiB))

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: config},
				{
					Config:            config,
					ResourceName:      "everpure_azure_volume.test",
					ImportState:       true,
					ImportStateVerify: true,
					ImportStateId: fmt.Sprintf(
						"/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/storagePools/%s/volumeGroups/vg1/volumes/vol1",
						testSubscriptionID, testResourceGroup, testStoragePoolName,
					),
				},
				{Config: unprotected},
			},
		})
	})

	// Import-shaped: ImportState composes volume_group_id via
	// parsed.VolumeGroup().FullID(), which always renders ARM's canonical keyword
	// segment casing, regardless of what casing the import ID itself used. So a
	// config that spells volume_group_id with different (but ARM-equivalent) keyword
	// casing than the canonical form must still plan no changes after import — a
	// missing plan modifier here would show a spurious replace on the very first
	// plan. Only an import-shaped test catches this: an apply-then-plan test can't,
	// because by then state already holds the config's own spelling.
	t.Run("an imported volume plans no changes when config spells volume_group_id with different casing", func(t *testing.T) {
		factories, store := newTestVolumeProviderFactories()

		// Nothing here ever applied this volume through Terraform: it's seeded
		// straight into the store, exactly as if it had been created outside
		// Terraform before this import.
		store.mu.Lock()
		store.volumes[volumeKey(testResourceGroup, testStoragePoolName, "vg1", "vol1")] = &armpurestorageblock.Volume{
			ID:   to.Ptr(store.fakeVolumeID(testResourceGroup, testStoragePoolName, "vg1", "vol1")),
			Name: to.Ptr("vol1"),
			Properties: &armpurestorageblock.AzureVolumeProperties{
				ProvisionedSize: to.Ptr(int64(bytesPerGiB)),
				SerialNumber:    to.Ptr(store.nextSerialNumber()),
			},
		}
		store.mu.Unlock()

		// The practitioner's own config spells volume_group_id with different, but
		// ARM-equivalent, keyword casing; the import ID's own keyword segments are
		// non-canonically cased too, as composing one by hand from the same config
		// would naturally produce.
		mangledVGID := fmt.Sprintf(
			"/Subscriptions/%s/ResourceGroups/%s/providers/purestorage.block/StoragePools/%s/VolumeGroups/vg1",
			testSubscriptionID, testResourceGroup, testStoragePoolName,
		)
		mangledVolumeID := fmt.Sprintf(
			"/Subscriptions/%s/ResourceGroups/%s/providers/purestorage.block/StoragePools/%s/VolumeGroups/vg1/Volumes/vol1",
			testSubscriptionID, testResourceGroup, testStoragePoolName,
		)

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                       = "vol1"
  volume_group_id            = %[1]q
  provisioned_size_in_bytes  = %[2]d
}
`, mangledVGID, int64(bytesPerGiB))

		unprotected := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                       = "vol1"
  volume_group_id            = %[1]q
  deletion_protection        = false
  provisioned_size_in_bytes  = %[2]d
}
`, mangledVGID, int64(bytesPerGiB))

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config:             config,
					ResourceName:       "everpure_azure_volume.test",
					ImportState:        true,
					ImportStatePersist: true,
					ImportStateVerify:  false,
					ImportStateId:      mangledVolumeID,
				},
				{
					// Expected behaviour: config and imported state spell
					// volume_group_id differently but ARM-equivalently, so this
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

	t.Run("drift", func(t *testing.T) {
		t.Run("size changed outside Terraform plans an update", func(t *testing.T) {
			factories, store := newTestVolumeProviderFactories()

			config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg1"))

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: config},
					{
						// Drifts the size down, not up: growing outside Terraform while
						// config still names the smaller, already-applied value would
						// have this refresh plan try to reconcile back down to it, which
						// is indistinguishable from a real shrink and is rejected for
						// the same reason a real one would be. Drifting down instead
						// makes the reconciling plan a grow back to the configured
						// value — a legitimate non-empty plan, not a shrink.
						PreConfig: func() {
							store.mutateVolume(testResourceGroup, testStoragePoolName, "vg1", "vol1", func(v *armpurestorageblock.Volume) {
								v.Properties.ProvisionedSize = to.Ptr(int64(bytesPerGiB))
							})
						},
						Config:             config,
						PlanOnly:           true,
						ExpectNonEmptyPlan: true,
					},
				},
			})
		})

		t.Run("size grown outside Terraform cannot be reconciled with a smaller config", func(t *testing.T) {
			factories, store := newTestVolumeProviderFactories()

			config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg1"))

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: config},
					{
						// The counterpart to the drift-down case above, and a dead end
						// by design rather than a gap: once the volume is larger than
						// the configuration asks for, reconciling would mean shrinking
						// it, which neither the provider nor the backend permits. Every
						// plan fails until someone raises the configured size or drops
						// the attribute, so the error has to say both of those — pinning
						// it here keeps that guidance from being edited away.
						PreConfig: func() {
							store.mutateVolume(testResourceGroup, testStoragePoolName, "vg1", "vol1", func(v *armpurestorageblock.Volume) {
								v.Properties.ProvisionedSize = to.Ptr(int64(10 * bytesPerGiB))
							})
						},
						Config:   config,
						PlanOnly: true,
						ExpectError: diagRegexp(diag.WithPath(path.Root("provisioned_size_in_gib"), invalidAttributeValueDiagnostic(
							newVolumeSizeDecreaseError(5*bytesPerGiB, 10*bytesPerGiB)))),
					},
				},
			})
		})

		t.Run("volume removed outside Terraform plans a recreate", func(t *testing.T) {
			factories, store := newTestVolumeProviderFactories()

			config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg1"))

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: config},
					{
						PreConfig: func() {
							store.removeVolume(testResourceGroup, testStoragePoolName, "vg1", "vol1")
						},
						Config:             config,
						PlanOnly:           true,
						ExpectNonEmptyPlan: true,
					},
				},
			})
		})
	})

	t.Run("mapping-contract variants", func(t *testing.T) {
		t.Run("re-casing fake: config casing survives even though the API echoes a different case", func(t *testing.T) {
			factories, store := newTestVolumeProviderFactories()
			store.recaseIDs = true

			config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg1"))

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config: config,
						Check: resource.ComposeAggregateTestCheckFunc(
							// name is Required (not Computed): had applyVolume mapped the
							// API's echoed, upper-cased name onto state, this apply would
							// have failed with "Provider produced inconsistent result
							// after apply" before this check ever ran.
							resource.TestCheckResourceAttr("everpure_azure_volume.test", "name", "vol1"),
							resource.TestCheckResourceAttr("everpure_azure_volume.test", "volume_group_id", testVolumeGroupID("vg1")),
							// id is Computed, so it's fine (and expected) for the
							// recased fake's upper-cased id to reach state — this just
							// proves recaseIDs actually took effect, so the check above
							// isn't vacuously passing against an inactive fake mode.
							resource.TestMatchResourceAttr("everpure_azure_volume.test", "id", regexp.MustCompile("[A-Z]")),
						),
					},
				},
			})
		})

		t.Run("nil Properties on Get surfaces as an error rather than a panic", func(t *testing.T) {
			factories, store := newTestVolumeProviderFactories()

			config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg1"))

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: config},
					{
						PreConfig:   func() { store.nilPropertiesOnGet = true },
						Config:      config,
						PlanOnly:    true,
						ExpectError: regexp.MustCompile("no properties"),
					},
					{
						// Restore normal Get behavior before the test harness's own
						// post-test destroy runs its refresh.
						PreConfig: func() { store.nilPropertiesOnGet = false },
						Config:    config,
					},
				},
			})
		})
	})

	t.Run("cloning from a managed volume's own serial_number fails with the backend's error surfaced intact", func(t *testing.T) {
		factories, _ := newTestVolumeProviderFactories()

		vol1Only := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "a" {
  name                    = "vol-a"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 1
}
`, testVolumeGroupID("vg1"))

		bothVolumes := vol1Only + fmt.Sprintf(`
resource "everpure_azure_volume" "b" {
  name                   = "vol-b"
  volume_group_id        = %[1]q
  deletion_protection    = false
  source_type            = "serialNumber"
  source_serial_number   = everpure_azure_volume.a.serial_number
}
`, testVolumeGroupID("vg1"))

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: vol1Only},
				{
					// The provider cannot detect this locally — serial_number's own
					// description says it always fails as a source, but nothing short
					// of the API call itself can know that "a"'s serial happens to
					// belong to a volume this provider manages. This test exists to
					// pin that the backend's rejection reaches the practitioner
					// unfiltered rather than being swallowed or reworded.
					Config:      bothVolumes,
					ExpectError: regexp.MustCompile("ErrNativeVMVolumeSerialSourceNotAllowed"),
				},
			},
		})
	})

	t.Run("rejects invalid configuration at plan time", func(t *testing.T) {
		tests := []struct {
			name          string
			resourceBlock string
			wantErr       string
		}{
			{
				name: "name too short",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "a"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg1")),
				wantErr: "2-40 characters",
			},
			{
				name: "malformed volume_group_id",
				resourceBlock: `
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = "not-a-resource-id"
  deletion_protection     = false
  provisioned_size_in_gib = 5
}
`,
				wantErr: "Resource Manager ID of",
			},
			{
				name: "missing size for source_type none",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                 = "vol1"
  volume_group_id      = %[1]q
  deletion_protection  = false
}
`, testVolumeGroupID("vg1")),
				// \s+ rather than a literal space: Terraform's CLI output wraps this
				// long detail sentence right at that word boundary.
				wantErr: `is required when\s+source_type`,
			},
			{
				name: "size set for a clone",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                     = "vol1"
  volume_group_id          = %[1]q
  deletion_protection      = false
  source_type              = "volume"
  source_volume_id         = %[2]q
  provisioned_size_in_gib  = 5
}
`, testVolumeGroupID("vg1"), testVolumeSourceID("vg-source", "source-vol")),
				// \s+ rather than a literal space: same wrapping hazard as above.
				wantErr: `must not be set when\s+source_type`,
			},
			{
				name: "non-512-multiple size",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                       = "vol1"
  volume_group_id            = %[1]q
  deletion_protection        = false
  provisioned_size_in_bytes  = 1048577
}
`, testVolumeGroupID("vg1")),
				wantErr: "multiple of 512",
			},
			{
				name: "both size attributes set",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                       = "vol1"
  volume_group_id            = %[1]q
  deletion_protection        = false
  provisioned_size_in_bytes  = 1073741824
  provisioned_size_in_gib    = 1
}
`, testVolumeGroupID("vg1")),
				wantErr: "cannot be specified when",
			},
			{
				name: "source_volume_id set while source_type is none",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 5
  source_volume_id        = %[2]q
}
`, testVolumeGroupID("vg1"), testVolumeSourceID("vg-source", "source-vol")),
				wantErr: "must not be set",
			},
			{
				name: "source_type = volume without source_volume_id",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                 = "vol1"
  volume_group_id      = %[1]q
  deletion_protection  = false
  source_type          = "volume"
}
`, testVolumeGroupID("vg1")),
				wantErr: "is required",
			},
			{
				name: "source_type = snapshot without source_snapshot",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                 = "vol1"
  volume_group_id      = %[1]q
  deletion_protection  = false
  source_type          = "snapshot"
}
`, testVolumeGroupID("vg1")),
				wantErr: "is required",
			},
			{
				name: "invalid source_type",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                 = "vol1"
  volume_group_id      = %[1]q
  deletion_protection  = false
  source_type          = "bogus"
}
`, testVolumeGroupID("vg1")),
				wantErr: "value must be one of",
			},
			{
				name: "malformed source_volume_id",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                 = "vol1"
  volume_group_id      = %[1]q
  deletion_protection  = false
  source_type          = "volume"
  source_volume_id     = "not-a-resource-id"
}
`, testVolumeGroupID("vg1")),
				wantErr: "Resource Manager ID of",
			},
			{
				name: "malformed source_snapshot.id",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                 = "vol1"
  volume_group_id      = %[1]q
  deletion_protection  = false
  source_type          = "snapshot"

  source_snapshot {
    id          = "not-a-resource-id"
    volume_name = "member-vol"
  }
}
`, testVolumeGroupID("vg1")),
				wantErr: "Resource Manager ID of",
			},
			{
				name: "empty source_serial_number",
				resourceBlock: fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                   = "vol1"
  volume_group_id        = %[1]q
  deletion_protection    = false
  source_type            = "serialNumber"
  source_serial_number   = ""
}
`, testVolumeGroupID("vg1")),
				wantErr: "string length must be at least 1",
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				factories, _ := newTestVolumeProviderFactories()
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
}

// TestVolumeResourceDynamicSourceSnapshotBlock is the mandatory regression test for a
// dynamic block whose for_each derives from a Terraform variable rather than a literal
// list: Terraform core then reports the whole source_snapshot list as unknown while
// validating the config, even before the for_each value itself has been resolved, and
// even when the variable's value would already be statically known. A model field that
// can't represent unknown crashes with "Received unknown value, however the target type
// cannot handle unknown values" as soon as such a config is planned or validated. This
// exact hazard has shipped twice in this provider on two different Optional blocks,
// both times undetected because every existing test config used a literal block
// instead of "dynamic". source_snapshot's own presence is coupled to source_type (it is
// required exactly when source_type = "snapshot"), so this needs two separate configs
// rather than one shared one toggled by the variable alone: one exercises a populated
// dynamic block, the other an empty one.
func TestVolumeResourceDynamicSourceSnapshotBlock(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	t.Run("one element: plan and apply succeed, the block lands in state", func(t *testing.T) {
		factories, _ := newTestVolumeProviderFactories()

		cfg := providerBlock() + fmt.Sprintf(`
variable "snapshots" {
  type = list(object({
    id          = string
    volume_name = string
  }))
  default = []
}

resource "everpure_azure_volume" "test" {
  name                 = "vol1"
  volume_group_id      = %[1]q
  deletion_protection  = false
  source_type          = "snapshot"

  dynamic "source_snapshot" {
    for_each = var.snapshots
    content {
      id          = source_snapshot.value.id
      volume_name = source_snapshot.value.volume_name
    }
  }
}
`, testVolumeGroupID("vg1"))

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: cfg,
					ConfigVariables: config.Variables{
						"snapshots": config.ListVariable(config.ObjectVariable(map[string]config.Variable{
							"id":          config.StringVariable(testVolumeGroupSnapshotID("vg-source", "snap1")),
							"volume_name": config.StringVariable("member-vol"),
						})),
					},
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_volume.test", "source_snapshot.#", "1"),
						resource.TestCheckResourceAttr("everpure_azure_volume.test", "source_snapshot.0.id", testVolumeGroupSnapshotID("vg-source", "snap1")),
						resource.TestCheckResourceAttr("everpure_azure_volume.test", "source_snapshot.0.volume_name", "member-vol"),
					),
				},
			},
		})
	})

	t.Run("empty list (the variable's default): applies clean with no block", func(t *testing.T) {
		factories, _ := newTestVolumeProviderFactories()

		cfg := providerBlock() + fmt.Sprintf(`
variable "snapshots" {
  type = list(object({
    id          = string
    volume_name = string
  }))
  default = []
}

resource "everpure_azure_volume" "test" {
  name                     = "vol1"
  volume_group_id          = %[1]q
  deletion_protection      = false
  provisioned_size_in_gib  = 5

  dynamic "source_snapshot" {
    for_each = var.snapshots
    content {
      id          = source_snapshot.value.id
      volume_name = source_snapshot.value.volume_name
    }
  }
}
`, testVolumeGroupID("vg1"))

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: cfg,
					Check:  resource.TestCheckResourceAttr("everpure_azure_volume.test", "source_snapshot.#", "0"),
				},
			},
		})
	})
}

// TestVolumeResourceImportOtherSubscription covers the subscription guard on
// import. The provider's clients are pinned to its configured subscription, so
// importing an ID from another one would otherwise surface later as a
// misleading "not found" from ARM rather than an actionable error.
func TestVolumeResourceImportOtherSubscription(t *testing.T) {
	factories, _ := newTestVolumeProviderFactories()

	config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                      = "vol1"
  volume_group_id           = %[1]q
  deletion_protection       = false
  provisioned_size_in_bytes = %[2]d
}
`, testVolumeGroupID("vg1"), int64(bytesPerGiB))

	importID := fmt.Sprintf(
		"/subscriptions/22222222-2222-2222-2222-222222222222/resourceGroups/%s/providers/"+
			"PureStorage.Block/storagePools/%s/volumeGroups/vg1/volumes/vol1",
		testResourceGroup, testStoragePoolName,
	)
	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{Config: config},
			{
				Config:        config,
				ResourceName:  "everpure_azure_volume.test",
				ImportState:   true,
				ImportStateId: importID,
				ExpectError:   subscriptionMismatchRegexp(kindVolume, importID),
			},
		},
	})
}

func TestVolumeResourceImportSoftDeletedVolume(t *testing.T) {
	factories, store := newTestVolumeProviderFactories()
	store.seedDestroyedVolume(testResourceGroup, testStoragePoolName, "vg1", "vol1-20261026170054", "vol1", time.Now())

	config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                      = "vol1"
  volume_group_id           = %[1]q
  deletion_protection       = false
  provisioned_size_in_bytes = %[2]d
}
`, testVolumeGroupID("vg1"), int64(bytesPerGiB))

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config:        config,
				ResourceName:  "everpure_azure_volume.test",
				ImportState:   true,
				ImportStateId: testVolumeGroupID("vg1") + "/volumes/vol1-20261026170054",
				ExpectError:   regexp.MustCompile(`(?s)Cannot import non-existent remote object`),
			},
		},
	})
}

// newTestVolumeDeleteResource builds a VolumeResource wired to a fresh fakeVolumeStore
// through a direct Configure call (bypassing the provider server entirely), for tests
// that drive Delete directly rather than through terraform-plugin-testing. Calling
// Delete directly, rather than through a full plan/apply, is what lets these tests seed
// a live volume whose stored Name deliberately differs in case from the map key its
// name is looked up by — simulating state and the server disagreeing about a name's
// letter case (see the comment on the eradication match in resource_volume.go's Delete)
// — without the fake's key lookup itself (which is exact-match, unlike the real ARM
// API) getting in the way.
func newTestVolumeDeleteResource(t *testing.T) (*VolumeResource, *fakeVolumeStore) {
	t.Helper()

	store := newFakeVolumeStore(testSubscriptionID)
	srv := store.newVolumesServer()
	transport := psbfake.NewVolumesServerTransport(&srv)
	opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: transport}}

	r := &VolumeResource{}
	var configureResp fwresource.ConfigureResponse
	r.Configure(context.Background(), fwresource.ConfigureRequest{ProviderData: &Clients{
		cred:           &azcorefake.TokenCredential{},
		opts:           opts,
		subscriptionID: testSubscriptionID,
	}}, &configureResp)
	if configureResp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", configureResp.Diagnostics)
	}

	return r, store
}

// newTestVolumeDeleteState builds a tfsdk.State for VolumeResource.Delete, starting
// from baseVolumeResourceModel (name "vol1", volume group testVolumeGroupIDFixture, i.e.
// "vg1") and applying overrides.
func newTestVolumeDeleteState(t *testing.T, r *VolumeResource, overrides func(*VolumeResourceModel)) tfsdk.State {
	t.Helper()

	ctx := context.Background()
	var schemaResp fwresource.SchemaResponse
	r.Schema(ctx, fwresource.SchemaRequest{}, &schemaResp)

	data := baseVolumeResourceModel()
	data.DeletionProtection = types.BoolValue(false)
	data.EradicateOnDestroy = types.BoolValue(true)
	if overrides != nil {
		overrides(&data)
	}

	state := tfsdk.State{Schema: schemaResp.Schema}
	if diags := state.Set(ctx, &data); diags.HasError() {
		t.Fatalf("unexpected diagnostics building state: %v", diags)
	}
	return state
}

// TestVolumeDeleteEradicatesDespiteCaseDivergence proves the fix for the bug where
// eradicate_on_destroy silently skipped eradication whenever state's copy of the name
// differed in letter case from the server's own stored spelling (e.g. after importing
// with a different case than the server actually holds — Read never corrects it). The
// seeded entry's stored Name ("Vol1") deliberately differs in case from both the map key
// it's looked up by and from state's own name (both "vol1"), standing in for that
// divergence; a real backend would resolve the lookup by "vol1" case-insensitively but
// rename using its own stored spelling on soft delete, exactly like this fake now does
// (see the fix to fakeVolumeStore's BeginDelete).
func TestVolumeDeleteEradicatesDespiteCaseDivergence(t *testing.T) {
	r, store := newTestVolumeDeleteResource(t)

	key := volumeKey(testResourceGroup, testStoragePoolName, "vg1", "vol1")
	store.volumes[key] = &armpurestorageblock.Volume{
		ID:   to.Ptr(store.fakeVolumeID(testResourceGroup, testStoragePoolName, "vg1", "Vol1")),
		Name: to.Ptr("Vol1"),
		Properties: &armpurestorageblock.AzureVolumeProperties{
			ProvisionedSize: to.Ptr(int64(1073741824)),
			SerialNumber:    to.Ptr(store.nextSerialNumber()),
			CreatedAt:       to.Ptr(time.Now().UTC()),
		},
	}

	state := newTestVolumeDeleteState(t, r, func(data *VolumeResourceModel) {
		data.Name = types.StringValue("vol1")
	})

	deleteResp := &fwresource.DeleteResponse{}
	r.Delete(context.Background(), fwresource.DeleteRequest{State: state}, deleteResp)

	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", deleteResp.Diagnostics)
	}
	if len(deleteResp.Diagnostics) != 0 {
		t.Fatalf("expected no diagnostics at all (a warning would mean eradication was skipped), got %v", deleteResp.Diagnostics)
	}

	remaining := volumesUnderPrefix(store, testResourceGroup, testStoragePoolName, "vg1")
	if len(remaining) != 0 {
		t.Fatalf("got %d entries left, want 0 (the twin must have been eradicated): %+v", len(remaining), remaining)
	}
}

// TestVolumeDeleteAmbiguousRemovalSkipsEradication exercises the defence-in-depth path
// covering concurrent destroys in the same volume group (reachable under Terraform's
// default parallelism): if more than one volume disappears between the two listings and
// narrowing by state's own name still leaves more than one candidate, eradication must
// not guess — it must skip and warn, the same as when no twin is found at all.
// mutateVolume can't produce this shape (it edits one stored entry, not the listings'
// membership), so this seeds two live entries directly and removes both without a real
// two-name-matching twin in between — standing in for a case where the before/after
// listings can't tell which removal was this delete's own.
func TestVolumeDeleteAmbiguousRemovalSkipsEradication(t *testing.T) {
	r, store := newTestVolumeDeleteResource(t)

	// Two live volumes whose folded names both equal "vol1" cannot really coexist on
	// the backend (case-insensitive uniqueness), but removedVolumeName must still
	// refuse to pick between them rather than assume the shape can't occur.
	store.volumes[volumeKey(testResourceGroup, testStoragePoolName, "vg1", "vol1")] = &armpurestorageblock.Volume{
		Name: to.Ptr("vol1"),
		Properties: &armpurestorageblock.AzureVolumeProperties{
			ProvisionedSize: to.Ptr(int64(1073741824)),
			SerialNumber:    to.Ptr(store.nextSerialNumber()),
		},
	}
	store.volumes[volumeKey(testResourceGroup, testStoragePoolName, "vg1", "VOL1")] = &armpurestorageblock.Volume{
		Name: to.Ptr("VOL1"),
		Properties: &armpurestorageblock.AzureVolumeProperties{
			ProvisionedSize: to.Ptr(int64(1073741824)),
			SerialNumber:    to.Ptr(store.nextSerialNumber()),
		},
	}

	// skipTwinOnDelete removes the live entry with no renamed twin, matching the
	// shape this test cares about: a volume this delete removed, with nothing left
	// to disambiguate by matching a twin's own previousName.
	store.skipTwinOnDelete = true

	state := newTestVolumeDeleteState(t, r, func(data *VolumeResourceModel) {
		data.Name = types.StringValue("vol1")
	})

	deleteResp := &fwresource.DeleteResponse{}
	r.Delete(context.Background(), fwresource.DeleteRequest{State: state}, deleteResp)

	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("unexpected error diagnostics: %v", deleteResp.Diagnostics)
	}
	if len(deleteResp.Diagnostics) != 1 {
		t.Fatalf("expected exactly one warning, got %v", deleteResp.Diagnostics)
	}
	want := softDeletedTwinNotFoundWarning(kindVolume, "vol1")
	if !deleteResp.Diagnostics.Contains(want) {
		t.Fatalf("diagnostics = %v, want %v", deleteResp.Diagnostics, want)
	}

	// The other, unrelated live volume (VOL1) must be untouched.
	remaining := volumesUnderPrefix(store, testResourceGroup, testStoragePoolName, "vg1")
	if len(remaining) != 1 || remaining[0].Name == nil || *remaining[0].Name != "VOL1" {
		t.Fatalf("got %+v, want only VOL1 left untouched", remaining)
	}
}

// TestVolumeFailedCreateReplacementIsNotBlockedByDeletionProtection covers a create whose
// poll fails after Azure finished creating the object: the tainted volume survives refresh,
// so its replacement really runs Delete, which must not be refused by the default
// deletion_protection = true.
func TestVolumeFailedCreateReplacementIsNotBlockedByDeletionProtection(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	factories, store := newTestVolumeProviderFactories()
	store.failCreatePollKeepObject = true

	config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %q
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg1"))
	unprotected := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %q
  deletion_protection     = false
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg1"))
	const addr = "everpure_azure_volume.test"

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: summaryRegexp(operation{action: actionCreate, kind: kindVolume, name: "vol1"}.fail(detailError{})),
			},
			{
				PreConfig: func() {
					store.mu.Lock()
					defer store.mu.Unlock()
					store.failCreatePollKeepObject = false
				},
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "id", testVolumeSourceID("vg1", "vol1")),
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

// TestVolumeResourceConfigCasingChange: a case-only respelling of volume_group_id in
// config must plan nothing.
func TestVolumeResourceConfigCasingChange(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	configWith := func(vgID string) string {
		return providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  deletion_protection     = false
  provisioned_size_in_gib = 5
}
`, vgID)
	}

	factories, _ := newTestVolumeProviderFactories()
	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{Config: configWith(testVolumeGroupID("vg1"))},
			{Config: configWith(strings.ToUpper(testVolumeGroupID("vg1"))), PlanOnly: true},
		},
	})
}

// TestVolumeDeleteMissingVolumeGroupSucceeds covers a destroy whose volume group was
// deleted first (a literal volume_group_id gives Terraform no ordering against it): the
// group takes its volumes with it, so the pre-delete listing that eradicate_on_destroy
// needs 404s on the missing parent, and the delete must count as done.
func TestVolumeDeleteMissingVolumeGroupSucceeds(t *testing.T) {
	for _, eradicate := range []bool{true, false} {
		t.Run(fmt.Sprintf("eradicate_on_destroy=%t", eradicate), func(t *testing.T) {
			r, store := newTestVolumeDeleteResource(t)
			store.markVolumeGroupMissing(testResourceGroup, testStoragePoolName, "vg1")

			state := newTestVolumeDeleteState(t, r, func(data *VolumeResourceModel) {
				data.EradicateOnDestroy = types.BoolValue(eradicate)
			})
			deleteResp := &fwresource.DeleteResponse{}
			r.Delete(context.Background(), fwresource.DeleteRequest{State: state}, deleteResp)

			if len(deleteResp.Diagnostics) != 0 {
				t.Fatalf("expected no diagnostics, got %v", deleteResp.Diagnostics)
			}
		})
	}
}
