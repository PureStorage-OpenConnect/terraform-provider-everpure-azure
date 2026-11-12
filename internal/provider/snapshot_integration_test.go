// SPDX-License-Identifier: Apache-2.0

package provider

// everpure_azure_snapshot has no input blocks, so the dynamic-block-unknown-list
// regression covered by other resources' tests does not apply here: nothing in its
// schema is a block a config could wrap in a dynamic block. Its one nested attribute,
// volume_snapshots, is Computed-only and modelled as a types.List.

import (
	"context"
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
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

// newTestSnapshotProviderFactories builds an isolated provider instance wired to a fresh
// in-memory fakeSnapshotStore. A snapshot needs no parent storage pool or volume group
// seeded anywhere: Create never fetches either, so the fake only stands in for
// VolumeGroupSnapshotsClient.
func newTestSnapshotProviderFactories() (map[string]func() (tfprotov6.ProviderServer, error), *fakeSnapshotStore) {
	store := newFakeSnapshotStore(testSubscriptionID)
	transport := &selfHealingTransport{build: func() policy.Transporter {
		srv := store.newVolumeGroupSnapshotsServer()
		return psbfake.NewVolumeGroupSnapshotsServerTransport(&srv)
	}}

	opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: transport}}
	p := newProvider("test", fixedCredentialFactory(&azcorefake.TokenCredential{}), opts)()

	return map[string]func() (tfprotov6.ProviderServer, error){
		"everpure": providerserver.NewProtocol6WithError(p),
	}, store
}

// snapshotConfig renders a snapshot resource block. extra is appended inside the block.
func snapshotConfig(name, volumeGroupID, extra string) string {
	return providerBlock() + fmt.Sprintf(`
resource "everpure_azure_snapshot" "test" {
  name            = %[1]q
  volume_group_id = %[2]q
%[3]s}
`, name, volumeGroupID, extra)
}

