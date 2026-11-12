// SPDX-License-Identifier: Apache-2.0

package provider

// everpure_azure_snapshots has no input blocks, so the dynamic-block-unknown-list
// regression covered elsewhere in this package does not apply here.

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azcorefake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	psbfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock/fake"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

func TestSnapshotsDataSource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	t.Run("an empty volume group gives an empty list", func(t *testing.T) {
		factories, _ := newTestSnapshotProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_snapshots" "test" {
  volume_group_id = %[1]q
}
`, testVolumeGroupID("vg1")),
					Check: resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.#", "0"),
				},
			},
		})
	})

	t.Run("lists snapshots newest first, without member volumes", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		createdAt := mustParseTime(t, "2026-09-01T00:00:00Z")
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap-c", createdAt.Add(-time.Hour), false)
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap-a", createdAt, false)
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "17", createdAt.Add(time.Hour), true)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_snapshots" "test" {
  volume_group_id = %[1]q
}
`, testVolumeGroupID("vg1")),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.#", "3"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.0.name", "17"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.0.created_by_policy", "true"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.1.name", "snap-a"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.1.created_by_policy", "false"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.2.name", "snap-c"),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_snapshots.test", "snapshots.1.volume_group_id", testVolumeGroupID("vg1"),
						),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_snapshots.test", "snapshots.1.id", testVolumeGroupSnapshotID("vg1", "snap-a"),
						),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.1.created_at", "2026-09-01T00:00:00Z"),
						resource.TestCheckNoResourceAttr("data.everpure_azure_snapshots.test", "snapshots.1.soft_deletion"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.1.space.total_used_in_bytes", "4096"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.1.space.shared_in_bytes", "512"),
						// The list API returns no member volumes, and the element has no
						// attribute for them.
						resource.TestCheckNoResourceAttr("data.everpure_azure_snapshots.test", "snapshots.1.volume_snapshots.#"),
					),
				},
			},
		})
	})

	t.Run("reads every page with $skip, past the API's 100-entry page", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		createdAt := mustParseTime(t, "2026-09-01T00:00:00Z")
		for i := range 205 {
			store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", fmt.Sprintf("snap-%03d", i), createdAt, false)
		}

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_snapshots" "test" {
  volume_group_id = %[1]q
}
`, testVolumeGroupID("vg1")),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.#", "205"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.204.name", "snap-204"),
					),
				},
			},
		})
		if opts := store.lastListOptions; opts == nil || opts.Skip == nil || *opts.Skip != 200 {
			t.Errorf("last list options = %+v, want $skip=200", opts)
		}
	})

	t.Run("a snapshot created between pages does not show up twice", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		createdAt := mustParseTime(t, "2026-09-01T00:00:00Z")
		for i := range 150 {
			store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", fmt.Sprintf("snap-%03d", i), createdAt, false)
		}
		// Newest first, so it lands ahead of everything already read and pushes the first
		// page's last entry (snap-099) onto the second page as well.
		store.beforeListCall = func(call int) {
			if call == 2 {
				store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap-new", createdAt.Add(time.Hour), false)
			}
		}

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_snapshots" "test" {
  volume_group_id = %[1]q
}
`, testVolumeGroupID("vg1")),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.#", "150"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.99.name", "snap-099"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.100.name", "snap-100"),
					),
				},
			},
		})
	})

	t.Run("stops when a page brings nothing new, as from a backend that ignores $skip", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		createdAt := mustParseTime(t, "2026-09-01T00:00:00Z")
		for i := range 150 {
			store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", fmt.Sprintf("snap-%03d", i), createdAt, false)
		}
		store.ignoreSkip = true

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_snapshots" "test" {
  volume_group_id = %[1]q
}
`, testVolumeGroupID("vg1")),
					Check: resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.#", "100"),
				},
			},
		})
	})

	t.Run("split ownership: the snapshot's own attributes match the resource that manages it", func(t *testing.T) {
		factories, _ := newTestSnapshotProviderFactories()

		config := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_snapshot" "test" {
  name                = "snap1"
  volume_group_id     = %[1]q
  deletion_protection = false
}

data "everpure_azure_snapshots" "test" {
  volume_group_id = everpure_azure_snapshot.test.volume_group_id
}
`, testVolumeGroupID("vg1"))

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.#", "1"),
						resource.TestCheckResourceAttrPair(
							"data.everpure_azure_snapshots.test", "snapshots.0.id",
							"everpure_azure_snapshot.test", "id",
						),
						resource.TestCheckResourceAttrPair(
							"data.everpure_azure_snapshots.test", "snapshots.0.created_at",
							"everpure_azure_snapshot.test", "created_at",
						),
					),
				},
			},
		})
	})

	t.Run("excludes a soft-deleted twin by default, and includes it with soft_deletion populated when asked", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap1", mustParseTime(t, "2026-09-01T00:00:00Z"), false)
		destroyedAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		store.seedDestroyedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap1-20260901000000", "snap1", destroyedAt, false)

		// Two separate steps, not two data sources in one config: the fake SDK's list
		// pager keeps per-URL state, and two data sources issuing a list against the
		// identical URL in the same apply race that state.
		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_snapshots" "test" {
  volume_group_id = %[1]q
}
`, testVolumeGroupID("vg1")),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.#", "1"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.0.name", "snap1"),
						resource.TestCheckNoResourceAttr("data.everpure_azure_snapshots.test", "snapshots.0.soft_deletion"),
						expectSnapshotListOptions(store, to.Ptr(liveSnapshotsFilter)),
					),
				},
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_snapshots" "test" {
  volume_group_id   = %[1]q
  include_destroyed = true
}
`, testVolumeGroupID("vg1")),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.#", "2"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.0.name", "snap1"),
						resource.TestCheckNoResourceAttr("data.everpure_azure_snapshots.test", "snapshots.0.soft_deletion"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.1.name", "snap1-20260901000000"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.1.soft_deletion.previous_name", "snap1"),
						resource.TestCheckResourceAttr(
							"data.everpure_azure_snapshots.test", "snapshots.1.soft_deletion.destroyed_at",
							destroyedAt.Format(time.RFC3339),
						),
						expectSnapshotListOptions(store, nil),
					),
				},
			},
		})
	})

	t.Run("a malformed list element does not fail the read: no properties is kept, no id is dropped", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap-a", mustParseTime(t, "2026-09-01T00:00:00Z"), false)
		store.mu.Lock()
		store.snapshots[snapshotKey(testResourceGroup, testStoragePoolName, "vg1", "noprops")] = &armpurestorageblock.VolumeGroupSnapshot{
			ID:   to.Ptr(store.fakeSnapshotID(testResourceGroup, testStoragePoolName, "vg1", "noprops")),
			Name: to.Ptr("noprops"),
		}
		store.snapshots[snapshotKey(testResourceGroup, testStoragePoolName, "vg1", "noid")] = &armpurestorageblock.VolumeGroupSnapshot{
			Name:       to.Ptr("noid"),
			Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{},
		}
		store.mu.Unlock()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_snapshots" "test" {
  volume_group_id = %[1]q
}
`, testVolumeGroupID("vg1")),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.#", "2"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.0.name", "snap-a"),
						// No properties means no createdAt, which sorts after every dated snapshot.
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.1.name", "noprops"),
					),
				},
			},
		})
	})

	t.Run("chains to the singular data source by name for the member volumes", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap1", mustParseTime(t, "2026-09-01T00:00:00Z"), false)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_snapshots" "all" {
  volume_group_id = %[1]q
}

data "everpure_azure_snapshot" "first" {
  name            = data.everpure_azure_snapshots.all.snapshots[0].name
  volume_group_id = data.everpure_azure_snapshots.all.volume_group_id
}
`, testVolumeGroupID("vg1")),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_snapshot.first", "volume_snapshots.#", "1"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshot.first", "volume_snapshots.0.volume_name", "member-vol"),
					),
				},
			},
		})
	})

	// The documented way to pick the most recent scheduled snapshot: created_at is UTC
	// RFC 3339, so the strings sort chronologically.
	t.Run("the latest scheduled snapshot is picked by sorting created_at", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "17", mustParseTime(t, "2026-09-01T10:00:00Z"), true)
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "19", mustParseTime(t, "2026-09-01T12:00:00Z"), true)
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "18", mustParseTime(t, "2026-09-01T11:00:00Z"), true)
		// Newer than every scheduled one, but manual: it must not be picked.
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "manual", mustParseTime(t, "2026-09-02T00:00:00Z"), false)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_snapshots" "all" {
  volume_group_id = %[1]q
}

