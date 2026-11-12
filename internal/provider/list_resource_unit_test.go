// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/list"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

func TestListResultsStream(t *testing.T) {
	collect := func(limit int64, n int, failAt int) (names []string) {
		entries := make([]int, n)
		for i := range entries {
			entries[i] = i
		}
		stream := listResultsStream(limit, entries, func(i int) list.ListResult {
			if i == failAt {
				return errorListResult(diag.NewErrorDiagnostic("boom", "failed"))
			}
			return list.ListResult{DisplayName: strconv.Itoa(i)}
		})
		for result := range stream {
			if result.Diagnostics.HasError() {
				names = append(names, "error")
				continue
			}
			names = append(names, result.DisplayName)
		}
		return names
	}

	tests := []struct {
		name   string
		limit  int64
		n      int
		failAt int
		want   string
	}{
		{"a limit of zero means no limit", 0, 3, -1, "[0 1 2]"},
		{"a negative limit means no limit", -1, 3, -1, "[0 1 2]"},
		{"a limit below the count stops there", 2, 5, -1, "[0 1]"},
		{"a limit equal to the count returns everything", 3, 3, -1, "[0 1 2]"},
		{"a limit above the count returns everything", 10, 3, -1, "[0 1 2]"},
		{"no entries gives no results", 5, 0, -1, "[]"},
		{"an error result is pushed and ends the stream", 0, 5, 2, "[0 1 error]"},
		{"an entry beyond the limit is never built, so it cannot fail the query", 2, 5, 3, "[0 1]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := fmt.Sprint(collect(tc.limit, tc.n, tc.failAt))
			if got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}

	t.Run("stops when the consumer stops pulling", func(t *testing.T) {
		built := 0
		stream := listResultsStream(0, []int{0, 1, 2, 3}, func(i int) list.ListResult {
			built++
			return list.ListResult{DisplayName: strconv.Itoa(i)}
		})
		for range stream {
			break
		}
		if built != 1 {
			t.Errorf("built %d results, want 1", built)
		}
	})
}

func TestSortByAPIID(t *testing.T) {
	ids := func(entries []*string) []string {
		var out []string
		for _, e := range entries {
			out = append(out, derefString(e))
		}
		return out
	}
	entries := []*string{to.Ptr("/b/2"), nil, to.Ptr("/A/1"), to.Ptr("/a/2"), to.Ptr("/B/1")}
	sortByAPIID(entries, func(s *string) *string { return s })

	if got, want := fmt.Sprint(ids(entries)), "[ /A/1 /a/2 /B/1 /b/2]"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// blankModelForTest returns the typed-null model a list result starts from, built
// by blankResourceModel against the resource's real schema.
func blankModelForTest[M any](t *testing.T, r resource.Resource) M {
	t.Helper()
	ctx := context.Background()
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	blank, diags := blankResourceModel[M](ctx, list.ListRequest{ResourceSchema: schemaResp.Schema})
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	return blank
}

// The importedXFromAPI functions are what a list result's resource object comes from;
// these tests pin the two things that make it equal to a fresh import followed by Read:
// the defaults ImportState seeds, and the failure modes a malformed list entry hits.
func TestImportedStoragePoolFromAPI(t *testing.T) {
	ctx := context.Background()
	id := "/subscriptions/" + testSubscriptionID + "/resourceGroups/rg-a/providers/PureStorage.Block/storagePools/pool-a"
	blank := blankModelForTest[StoragePoolResourceModel](t, &StoragePoolResource{})

	t.Run("seeds the ID-derived attributes and maps the response", func(t *testing.T) {
		model, parsed, diags := importedStoragePoolFromAPI(ctx, blank, armpurestorageblock.StoragePool{
			ID:         to.Ptr(id),
			Name:       to.Ptr("pool-a"),
			Location:   to.Ptr("eastus2"),
			Properties: &armpurestorageblock.StoragePoolProperties{AvailabilityZone: to.Ptr("1")},
		})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if model.ID.ValueString() != id || model.Name.ValueString() != "pool-a" ||
			model.ResourceGroupName.ValueString() != "rg-a" || model.Zone.ValueString() != "1" {
			t.Errorf("unexpected model: %+v", model)
		}
		if parsed.ResourceGroupName != "rg-a" || parsed.StoragePoolName != "pool-a" {
			t.Errorf("unexpected parsed ID: %+v", parsed)
		}
		if !model.Tags.IsNull() {
			t.Errorf("tags = %v, want null when the API has none", model.Tags)
		}
	})

	for name, pool := range map[string]armpurestorageblock.StoragePool{
		"no id":         {Properties: &armpurestorageblock.StoragePoolProperties{}},
		"unparseable":   {ID: to.Ptr("/subscriptions/x"), Properties: &armpurestorageblock.StoragePoolProperties{}},
		"no properties": {ID: to.Ptr(id)},
	} {
		t.Run("an entry with "+name+" is an error", func(t *testing.T) {
			if _, _, diags := importedStoragePoolFromAPI(ctx, blank, pool); !diags.HasError() {
				t.Error("expected an error")
			}
		})
	}
}

func TestImportedReservationFromAPI(t *testing.T) {
	ctx := context.Background()
	id := "/subscriptions/" + testSubscriptionID + "/resourceGroups/rg-a/providers/PureStorage.Block/reservations/res-a"
	blank := blankModelForTest[ReservationResourceModel](t, &ReservationResource{})

	t.Run("seeds the ID-derived attributes, fills offer from the response and leaves user null", func(t *testing.T) {
		model, parsed, diags := importedReservationFromAPI(ctx, blank, armpurestorageblock.Reservation{
			ID:       to.Ptr(id),
			Name:     to.Ptr("res-a"),
			Location: to.Ptr("eastus2"),
			Properties: &armpurestorageblock.ReservationPropertiesBaseResourceProperties{
				User: &armpurestorageblock.UserDetails{FirstName: to.Ptr("Ada")},
				Marketplace: &armpurestorageblock.MarketplaceDetails{
					OfferDetails: &armpurestorageblock.OfferDetails{
						OfferID: to.Ptr("offer_a"), PlanID: to.Ptr("plan_a"), TermUnit: to.Ptr("P1M"),
					},
				},
			},
		})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if model.ID.ValueString() != id || model.Name.ValueString() != "res-a" ||
			model.ResourceGroupName.ValueString() != "rg-a" || model.Location.ValueString() != "eastus2" {
			t.Errorf("unexpected model: %+v", model)
		}
		if parsed.ResourceGroupName != "rg-a" || parsed.ReservationName != "res-a" {
			t.Errorf("unexpected parsed ID: %+v", parsed)
		}
		if !model.User.IsNull() {
			t.Errorf("user = %v, want null", model.User)
		}
		if model.Offer.IsNull() || model.Offer.IsUnknown() {
			t.Errorf("offer = %v, want the response's offer", model.Offer)
		}
		if !model.Tags.IsNull() {
			t.Errorf("tags = %v, want null when the API has none", model.Tags)
		}
	})

	for name, res := range map[string]armpurestorageblock.Reservation{
		"no id":         {Properties: &armpurestorageblock.ReservationPropertiesBaseResourceProperties{}},
		"unparseable":   {ID: to.Ptr("/subscriptions/x"), Properties: &armpurestorageblock.ReservationPropertiesBaseResourceProperties{}},
		"no properties": {ID: to.Ptr(id)},
	} {
		t.Run("an entry with "+name+" is an error", func(t *testing.T) {
			if _, _, diags := importedReservationFromAPI(ctx, blank, res); !diags.HasError() {
				t.Error("expected an error")
			}
		})
	}
}

func TestImportedVolumeGroupFromAPI(t *testing.T) {
	ctx := context.Background()
	id := testStoragePoolIDForVG() + "/volumeGroups/vg-a"
	blank := blankModelForTest[VolumeGroupResourceModel](t, &VolumeGroupResource{})

	t.Run("seeds ImportState's defaults and maps the response", func(t *testing.T) {
		model, parsed, diags := importedVolumeGroupFromAPI(ctx, blank, armpurestorageblock.VolumeGroup{
			ID:   to.Ptr(id),
			Name: to.Ptr("vg-a"),
			Properties: &armpurestorageblock.VolumeGroupProperties{
				VolumeGroupInternalID: to.Ptr("internal-vg-a"),
			},
		})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if model.ID.ValueString() != id || model.Name.ValueString() != "vg-a" ||
			model.StoragePoolID.ValueString() != testStoragePoolIDForVG() ||
			model.InternalID.ValueString() != "internal-vg-a" {
			t.Errorf("unexpected model: %+v", model)
		}
		if model.SourceType.ValueString() != volumeGroupSourceTypeNone || !model.DeletionProtection.ValueBool() ||
			model.EradicateOnDestroy.ValueBool() {
			t.Errorf("import defaults not seeded: source_type=%v deletion_protection=%v eradicate_on_destroy=%v",
				model.SourceType, model.DeletionProtection, model.EradicateOnDestroy)
		}
		if parsed.VolumeGroupName != "vg-a" {
			t.Errorf("unexpected parsed ID: %+v", parsed)
		}
	})

	for name, vg := range map[string]armpurestorageblock.VolumeGroup{
		"no id":         {Properties: &armpurestorageblock.VolumeGroupProperties{}},
		"unparseable":   {ID: to.Ptr(testStoragePoolIDForVG()), Properties: &armpurestorageblock.VolumeGroupProperties{}},
		"no properties": {ID: to.Ptr(id)},
	} {
		t.Run("an entry with "+name+" is an error", func(t *testing.T) {
			if _, _, diags := importedVolumeGroupFromAPI(ctx, blank, vg); !diags.HasError() {
				t.Error("expected an error")
			}
		})
	}
}

func TestImportedVolumeFromAPI(t *testing.T) {
	id := testVolumeGroupID("vg1") + "/volumes/vol-a"
	blank := blankModelForTest[VolumeResourceModel](t, &VolumeResource{})

	t.Run("seeds ImportState's defaults and maps the response", func(t *testing.T) {
		model, parsed, diags := importedVolumeFromAPI(blank, armpurestorageblock.Volume{
			ID:   to.Ptr(id),
			Name: to.Ptr("ignored-api-name"),
			Properties: &armpurestorageblock.AzureVolumeProperties{
				ProvisionedSize: to.Ptr(int64(bytesPerGiB)),
				SerialNumber:    to.Ptr("SERIAL"),
			},
		})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if model.ID.ValueString() != id || model.VolumeGroupID.ValueString() != testVolumeGroupID("vg1") ||
			model.ProvisionedSizeInBytes.ValueInt64() != bytesPerGiB || model.SerialNumber.ValueString() != "SERIAL" {
			t.Errorf("unexpected model: %+v", model)
		}
		// Read never maps the response's name (see applyVolume), so neither may the list.
		if model.Name.ValueString() != "vol-a" {
			t.Errorf("name = %q, want the one in the ID", model.Name.ValueString())
		}
		if model.SourceType.ValueString() != volumeSourceTypeNone || !model.DeletionProtection.ValueBool() ||
			model.EradicateOnDestroy.ValueBool() {
			t.Errorf("import defaults not seeded: source_type=%v deletion_protection=%v eradicate_on_destroy=%v",
				model.SourceType, model.DeletionProtection, model.EradicateOnDestroy)
		}
		if !model.ProvisionedSizeInGiB.IsNull() {
			t.Errorf("provisioned_size_in_gib = %v, want null as after an import", model.ProvisionedSizeInGiB)
		}
		if parsed.VolumeName != "vol-a" {
			t.Errorf("unexpected parsed ID: %+v", parsed)
		}
	})

	for name, v := range map[string]armpurestorageblock.Volume{
		"no id":         {Properties: &armpurestorageblock.AzureVolumeProperties{}},
		"unparseable":   {ID: to.Ptr(testVolumeGroupID("vg1")), Properties: &armpurestorageblock.AzureVolumeProperties{}},
		"no properties": {ID: to.Ptr(id)},
	} {
		t.Run("an entry with "+name+" is an error", func(t *testing.T) {
			if _, _, diags := importedVolumeFromAPI(blank, v); !diags.HasError() {
				t.Error("expected an error")
			}
		})
	}

	t.Run("the error names the problem", func(t *testing.T) {
		_, _, diags := importedVolumeFromAPI(blank, armpurestorageblock.Volume{ID: to.Ptr(id)})
		if len(diags) == 0 || !strings.Contains(diags[0].Detail(), "no properties") {
			t.Errorf("unexpected diagnostics: %v", diags)
		}
	})
}

func TestImportedSnapshotFromAPI(t *testing.T) {
	ctx := context.Background()
	id := testSnapshotVolumeGroupID + "/snapshots/snap-a"
	blank := blankModelForTest[SnapshotResourceModel](t, &SnapshotResource{})

	t.Run("seeds ImportState's defaults and maps the response", func(t *testing.T) {
		model, parsed, diags := importedSnapshotFromAPI(ctx, blank, armpurestorageblock.VolumeGroupSnapshot{
			ID:   to.Ptr(id),
			Name: to.Ptr("ignored-api-name"),
			Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{
				CreatedAt: to.Ptr(time.Date(2026, 9, 1, 2, 3, 4, 0, time.UTC)),
				VolumeSnapshots: []*armpurestorageblock.VolumeSnapshotInfo{
					{Name: to.Ptr("vol1"), ProvisionedSize: to.Ptr(int64(bytesPerGiB)), SerialNumber: to.Ptr("SERIAL")},
				},
			},
		})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if model.ID.ValueString() != id || model.Name.ValueString() != "snap-a" ||
			model.VolumeGroupID.ValueString() != testSnapshotVolumeGroupID ||
			model.CreatedAt.ValueString() != "2026-09-01T02:03:04Z" {
			t.Errorf("unexpected model: %+v", model)
		}
		if !model.DeletionProtection.ValueBool() || model.EradicateOnDestroy.ValueBool() {
			t.Errorf("import defaults not seeded: deletion_protection=%v eradicate_on_destroy=%v",
				model.DeletionProtection, model.EradicateOnDestroy)
		}
		if !model.SourceRecoverableSnapshotID.IsNull() {
			t.Errorf("source_recoverable_snapshot_id = %v, want null", model.SourceRecoverableSnapshotID)
		}
		if got := len(model.VolumeSnapshots.Elements()); got != 1 {
			t.Errorf("volume_snapshots has %d elements, want 1", got)
		}
		if parsed.SnapshotName != "snap-a" {
			t.Errorf("unexpected parsed ID: %+v", parsed)
		}
	})

	t.Run("a list element without members maps to an empty known list", func(t *testing.T) {
		model, _, diags := importedSnapshotFromAPI(ctx, blank, armpurestorageblock.VolumeGroupSnapshot{
			ID: to.Ptr(id), Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{},
		})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if model.VolumeSnapshots.IsNull() || model.VolumeSnapshots.IsUnknown() || len(model.VolumeSnapshots.Elements()) != 0 {
			t.Errorf("volume_snapshots = %v, want empty known list", model.VolumeSnapshots)
		}
	})

	for name, s := range map[string]armpurestorageblock.VolumeGroupSnapshot{
		"no id":         {Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{}},
		"unparseable":   {ID: to.Ptr(testSnapshotVolumeGroupID), Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{}},
		"no properties": {ID: to.Ptr(id)},
	} {
		t.Run("an entry with "+name+" is an error", func(t *testing.T) {
			if _, _, diags := importedSnapshotFromAPI(ctx, blank, s); !diags.HasError() {
				t.Error("expected an error")
			}
		})
	}
}