func TestSnapshotResource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	t.Run("creates a snapshot, reads its member volumes back, and then plans empty", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		config := snapshotConfig("snap1", testVolumeGroupID("vg1"), "  deletion_protection = false\n")

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_snapshot.test", "name", "snap1"),
						resource.TestCheckResourceAttr("everpure_azure_snapshot.test", "id", testVolumeGroupSnapshotID("vg1", "snap1")),
						resource.TestCheckResourceAttrSet("everpure_azure_snapshot.test", "created_at"),
						resource.TestCheckNoResourceAttr("everpure_azure_snapshot.test", "source_recoverable_snapshot_id"),
						// Only a single GET fills the member volumes in, so seeing them
						// in state proves Create read the snapshot back.
						resource.TestCheckResourceAttr("everpure_azure_snapshot.test", "volume_snapshots.#", "1"),
						resource.TestCheckResourceAttr("everpure_azure_snapshot.test", "volume_snapshots.0.volume_name", "member-vol"),
						resource.TestCheckResourceAttr("everpure_azure_snapshot.test", "volume_snapshots.0.provisioned_size_in_bytes", "1073741824"),
						resource.TestCheckResourceAttr("everpure_azure_snapshot.test", "volume_snapshots.0.serial_number", "FAKESERIAL00000000000001"),
					),
				},
				{Config: config, PlanOnly: true},
			},
		})

		if store.lastCreate == nil {
			t.Fatal("expected a captured create body")
		}
		if store.lastCreate.Properties != nil {
			t.Fatalf("got create properties %+v, want none for a plain create", store.lastCreate.Properties)
		}
	})

	t.Run("recovers a soft-deleted snapshot under a new name, and the soft-deleted copy is gone", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		twinName := "snap1-20260901000000"
		store.seedDestroyedSnapshot(testResourceGroup, testStoragePoolName, "vg1", twinName, "snap1", mustParseTime(t, "2026-09-01T00:00:00Z"), false)
		sourceID := testVolumeGroupSnapshotID("vg1", twinName)

		config := snapshotConfig("recovered", testVolumeGroupID("vg1"),
			fmt.Sprintf("  deletion_protection            = false\n  source_recoverable_snapshot_id = %q\n", sourceID))

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_snapshot.test", "name", "recovered"),
						resource.TestCheckResourceAttr("everpure_azure_snapshot.test", "id", testVolumeGroupSnapshotID("vg1", "recovered")),
						resource.TestCheckResourceAttr("everpure_azure_snapshot.test", "source_recoverable_snapshot_id", sourceID),
						resource.TestCheckResourceAttr("everpure_azure_snapshot.test", "volume_snapshots.#", "1"),
					),
				},
				// A second, unchanged plan proves Read preserved the create-only
				// source from state: the API never returns it.
				{Config: config, PlanOnly: true},
			},
		})

		props := store.lastCreate.Properties
		if props == nil || props.SourceSnapshotResourceID == nil || *props.SourceSnapshotResourceID != sourceID {
			t.Fatalf("got create properties %+v, want sourceSnapshotResourceId %s", props, sourceID)
		}
	})

	t.Run("replacing a recovered snapshot checks the recovery source again, which the first recovery consumed", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		twinName := "snap1-20260901000000"
		store.seedDestroyedSnapshot(testResourceGroup, testStoragePoolName, "vg1", twinName, "snap1", mustParseTime(t, "2026-09-01T00:00:00Z"), false)
		sourceID := testVolumeGroupSnapshotID("vg1", twinName)
		extra := fmt.Sprintf("  deletion_protection            = false\n  source_recoverable_snapshot_id = %q\n", sourceID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: snapshotConfig("recovered", testVolumeGroupID("vg1"), extra)},
				{
					Config:   snapshotConfig("recovered-again", testVolumeGroupID("vg1"), extra),
					PlanOnly: true,
					ExpectError: diagRegexp(diag.WithPath(path.Root("source_recoverable_snapshot_id"), invalidAttributeValueDiagnostic(
						newRecoverySourceNotFoundError(sourceID, apiFailure{})))),
				},
			},
		})
	})

	t.Run("imports a snapshot by its full ARM ID", func(t *testing.T) {
		factories, _ := newTestSnapshotProviderFactories()

		// deletion_protection is left at its schema default (true): ImportState always
		// sets it to true, so leaving the default here is what lets the pre-import apply
		// and the imported state agree.
		config := snapshotConfig("snap1", testVolumeGroupID("vg1"), "")
		unprotected := snapshotConfig("snap1", testVolumeGroupID("vg1"), "  deletion_protection = false\n")

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: config},
				{
					Config:            config,
					ResourceName:      "everpure_azure_snapshot.test",
					ImportState:       true,
					ImportStateVerify: true,
					ImportStateId:     testVolumeGroupSnapshotID("vg1", "snap1"),
				},
				{Config: unprotected},
			},
		})
	})

	// Import-shaped: ImportState composes volume_group_id via
	// parsed.VolumeGroup().FullID(), which always renders ARM's canonical keyword
	// casing, whatever casing the import ID used. A config that spells volume_group_id
	// with different (but ARM-equivalent) casing must still plan no changes after import.
	t.Run("an imported snapshot plans no changes when config spells volume_group_id with different casing", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap1", mustParseTime(t, "2026-09-01T00:00:00Z"), false)

		mangledVGID := fmt.Sprintf(
			"/Subscriptions/%s/ResourceGroups/%s/providers/purestorage.block/StoragePools/%s/VolumeGroups/vg1",
			testSubscriptionID, testResourceGroup, testStoragePoolName,
		)
		mangledSnapshotID := mangledVGID + "/Snapshots/snap1"

		config := snapshotConfig("snap1", mangledVGID, "")
		unprotected := snapshotConfig("snap1", mangledVGID, "  deletion_protection = false\n")

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config:             config,
					ResourceName:       "everpure_azure_snapshot.test",
					ImportState:        true,
					ImportStatePersist: true,
					ImportStateId:      mangledSnapshotID,
				},
				{Config: config, PlanOnly: true},
				// Unprotect so the harness's own post-test destroy can succeed.
				{Config: unprotected},
			},
		})
	})

	t.Run("a snapshot taken by the snapshot schedule cannot be imported", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "17", mustParseTime(t, "2026-09-01T00:00:00Z"), true)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config:        snapshotConfig("snap1", testVolumeGroupID("vg1"), ""),
					ResourceName:  "everpure_azure_snapshot.test",
					ImportState:   true,
					ImportStateId: testVolumeGroupSnapshotID("vg1", "17"),
					ExpectError:   causeRegexp(newSnapshotOwnedByScheduleError(testVolumeGroupSnapshotID("vg1", "17"))),
				},
			},
		})
	})

	t.Run("drift", func(t *testing.T) {
		t.Run("snapshot removed outside Terraform plans a recreate", func(t *testing.T) {
			factories, store := newTestSnapshotProviderFactories()
			config := snapshotConfig("snap1", testVolumeGroupID("vg1"), "  deletion_protection = false\n")

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: config},
					{
						PreConfig: func() {
							store.removeSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap1")
						},
						Config:             config,
						PlanOnly:           true,
						ExpectNonEmptyPlan: true,
					},
				},
			})
		})

		t.Run("snapshot destroyed outside Terraform plans a recreate", func(t *testing.T) {
			factories, store := newTestSnapshotProviderFactories()
			config := snapshotConfig("snap1", testVolumeGroupID("vg1"), "  deletion_protection = false\n")

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: config},
					{
						// A GET by the original name still answers 200 here, with
						// softDeletion.destroyed = true: Read has to treat that as gone.
						PreConfig: func() {
							store.destroySnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap1")
						},
						Config:             config,
						PlanOnly:           true,
						ExpectNonEmptyPlan: true,
					},
				},
			})
		})
	})

	t.Run("destroys as a soft delete by default, leaving the renamed entry", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		config := snapshotConfig("snap1", testVolumeGroupID("vg1"), "  deletion_protection = false\n")

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps:                    []resource.TestStep{{Config: config}},
		})

		twins := snapshotsUnderPrefix(store, testResourceGroup, testStoragePoolName, "vg1")
		if len(twins) != 1 {
			t.Fatalf("got %d entries left in the volume group, want 1 (soft-deleted, not eradicated)", len(twins))
		}
		if twins[0].Properties == nil || twins[0].Properties.SoftDeletion == nil ||
			twins[0].Properties.SoftDeletion.PreviousName == nil || *twins[0].Properties.SoftDeletion.PreviousName != "snap1" {
			t.Fatalf("expected the surviving entry to be snap1's renamed twin, got %+v", twins[0])
		}
		if store.deleteCallCount != 1 {
			t.Fatalf("got %d delete calls, want exactly 1", store.deleteCallCount)
		}
	})

	t.Run("destroys with eradicate_on_destroy = true", func(t *testing.T) {
		t.Run("eradicates only the twin this delete just created, ignoring decoys", func(t *testing.T) {
			factories, store := newTestSnapshotProviderFactories()

			// Both decoys exist before this test's delete ever runs, so set-difference
			// selection excludes them regardless of name or age: one under a different
			// previousName, and one already sharing this snapshot's own previousName.
			store.seedDestroyedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "other-snap-20260101000000", "other-snap", mustParseTime(t, "2026-01-01T00:00:00Z"), false)
			store.seedDestroyedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap1-20260101000000", "snap1", mustParseTime(t, "2026-01-01T00:00:00Z"), false)

			config := snapshotConfig("snap1", testVolumeGroupID("vg1"), "  deletion_protection = false\n  eradicate_on_destroy = true\n")

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps:                    []resource.TestStep{{Config: config}},
			})

			twins := snapshotsUnderPrefix(store, testResourceGroup, testStoragePoolName, "vg1")
			if len(twins) != 2 {
				t.Fatalf("got %d entries left, want 2 (both decoys; the freshly created twin was eradicated)", len(twins))
			}
			for _, twin := range twins {
				if *twin.Name != "other-snap-20260101000000" && *twin.Name != "snap1-20260101000000" {
					t.Fatalf("unexpected entry remaining: %s", *twin.Name)
				}
			}
		})

		t.Run("finds the twin past the first page of the listing", func(t *testing.T) {
			factories, store := newTestSnapshotProviderFactories()
			// Newer than the snapshot the test creates, so newest-first puts its twin after
			// all of them, beyond the first 100-entry page.
			future := time.Now().Add(time.Hour)
			for i := range 150 {
				store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", fmt.Sprintf("newer-%03d", i), future, false)
			}

			config := snapshotConfig("snap1", testVolumeGroupID("vg1"), "  deletion_protection = false\n  eradicate_on_destroy = true\n")
			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps:                    []resource.TestStep{{Config: config}},
			})

			for _, s := range snapshotsUnderPrefix(store, testResourceGroup, testStoragePoolName, "vg1") {
				if strings.HasPrefix(*s.Name, "snap1") {
					t.Fatalf("%s was left behind; want the twin eradicated", *s.Name)
				}
			}
		})

		// The plugin-testing harness does not surface a destroy's warnings, so this case
		// proves only that a missing twin does not fail the destroy; the warning itself is
		// asserted by TestSnapshotDeleteNoTwinWarns.
		t.Run("does not fail the destroy when no twin matches", func(t *testing.T) {
			factories, store := newTestSnapshotProviderFactories()
			// Simulates auto-eradication (or someone else) having already won the race
			// by the time this destroy's eradicate step looks for a twin.
			store.skipTwinOnDelete = true

			config := snapshotConfig("snap1", testVolumeGroupID("vg1"), "  deletion_protection = false\n  eradicate_on_destroy = true\n")

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: config},
					// A destroy; if the missing twin surfaced as a hard error instead of
					// a warning, this step would fail.
					{Config: providerBlock()},
				},
			})
		})
	})

	t.Run("deletion_protection default (true) blocks both a destroy plan and a replace-forcing plan", func(t *testing.T) {
		t.Run("destroy", func(t *testing.T) {
			factories, _ := newTestSnapshotProviderFactories()

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: snapshotConfig("snap1", testVolumeGroupID("vg1"), "")},
					{
						Config:      providerBlock(),
						ExpectError: deletionProtectedRegexp(kindSnapshot),
					},
					// Unprotect so the harness's own post-test destroy can succeed.
					{Config: snapshotConfig("snap1", testVolumeGroupID("vg1"), "  deletion_protection = false\n")},
				},
			})
		})

		t.Run("replace", func(t *testing.T) {
			factories, _ := newTestSnapshotProviderFactories()

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: snapshotConfig("snap1", testVolumeGroupID("vg1"), "")},
					{
						Config:      snapshotConfig("snap2", testVolumeGroupID("vg1"), ""),
						ExpectError: deletionProtectedReplaceRegexp(kindSnapshot, "name"),
					},
					{Config: snapshotConfig("snap1", testVolumeGroupID("vg1"), "  deletion_protection = false\n")},
				},
			})
		})
	})

	t.Run("changing only deletion_protection, eradicate_on_destroy or timeouts makes no API call", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()

		base := snapshotConfig("snap1", testVolumeGroupID("vg1"), "  deletion_protection = true\n  eradicate_on_destroy = false\n")
		changed := snapshotConfig("snap1", testVolumeGroupID("vg1"), `  deletion_protection  = false
  eradicate_on_destroy = true

  timeouts {
    create = "30m"
  }
`)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: base},
				{
					Config: changed,
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction("everpure_azure_snapshot.test", plancheck.ResourceActionUpdate),
						},
					},
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("everpure_azure_snapshot.test", "deletion_protection", "false"),
						resource.TestCheckResourceAttr("everpure_azure_snapshot.test", "eradicate_on_destroy", "true"),
						resource.TestCheckResourceAttr("everpure_azure_snapshot.test", "timeouts.create", "30m"),
						// Checked here, before the harness's own destroy: neither a
						// second PUT nor a DELETE has been sent by the in-place update.
						func(*terraform.State) error {
							if store.createCallCount != 1 || store.deleteCallCount != 0 {
								return fmt.Errorf("got %d creates and %d deletes, want 1 and 0", store.createCallCount, store.deleteCallCount)
							}
							return nil
						},
						// The computed attributes survive the in-place update.
						resource.TestCheckResourceAttrSet("everpure_azure_snapshot.test", "created_at"),
						resource.TestCheckResourceAttr("everpure_azure_snapshot.test", "volume_snapshots.#", "1"),
					),
				},
				{Config: changed, PlanOnly: true},
			},
		})
	})

	t.Run("changing volume_group_id or name forces replacement", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: snapshotConfig("snap1", testVolumeGroupID("vg1"), "  deletion_protection = false\n")},
				{
					Config: snapshotConfig("snap1", testVolumeGroupID("vg2"), "  deletion_protection = false\n"),
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction("everpure_azure_snapshot.test", plancheck.ResourceActionDestroyBeforeCreate),
						},
					},
				},
			},
		})

		if store.createCallCount != 2 {
			t.Fatalf("got %d creates, want 2 (one per volume group)", store.createCallCount)
		}
	})

	t.Run("rejects an invalid recovery source", func(t *testing.T) {
		const sourceVG = "vg1"
		destroyedAt := mustParseTime(t, "2026-09-01T00:00:00Z")

		tests := []struct {
			name string
			// seed prepares the store; source is the recovery source ID in the config.
			seed        func(*fakeSnapshotStore)
			volumeGroup string
			source      string
			wantError   *regexp.Regexp
			// wantNoGet asserts the rejection came from the check that reads only the
			// IDs, so not even a GET was sent.
			wantNoGet bool
		}{
			{
				name:        "in another volume group, rejected at plan time without an API call",
				seed:        func(s *fakeSnapshotStore) {},
				volumeGroup: "vg1",
				source:      testVolumeGroupSnapshotID("vg-other", "snap1-20260901000000"),
				wantError: diagRegexp(diag.WithPath(path.Root("source_recoverable_snapshot_id"), invalidAttributeCombinationDiagnostic(
					newRecoverySourceInOtherVolumeGroupError(path.Root("source_recoverable_snapshot_id"), kindSnapshot, testVolumeGroupID("vg-other"), testVolumeGroupID("vg1"))))),
				wantNoGet: true,
			},
			{
				name: "created by the snapshot schedule",
				seed: func(s *fakeSnapshotStore) {
					s.seedDestroyedSnapshot(testResourceGroup, testStoragePoolName, sourceVG, "17-20260901000000", "17", destroyedAt, true)
				},
				volumeGroup: sourceVG,
				source:      testVolumeGroupSnapshotID(sourceVG, "17-20260901000000"),
				wantError: diagRegexp(diag.WithPath(path.Root("source_recoverable_snapshot_id"), invalidAttributeValueDiagnostic(
					newRecoverySourceCreatedByScheduleError(testVolumeGroupSnapshotID(sourceVG, "17-20260901000000"))))),
			},
			{
				name: "still live",
				seed: func(s *fakeSnapshotStore) {
					s.seedSnapshot(testResourceGroup, testStoragePoolName, sourceVG, "live", destroyedAt, false)
				},
				volumeGroup: sourceVG,
				source:      testVolumeGroupSnapshotID(sourceVG, "live"),
				wantError: diagRegexp(diag.WithPath(path.Root("source_recoverable_snapshot_id"), invalidAttributeValueDiagnostic(
					newRecoverySourceNotSoftDeletedError(testVolumeGroupSnapshotID(sourceVG, "live"))))),
			},
			{
				name:        "missing",
				seed:        func(s *fakeSnapshotStore) {},
				volumeGroup: sourceVG,
				source:      testVolumeGroupSnapshotID(sourceVG, "gone-20260901000000"),
				wantError: diagRegexp(diag.WithPath(path.Root("source_recoverable_snapshot_id"), invalidAttributeValueDiagnostic(
					newRecoverySourceNotFoundError(testVolumeGroupSnapshotID(sourceVG, "gone-20260901000000"), apiFailure{})))),
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				factories, store := newTestSnapshotProviderFactories()
				tt.seed(store)

				resource.Test(t, resource.TestCase{
					IsUnitTest:               true,
					ProtoV6ProviderFactories: factories,
					Steps: []resource.TestStep{{
						Config: snapshotConfig("recovered", testVolumeGroupID(tt.volumeGroup),
							fmt.Sprintf("  deletion_protection            = false\n  source_recoverable_snapshot_id = %q\n", tt.source)),
						PlanOnly:    true,
						ExpectError: tt.wantError,
					}},
				})

				if tt.wantNoGet && store.getCallCount != 0 {
					t.Fatalf("got %d gets, want 0: this rejection must not call the API", store.getCallCount)
				}
				if store.createCallCount != 0 {
					t.Fatalf("got %d creates, want 0: the plan must be rejected before anything is sent", store.createCallCount)
				}
			})
		}
	})

	// The source id comes from another resource's output, so it is unknown at plan time
	// and ModifyPlan cannot check it. The check must still run at apply, before the
	// create request is sent.
	t.Run("a recovery source unknown at plan time is still checked before anything is sent", func(t *testing.T) {
		destroyedAt := mustParseTime(t, "2026-09-01T00:00:00Z")

		tests := []struct {
			name      string
			seed      func(*fakeSnapshotStore)
			source    string
			wantError *regexp.Regexp
		}{
			{
				name: "created by the snapshot schedule",
				seed: func(s *fakeSnapshotStore) {
					s.seedDestroyedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "17-20260901000000", "17", destroyedAt, true)
				},
				source: testVolumeGroupSnapshotID("vg1", "17-20260901000000"),
				wantError: diagRegexp(diag.WithPath(path.Root("source_recoverable_snapshot_id"), invalidAttributeValueDiagnostic(
					newRecoverySourceCreatedByScheduleError(testVolumeGroupSnapshotID("vg1", "17-20260901000000"))))),
			},
			{
				name:   "in another volume group",
				seed:   func(s *fakeSnapshotStore) {},
				source: testVolumeGroupSnapshotID("vg-other", "snap1-20260901000000"),
				wantError: diagRegexp(diag.WithPath(path.Root("source_recoverable_snapshot_id"), invalidAttributeCombinationDiagnostic(
					newRecoverySourceInOtherVolumeGroupError(path.Root("source_recoverable_snapshot_id"), kindSnapshot, testVolumeGroupID("vg-other"), testVolumeGroupID("vg1"))))),
			},
			{
				name: "still live",
				seed: func(s *fakeSnapshotStore) {
					s.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "live", destroyedAt, false)
				},
				source: testVolumeGroupSnapshotID("vg1", "live"),
				wantError: diagRegexp(diag.WithPath(path.Root("source_recoverable_snapshot_id"), invalidAttributeValueDiagnostic(
					newRecoverySourceNotSoftDeletedError(testVolumeGroupSnapshotID("vg1", "live"))))),
			},
			{
				name:   "missing",
				seed:   func(s *fakeSnapshotStore) {},
				source: testVolumeGroupSnapshotID("vg1", "gone-20260901000000"),
				wantError: diagRegexp(diag.WithPath(path.Root("source_recoverable_snapshot_id"), invalidAttributeValueDiagnostic(
					newRecoverySourceNotFoundError(testVolumeGroupSnapshotID("vg1", "gone-20260901000000"), apiFailure{})))),
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				factories, store := newTestSnapshotProviderFactories()
				tt.seed(store)

				config := providerBlock() + fmt.Sprintf(`
resource "terraform_data" "source" {
  input = %[1]q
}

resource "everpure_azure_snapshot" "test" {
  name                           = "recovered"
  volume_group_id                = %[2]q
  deletion_protection            = false
  source_recoverable_snapshot_id = terraform_data.source.output
}
`, tt.source, testVolumeGroupID("vg1"))

				resource.Test(t, resource.TestCase{
					IsUnitTest:               true,
					ProtoV6ProviderFactories: factories,
					Steps: []resource.TestStep{{
						Config:      config,
						ExpectError: tt.wantError,
					}},
				})

				if store.createCallCount != 0 {
					t.Fatalf("got %d creates, want 0: the apply-time check must reject the source before sending a PUT", store.createCallCount)
				}
			})
		}
	})

	t.Run("a failed create poll leaves a tainted resource, and the next apply recovers", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		store.failCreatePoll = true
		failing := snapshotConfig("snap1", testVolumeGroupID("vg1"), "  deletion_protection = false\n")
		// The fake transport keeps one poller per request URL, and a failed poller is
		// not cleared, so the retry goes to a different name. That still exercises the
		// path under test: the tainted snap1 reads back as missing, is dropped from
		// state, and the next apply creates what the new config asks for.
		retry := snapshotConfig("snap2", testVolumeGroupID("vg1"), "  deletion_protection = false\n")

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config:      failing,
					ExpectError: summaryRegexp(operation{action: actionCreate, kind: kindSnapshot, name: "snap1"}.fail(detailError{})),
				},
				{
					PreConfig: func() {
						store.mu.Lock()
						defer store.mu.Unlock()
						store.failCreatePoll = false
					},
					Config: retry,
					Check:  resource.TestCheckResourceAttr("everpure_azure_snapshot.test", "id", testVolumeGroupSnapshotID("vg1", "snap2")),
				},
				{Config: retry, PlanOnly: true},
			},
		})

		if store.createCallCount != 2 {
			t.Fatalf("got %d creates, want 2 (the failed one and the retry)", store.createCallCount)
		}
	})

	t.Run("rejects invalid configuration at plan time", func(t *testing.T) {
		tests := []struct {
			name      string
			extra     string
			snapName  string
			vgID      string
			wantError *regexp.Regexp
		}{
			{name: "name starting with a digit", snapName: "1snap", vgID: testVolumeGroupID("vg1"), wantError: regexp.MustCompile(`must be 2-40\s+characters`)},
			{name: "name too short", snapName: "s", vgID: testVolumeGroupID("vg1"), wantError: regexp.MustCompile(`must be 2-40\s+characters`)},
			{name: "name longer than 40 characters", snapName: name40 + "x", vgID: testVolumeGroupID("vg1"), wantError: regexp.MustCompile(`must be 2-40\s+characters`)},
			{name: "volume_group_id that is not a volume group", snapName: "snap1", vgID: "not-an-id", wantError: regexp.MustCompile(`ID\s+of\s+a\s+volume\s+group,`)},
			{
				name: "source_recoverable_snapshot_id that is not a snapshot", snapName: "snap1", vgID: testVolumeGroupID("vg1"),
				extra:     fmt.Sprintf("  source_recoverable_snapshot_id = %q\n", testVolumeGroupID("vg1")),
				wantError: regexp.MustCompile(`ID\s+of\s+a\s+volume\s+group\s+snapshot`),
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				factories, _ := newTestSnapshotProviderFactories()
				resource.Test(t, resource.TestCase{
					IsUnitTest:               true,
					ProtoV6ProviderFactories: factories,
					Steps: []resource.TestStep{{
						Config:      snapshotConfig(tt.snapName, tt.vgID, tt.extra),
						PlanOnly:    true,
						ExpectError: tt.wantError,
					}},
				})
			})
		}
	})
}