locals {
  scheduled   = { for s in data.everpure_azure_snapshots.all.snapshots : s.created_at => s.name if s.created_by_policy }
  latest_name = local.scheduled[reverse(sort(keys(local.scheduled)))[0]]
}

data "everpure_azure_snapshot" "latest" {
  name            = local.latest_name
  volume_group_id = data.everpure_azure_snapshots.all.volume_group_id
}
`, testVolumeGroupID("vg1")),
					Check: resource.TestCheckResourceAttr("data.everpure_azure_snapshot.latest", "name", "19"),
				},
			},
		})
	})

	t.Run("errors when volume_group_id names another subscription", func(t *testing.T) {
		factories, _ := newTestSnapshotProviderFactories()

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
data "everpure_azure_snapshots" "test" {
  volume_group_id = %[1]q
}
`, otherSubscriptionVGID),
					ExpectError: subscriptionMismatchRegexp(kindVolumeGroup, otherSubscriptionVGID),
				},
			},
		})
	})

	t.Run("limit returns only the newest snapshots, in one request", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		createdAt := mustParseTime(t, "2026-09-01T00:00:00Z")
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap-old", createdAt, false)
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap-new", createdAt.Add(time.Hour), false)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_snapshots" "test" {
  volume_group_id = %[1]q
  limit           = 1
}
`, testVolumeGroupID("vg1")),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.#", "1"),
						resource.TestCheckResourceAttr("data.everpure_azure_snapshots.test", "snapshots.0.name", "snap-new"),
						func(*terraform.State) error {
							store.mu.Lock()
							defer store.mu.Unlock()
							if opts := store.lastListOptions; opts == nil || opts.Top == nil || *opts.Top != 1 {
								return fmt.Errorf("last list options = %+v, want $top=1", opts)
							}
							return nil
						},
					),
				},
			},
		})
	})

	t.Run("a limit below 1 is rejected", func(t *testing.T) {
		factories, _ := newTestSnapshotProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_snapshots" "test" {
  volume_group_id = %[1]q
  limit           = 0
}
`, testVolumeGroupID("vg1")),
					ExpectError: regexp.MustCompile(`(?s)limit.*at least 1`),
				},
			},
		})
	})

	t.Run("errors when the volume group itself doesn't exist", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		store.markVolumeGroupMissing(testResourceGroup, testStoragePoolName, "vg1")

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_snapshots" "test" {
  volume_group_id = %[1]q
}
`, testVolumeGroupID("vg1")),
					ExpectError: diagRegexp(operation{action: actionList, kind: kindSnapshots, unnamed: true}.fail(
						newNotFoundError(kindVolumeGroup, testVolumeGroupID("vg1"), nil, apiFailure{}))),
				},
			},
		})
	})
}

// expectSnapshotListOptions checks the options of the store's most recent list call:
// the newest-first order and full-size pages always, and filter (nil for none).
func expectSnapshotListOptions(store *fakeSnapshotStore, filter *string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		store.mu.Lock()
		opts := store.lastListOptions
		store.mu.Unlock()
		if opts == nil || derefString(opts.Orderby) != snapshotsOrderBy ||
			derefString(opts.Filter) != derefString(filter) || opts.Top == nil || *opts.Top != maxSnapshotListPageSize {
			return fmt.Errorf("list options = %+v, want $orderby=%q, $filter=%v and $top=%d",
				opts, snapshotsOrderBy, derefString(filter), maxSnapshotListPageSize)
		}
		return nil
	}
}

// TestSnapshotsDataSourceLargeListWarning drives Read directly, since the plugin-testing
// harness doesn't surface a data source's warnings.
func TestSnapshotsDataSourceLargeListWarning(t *testing.T) {
	ctx := context.Background()
	createdAt := mustParseTime(t, "2026-09-01T00:00:00Z")

	read := func(t *testing.T, limit *int64) diag.Diagnostics {
		t.Helper()
		store := newFakeSnapshotStore(testSubscriptionID)
		for i := range largeSnapshotListWarningThreshold + 1 {
			store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", fmt.Sprintf("snap-%04d", i), createdAt, false)
		}
		srv := store.newVolumeGroupSnapshotsServer()
		opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: psbfake.NewVolumeGroupSnapshotsServerTransport(&srv)}}

		d := &SnapshotsDataSource{}
		var configureResp datasource.ConfigureResponse
		d.Configure(ctx, datasource.ConfigureRequest{ProviderData: &Clients{
			cred: &azcorefake.TokenCredential{}, opts: opts, subscriptionID: testSubscriptionID,
		}}, &configureResp)
		var schemaResp datasource.SchemaResponse
		d.Schema(ctx, datasource.SchemaRequest{}, &schemaResp)
		if configureResp.Diagnostics.HasError() || schemaResp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v %v", configureResp.Diagnostics, schemaResp.Diagnostics)
		}

		objectType, ok := schemaResp.Schema.Type().TerraformType(ctx).(tftypes.Object)
		if !ok {
			t.Fatal("the data source schema does not describe an object")
		}
		attributes := make(map[string]tftypes.Value, len(objectType.AttributeTypes))
		for name, attributeType := range objectType.AttributeTypes {
			attributes[name] = tftypes.NewValue(attributeType, nil)
		}
		attributes["volume_group_id"] = tftypes.NewValue(tftypes.String, testVolumeGroupID("vg1"))
		if limit != nil {
			attributes["limit"] = tftypes.NewValue(tftypes.Number, *limit)
		}
		raw := tftypes.NewValue(objectType, attributes)

		resp := &datasource.ReadResponse{State: tfsdk.State{Schema: schemaResp.Schema, Raw: raw}}
		d.Read(ctx, datasource.ReadRequest{Config: tfsdk.Config{Schema: schemaResp.Schema, Raw: raw}}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected error diagnostics: %v", resp.Diagnostics)
		}
		return resp.Diagnostics
	}

	t.Run("warns past the threshold without limit", func(t *testing.T) {
		diags := read(t, nil)
		want := diag.WithPath(path.Root("limit"), largeSnapshotListWarning(largeSnapshotListWarningThreshold+1, testVolumeGroupID("vg1")))
		if len(diags) != 1 || !diags[0].Equal(want) {
			t.Fatalf("got %v, want exactly %v", diags, want)
		}
	})

	t.Run("does not warn when limit is set, however large", func(t *testing.T) {
		limit := int64(largeSnapshotListWarningThreshold + 1)
		if diags := read(t, &limit); len(diags) != 0 {
			t.Fatalf("got %v, want no diagnostics", diags)
		}
	})
}