func TestSnapshotResourceImportOtherSubscription(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}
	factories, _ := newTestSnapshotProviderFactories()

	importID := fmt.Sprintf(
		"/subscriptions/22222222-2222-2222-2222-222222222222/resourceGroups/%s/providers/"+
			"PureStorage.Block/storagePools/%s/volumeGroups/vg1/snapshots/snap1",
		testResourceGroup, testStoragePoolName,
	)
	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config:        snapshotConfig("snap1", testVolumeGroupID("vg1"), ""),
				ResourceName:  "everpure_azure_snapshot.test",
				ImportState:   true,
				ImportStateId: importID,
				ExpectError:   subscriptionMismatchRegexp(kindSnapshot, importID),
			},
		},
	})
}

// newTestSnapshotDirectResource builds a SnapshotResource wired to a fresh
// fakeSnapshotStore through a direct Configure call (bypassing the provider server), for
// tests that drive a method directly rather than through terraform-plugin-testing.
func newTestSnapshotDirectResource(t *testing.T) (*SnapshotResource, *fakeSnapshotStore) {
	t.Helper()

	store := newFakeSnapshotStore(testSubscriptionID)
	srv := store.newVolumeGroupSnapshotsServer()
	transport := psbfake.NewVolumeGroupSnapshotsServerTransport(&srv)
	opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: transport}}

	r := &SnapshotResource{}
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

func newTestSnapshotDeleteState(t *testing.T, r *SnapshotResource, overrides func(*SnapshotResourceModel)) tfsdk.State {
	t.Helper()

	ctx := context.Background()
	var schemaResp fwresource.SchemaResponse
	r.Schema(ctx, fwresource.SchemaRequest{}, &schemaResp)

	data := baseSnapshotResourceModel()
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

// TestSnapshotDeleteEradicatesDespiteCaseDivergence proves eradicate_on_destroy does not
// silently skip eradication when state's copy of the name differs in letter case from the
// server's stored spelling (e.g. after importing with a different case): the twin is
// matched by the server's own spelling. The fake looks entries up by exact name and
// builds the twin's name from the stored spelling, an assumption about the backend this
// path must tolerate rather than a verified behaviour.
func TestSnapshotDeleteEradicatesDespiteCaseDivergence(t *testing.T) {
	r, store := newTestSnapshotDirectResource(t)

	store.snapshots[snapshotKey(testResourceGroup, testStoragePoolName, "vg1", "snap1")] = &armpurestorageblock.VolumeGroupSnapshot{
		ID:   to.Ptr(store.fakeSnapshotID(testResourceGroup, testStoragePoolName, "vg1", "Snap1")),
		Name: to.Ptr("Snap1"),
		Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{
			CreatedAt: to.Ptr(time.Now().UTC()),
		},
	}

	state := newTestSnapshotDeleteState(t, r, nil)
	deleteResp := &fwresource.DeleteResponse{}
	r.Delete(context.Background(), fwresource.DeleteRequest{State: state}, deleteResp)

	if len(deleteResp.Diagnostics) != 0 {
		t.Fatalf("expected no diagnostics at all (a warning would mean eradication was skipped), got %v", deleteResp.Diagnostics)
	}
	if remaining := snapshotsUnderPrefix(store, testResourceGroup, testStoragePoolName, "vg1"); len(remaining) != 0 {
		t.Fatalf("got %d entries left, want 0 (the twin must have been eradicated): %+v", len(remaining), remaining)
	}
}

// TestSnapshotDeleteAmbiguousRemovalSkipsEradication covers concurrent destroys in the
// same volume group: when more than one snapshot disappears between the two listings and
// narrowing by state's own name still leaves more than one candidate, eradication must
// skip and warn rather than guess.
func TestSnapshotDeleteAmbiguousRemovalSkipsEradication(t *testing.T) {
	r, store := newTestSnapshotDirectResource(t)

	store.snapshots[snapshotKey(testResourceGroup, testStoragePoolName, "vg1", "snap1")] = &armpurestorageblock.VolumeGroupSnapshot{
		Name:       to.Ptr("snap1"),
		Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{},
	}
	store.snapshots[snapshotKey(testResourceGroup, testStoragePoolName, "vg1", "SNAP1")] = &armpurestorageblock.VolumeGroupSnapshot{
		Name:       to.Ptr("SNAP1"),
		Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{},
	}
	store.skipTwinOnDelete = true

	state := newTestSnapshotDeleteState(t, r, nil)
	deleteResp := &fwresource.DeleteResponse{}
	r.Delete(context.Background(), fwresource.DeleteRequest{State: state}, deleteResp)

	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("unexpected error diagnostics: %v", deleteResp.Diagnostics)
	}
	if len(deleteResp.Diagnostics) != 1 {
		t.Fatalf("expected exactly one warning, got %v", deleteResp.Diagnostics)
	}
	requireContains(t, deleteResp.Diagnostics[0].Summary(), softDeletedTwinNotFoundWarning(kindSnapshot, "snap1").Summary())
	requireContains(t, deleteResp.Diagnostics[0].Detail(), `"snap1"`)

	remaining := snapshotsUnderPrefix(store, testResourceGroup, testStoragePoolName, "vg1")
	if len(remaining) != 1 || *remaining[0].Name != "SNAP1" {
		t.Fatalf("got %+v, want only SNAP1 left untouched", remaining)
	}
}

// TestSnapshotImportStateRejectsPolicySnapshot drives ImportState directly: a snapshot
// the API reports as created by the volume group's snapshot schedule is refused, and a
// manual one is accepted with the provider-side defaults set.
func TestSnapshotImportStateRejectsPolicySnapshot(t *testing.T) {
	ctx := context.Background()
	r, store := newTestSnapshotDirectResource(t)
	createdAt := mustParseTime(t, "2026-09-01T00:00:00Z")
	store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "17", createdAt, true)
	store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "manual", createdAt, false)

	var schemaResp fwresource.SchemaResponse
	r.Schema(ctx, fwresource.SchemaRequest{}, &schemaResp)
	newResp := func() *fwresource.ImportStateResponse {
		return &fwresource.ImportStateResponse{
			State: tfsdk.State{
				Schema: schemaResp.Schema,
				Raw:    tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), nil),
			},
			Identity: nullIdentity(t, armids.VolumeGroupSnapshotIdentitySchema()),
		}
	}

	t.Run("policy snapshot: rejected, pointing at the data sources", func(t *testing.T) {
		resp := newResp()
		r.ImportState(ctx, fwresource.ImportStateRequest{ID: testVolumeGroupSnapshotID("vg1", "17")}, resp)
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an error")
		}
		want := operation{action: actionImport, kind: kindSnapshot, unnamed: true}.fail(newSnapshotOwnedByScheduleError(testVolumeGroupSnapshotID("vg1", "17")))
		if !resp.Diagnostics.Contains(want) {
			t.Fatalf("diagnostics = %v, want %v", resp.Diagnostics, want)
		}
	})

	t.Run("manual snapshot: accepted with the provider-side defaults", func(t *testing.T) {
		resp := newResp()
		r.ImportState(ctx, fwresource.ImportStateRequest{ID: testVolumeGroupSnapshotID("vg1", "manual")}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		var got SnapshotResourceModel
		if diags := resp.State.Get(ctx, &got); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if got.Name.ValueString() != "manual" || !got.DeletionProtection.ValueBool() || got.EradicateOnDestroy.ValueBool() {
			t.Fatalf("got %+v", got)
		}
		if !got.SourceRecoverableSnapshotID.IsNull() {
			t.Fatalf("source_recoverable_snapshot_id = %v, want null after import", got.SourceRecoverableSnapshotID)
		}
	})

	t.Run("a missing snapshot is left for Read to report", func(t *testing.T) {
		resp := newResp()
		r.ImportState(ctx, fwresource.ImportStateRequest{ID: testVolumeGroupSnapshotID("vg1", "gone")}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
	})

	t.Run("malformed ID", func(t *testing.T) {
		resp := newResp()
		r.ImportState(ctx, fwresource.ImportStateRequest{ID: "not-an-id"}, resp)
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an error")
		}
		requireContains(t, resp.Diagnostics[0].Summary(), operation{action: actionImport, kind: kindSnapshot, unnamed: true}.fail(detailError{}).Summary())
	})
}

// TestSnapshotDeleteNoTwinWarns covers eradicate_on_destroy when the soft delete leaves no
// twin to find: the destroy succeeds with a warning instead of failing.
func TestSnapshotDeleteNoTwinWarns(t *testing.T) {
	r, store := newTestSnapshotDirectResource(t)
	store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap1", mustParseTime(t, "2026-09-01T00:00:00Z"), false)
	store.skipTwinOnDelete = true

	state := newTestSnapshotDeleteState(t, r, nil)
	deleteResp := &fwresource.DeleteResponse{}
	r.Delete(context.Background(), fwresource.DeleteRequest{State: state}, deleteResp)

	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("unexpected error diagnostics: %v", deleteResp.Diagnostics)
	}
	if len(deleteResp.Diagnostics) != 1 {
		t.Fatalf("expected exactly one warning, got %v", deleteResp.Diagnostics)
	}
	requireContains(t, deleteResp.Diagnostics[0].Summary(), softDeletedTwinNotFoundWarning(kindSnapshot, "snap1").Summary())
	requireContains(t, deleteResp.Diagnostics[0].Detail(), `"snap1"`)
}

// TestSnapshotAPIErrors drives Read and Delete directly against a fake that fails with a
// non-404 status, which must surface as an error rather than as a vanished resource or
// a swallowed failure.
func TestSnapshotAPIErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("Read: a non-404 error is reported and the resource stays in state", func(t *testing.T) {
		r, store := newTestSnapshotDirectResource(t)
		store.getFailStatus = 500

		state := newTestSnapshotDeleteState(t, r, nil)
		resp := &fwresource.ReadResponse{State: state}
		r.Read(ctx, fwresource.ReadRequest{State: state}, resp)

		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an error")
		}
		if resp.State.Raw.IsNull() {
			t.Fatal("a failed read must not remove the resource from state")
		}
	})

	t.Run("Delete: a non-404 error on the soft delete is reported", func(t *testing.T) {
		r, store := newTestSnapshotDirectResource(t)
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap1", mustParseTime(t, "2026-09-01T00:00:00Z"), false)
		store.deleteFailOnCall = 1

		state := newTestSnapshotDeleteState(t, r, func(m *SnapshotResourceModel) { m.EradicateOnDestroy = types.BoolValue(false) })
		resp := &fwresource.DeleteResponse{}
		r.Delete(ctx, fwresource.DeleteRequest{State: state}, resp)

		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an error")
		}
		requireContains(t, resp.Diagnostics[0].Summary(), operation{action: actionDelete, kind: kindSnapshot, name: "snap1"}.fail(detailError{}).Summary())
	})

	t.Run("Delete: a failed eradication of the twin is reported", func(t *testing.T) {
		r, store := newTestSnapshotDirectResource(t)
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap1", mustParseTime(t, "2026-09-01T00:00:00Z"), false)
		store.deleteFailOnCall = 2

		state := newTestSnapshotDeleteState(t, r, nil)
		resp := &fwresource.DeleteResponse{}
		r.Delete(ctx, fwresource.DeleteRequest{State: state}, resp)

		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an error")
		}
		requireContains(t, resp.Diagnostics[0].Summary(), operation{action: actionEradicate, kind: kindSnapshot, name: "snap1"}.fail(detailError{}).Summary())
		if store.deleteCallCount != 2 {
			t.Fatalf("got %d delete calls, want 2 (soft delete, then the failed eradication)", store.deleteCallCount)
		}
	})
}

// TestSnapshotCreateSubscriptionMismatch covers the subscription guards on Create and on
// the recovery-source check. A source in another subscription cannot reach the check
// through Create (its volume group would differ from volume_group_id first), so
// checkRecoverySource is driven directly.
func TestSnapshotCreateSubscriptionMismatch(t *testing.T) {
	ctx := context.Background()
	const otherSubscription = "22222222-2222-2222-2222-222222222222"
	otherVolumeGroupID := armids.VolumeGroupID{
		ResourceGroupID: armids.ResourceGroupID{SubscriptionID: otherSubscription, ResourceGroupName: testResourceGroup},
		StoragePoolName: testStoragePoolName,
		VolumeGroupName: "vg1",
	}.FullID()

	t.Run("Create: volume_group_id in another subscription", func(t *testing.T) {
		r, store := newTestSnapshotDirectResource(t)
		var schemaResp fwresource.SchemaResponse
		r.Schema(ctx, fwresource.SchemaRequest{}, &schemaResp)

		data := baseSnapshotResourceModel()
		data.VolumeGroupID = armids.IDValue{StringValue: types.StringValue(otherVolumeGroupID)}
		plan := tfsdk.Plan{Schema: schemaResp.Schema}
		if diags := plan.Set(ctx, &data); diags.HasError() {
			t.Fatalf("unexpected diagnostics building plan: %v", diags)
		}
		resp := &fwresource.CreateResponse{State: tfsdk.State{Schema: schemaResp.Schema}}
		r.Create(ctx, fwresource.CreateRequest{Plan: plan}, resp)

		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an error")
		}
		op := operation{action: actionCreate, kind: kindSnapshot, name: data.Name.ValueString()}
		want := diag.WithPath(path.Root("volume_group_id"), op.fail(
			newSubscriptionMismatchError(kindVolumeGroup, otherVolumeGroupID, otherSubscription, testSubscriptionID)))
		requireContains(t, resp.Diagnostics[0].Summary(), want.Summary())
		requireContains(t, resp.Diagnostics[0].Detail(), want.Detail())
		if store.createCallCount != 0 {
			t.Fatalf("got %d creates, want 0", store.createCallCount)
		}
	})

	t.Run("checkRecoverySource: source in another subscription", func(t *testing.T) {
		r, store := newTestSnapshotDirectResource(t)

		var diags diag.Diagnostics
		sourceID := otherVolumeGroupID + "/snapshots/snap1-20260901000000"
		if r.checkRecoverySource(ctx, operation{action: actionCreate, kind: kindSnapshot, unnamed: true}, sourceID, &diags) {
			t.Fatal("expected the source to be rejected")
		}
		if !diags.HasError() {
			t.Fatal("expected an error")
		}
		want := diag.WithPath(path.Root("source_recoverable_snapshot_id"), invalidAttributeValueDiagnostic(
			newSubscriptionMismatchError(kindSnapshot, sourceID, otherSubscription, testSubscriptionID)))
		requireContains(t, diags[0].Summary(), want.Summary())
		requireContains(t, diags[0].Detail(), want.Detail())
		if store.getCallCount != 0 {
			t.Fatalf("got %d gets, want 0", store.getCallCount)
		}
	})
}

// TestSnapshotFailedCreateReplacementIsNotBlockedByDeletionProtection covers a create whose
// poll fails after Azure finished creating the object: the tainted snapshot survives refresh,
// so its replacement really runs Delete, which must not be refused by the default
// deletion_protection = true.
func TestSnapshotFailedCreateReplacementIsNotBlockedByDeletionProtection(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	factories, store := newTestSnapshotProviderFactories()
	store.failCreatePollKeepObject = true

	config := snapshotConfig("snap1", testVolumeGroupID("vg1"), "")
	unprotected := snapshotConfig("snap1", testVolumeGroupID("vg1"), "  deletion_protection = false\n")
	const addr = "everpure_azure_snapshot.test"

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: summaryRegexp(operation{action: actionCreate, kind: kindSnapshot, name: "snap1"}.fail(detailError{})),
			},
			{
				PreConfig: func() {
					store.mu.Lock()
					defer store.mu.Unlock()
					store.failCreatePollKeepObject = false
				},
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "id", testVolumeGroupSnapshotID("vg1", "snap1")),
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

// TestSnapshotResourceConfigCasingChange: a case-only respelling of volume_group_id in
// config must plan nothing.
func TestSnapshotResourceConfigCasingChange(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	extra := "  deletion_protection = false\n"
	factories, _ := newTestSnapshotProviderFactories()
	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{Config: snapshotConfig("snap1", testVolumeGroupID("vg1"), extra)},
			{Config: snapshotConfig("snap1", strings.ToUpper(testVolumeGroupID("vg1")), extra), PlanOnly: true},
		},
	})
}

// TestSnapshotDeleteMissingVolumeGroupSucceeds is TestVolumeDeleteMissingVolumeGroupSucceeds
// for a snapshot: the volume group was deleted first and took the snapshot with it.
func TestSnapshotDeleteMissingVolumeGroupSucceeds(t *testing.T) {
	for _, eradicate := range []bool{true, false} {
		t.Run(fmt.Sprintf("eradicate_on_destroy=%t", eradicate), func(t *testing.T) {
			r, store := newTestSnapshotDirectResource(t)
			store.markVolumeGroupMissing(testResourceGroup, testStoragePoolName, "vg1")

			state := newTestSnapshotDeleteState(t, r, func(data *SnapshotResourceModel) {
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
