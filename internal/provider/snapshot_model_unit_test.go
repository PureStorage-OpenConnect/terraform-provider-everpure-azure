// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

const (
	testSnapshotVolumeGroupID      = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg-test/providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg1"
	testSnapshotOtherVolumeGroupID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg-test/providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg2"
)

// baseSnapshotResourceModel returns a fully populated, schema-conformant
// SnapshotResourceModel — every field explicitly typed — for tests that build a real
// tfsdk.Plan or tfsdk.State via Set/Get.
func baseSnapshotResourceModel() SnapshotResourceModel {
	return SnapshotResourceModel{
		ID:                          armids.IDValue{StringValue: types.StringNull()},
		Name:                        types.StringValue("snap1"),
		VolumeGroupID:               armids.IDValue{StringValue: types.StringValue(testSnapshotVolumeGroupID)},
		SourceRecoverableSnapshotID: armids.IDValue{StringValue: types.StringNull()},
		CreatedAt:                   types.StringNull(),
		VolumeSnapshots:             types.ListNull(volumeSnapshotObjectType),
		DeletionProtection:          types.BoolValue(true),
		EradicateOnDestroy:          types.BoolValue(false),
		Timeouts: timeouts.Value{Object: types.ObjectNull(map[string]attr.Type{
			"create": types.StringType,
			"read":   types.StringType,
			"delete": types.StringType,
		})},
	}
}

func TestToSnapshot(t *testing.T) {
	t.Run("a plain create sends an empty body", func(t *testing.T) {
		got := toSnapshot(baseSnapshotResourceModel())
		if got.Properties != nil {
			t.Fatalf("got properties %+v, want none", got.Properties)
		}
	})

	t.Run("a recovery sends sourceSnapshotResourceId and nothing else", func(t *testing.T) {
		data := baseSnapshotResourceModel()
		sourceID := testSnapshotVolumeGroupID + "/snapshots/snap1-20260101000000"
		data.SourceRecoverableSnapshotID = armids.IDValue{StringValue: types.StringValue(sourceID)}

		got := toSnapshot(data)
		if got.Properties == nil || got.Properties.SourceSnapshotResourceID == nil || *got.Properties.SourceSnapshotResourceID != sourceID {
			t.Fatalf("got %+v, want sourceSnapshotResourceId %q", got.Properties, sourceID)
		}
		if props := got.Properties; props.CreatedAt != nil || props.CreatedByPolicy != nil || props.SoftDeletion != nil || props.VolumeSnapshots != nil {
			t.Fatalf("got properties %+v, want only sourceSnapshotResourceId", *props)
		}
	})
}

func TestVolumeSnapshotsFromAPI(t *testing.T) {
	ctx := context.Background()

	t.Run("maps each member, with a null serial number kept null", func(t *testing.T) {
		list, diags := volumeSnapshotsFromAPI(ctx, []*armpurestorageblock.VolumeSnapshotInfo{
			{Name: to.Ptr("vol-a"), ProvisionedSize: to.Ptr(int64(1073741824)), SerialNumber: to.Ptr("SERIAL1")},
			{Name: to.Ptr("vol-b"), ProvisionedSize: to.Ptr(int64(2147483648))},
		})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		var items []volumeSnapshotModel
		if d := list.ElementsAs(ctx, &items, false); d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}
		if len(items) != 2 {
			t.Fatalf("got %d items, want 2", len(items))
		}
		if items[0].VolumeName.ValueString() != "vol-a" || items[0].ProvisionedSizeInBytes.ValueInt64() != 1073741824 || items[0].SerialNumber.ValueString() != "SERIAL1" {
			t.Errorf("first item = %+v", items[0])
		}
		if items[1].VolumeName.ValueString() != "vol-b" || !items[1].SerialNumber.IsNull() {
			t.Errorf("second item = %+v, want a null serial number", items[1])
		}
	})

	t.Run("a nil slice is an empty, known list", func(t *testing.T) {
		list, diags := volumeSnapshotsFromAPI(ctx, nil)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if list.IsNull() || list.IsUnknown() || len(list.Elements()) != 0 {
			t.Fatalf("got %v, want an empty known list", list)
		}
		if !list.ElementType(ctx).Equal(volumeSnapshotObjectType) {
			t.Fatalf("element type = %v, want %v", list.ElementType(ctx), volumeSnapshotObjectType)
		}
	})

	t.Run("a nil entry is skipped", func(t *testing.T) {
		list, diags := volumeSnapshotsFromAPI(ctx, []*armpurestorageblock.VolumeSnapshotInfo{nil, {Name: to.Ptr("vol-a")}})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if len(list.Elements()) != 1 {
			t.Fatalf("got %d elements, want 1", len(list.Elements()))
		}
	})
}

func TestSnapshotCreatedAt(t *testing.T) {
	t.Run("nil maps to null", func(t *testing.T) {
		if got := snapshotCreatedAt(nil); !got.IsNull() {
			t.Fatalf("got %v, want null", got)
		}
	})

	t.Run("normalises to UTC", func(t *testing.T) {
		plusTwo := time.FixedZone("plus2", 2*60*60)
		got := snapshotCreatedAt(to.Ptr(time.Date(2026, 9, 1, 14, 30, 0, 0, plusTwo)))
		if got.ValueString() != "2026-09-01T12:30:00Z" {
			t.Fatalf("got %q, want 2026-09-01T12:30:00Z", got.ValueString())
		}
	})

	t.Run("string order is chronological across offsets", func(t *testing.T) {
		earlier := snapshotCreatedAt(to.Ptr(time.Date(2026, 9, 1, 13, 0, 0, 0, time.FixedZone("plus2", 2*60*60)))) // 11:00Z
		later := snapshotCreatedAt(to.Ptr(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)))
		if earlier.ValueString() >= later.ValueString() {
			t.Fatalf("%q should sort before %q", earlier.ValueString(), later.ValueString())
		}
	})
}

func TestApplySnapshot(t *testing.T) {
	ctx := context.Background()

	t.Run("fills id, created_at and volume_snapshots and leaves every other field alone", func(t *testing.T) {
		data := baseSnapshotResourceModel()
		data.SourceRecoverableSnapshotID = armids.IDValue{StringValue: types.StringValue(testSnapshotVolumeGroupID + "/snapshots/old")}
		data.DeletionProtection = types.BoolValue(false)
		data.EradicateOnDestroy = types.BoolValue(true)

		createdAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
		diags := applySnapshot(ctx, &data, armpurestorageblock.VolumeGroupSnapshot{
			ID:   to.Ptr(testSnapshotVolumeGroupID + "/snapshots/SNAP1"),
			Name: to.Ptr("SNAP1"),
			Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{
				CreatedAt:       to.Ptr(createdAt),
				VolumeSnapshots: []*armpurestorageblock.VolumeSnapshotInfo{{Name: to.Ptr("vol-a")}},
			},
		})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if data.ID.ValueString() != testSnapshotVolumeGroupID+"/snapshots/SNAP1" {
			t.Errorf("id = %q", data.ID.ValueString())
		}
		if data.CreatedAt.ValueString() != "2026-09-01T12:00:00Z" {
			t.Errorf("created_at = %q", data.CreatedAt.ValueString())
		}
		if len(data.VolumeSnapshots.Elements()) != 1 {
			t.Errorf("volume_snapshots has %d elements, want 1", len(data.VolumeSnapshots.Elements()))
		}
		if data.Name.ValueString() != "snap1" {
			t.Errorf("name = %q, want the configured value kept despite the API's casing", data.Name.ValueString())
		}
		if data.SourceRecoverableSnapshotID.IsNull() || data.DeletionProtection.ValueBool() || !data.EradicateOnDestroy.ValueBool() {
			t.Errorf("provider-side fields were changed: %+v", data)
		}
	})

	t.Run("nil Properties is an error, not a panic", func(t *testing.T) {
		data := baseSnapshotResourceModel()
		diags := applySnapshot(ctx, &data, armpurestorageblock.VolumeGroupSnapshot{})
		if !diags.HasError() {
			t.Fatal("expected an error")
		}
		requireContains(t, diags[0].Detail(), "no properties")
	})

	t.Run("nil volumeSnapshots gives an empty known list, nil createdAt a null created_at", func(t *testing.T) {
		data := baseSnapshotResourceModel()
		diags := applySnapshot(ctx, &data, armpurestorageblock.VolumeGroupSnapshot{
			Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{},
		})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if data.VolumeSnapshots.IsNull() || data.VolumeSnapshots.IsUnknown() || len(data.VolumeSnapshots.Elements()) != 0 {
			t.Errorf("volume_snapshots = %v, want an empty known list", data.VolumeSnapshots)
		}
		if !data.CreatedAt.IsNull() {
			t.Errorf("created_at = %v, want null", data.CreatedAt)
		}
		if !data.ID.IsNull() {
			t.Errorf("id = %v, want null", data.ID)
		}
	})
}

func TestCheckSnapshotRecoverySource(t *testing.T) {
	id := func(s string) armids.IDValue { return armids.IDValue{StringValue: types.StringValue(s)} }
	sourceIn := func(vg string) armids.IDValue { return id(vg + "/snapshots/snap1-20260101000000") }

	t.Run("same volume group: accepted", func(t *testing.T) {
		if diags := checkSnapshotRecoverySource(id(testSnapshotVolumeGroupID), sourceIn(testSnapshotVolumeGroupID)); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
	})

	t.Run("different volume group: rejected, naming both", func(t *testing.T) {
		diags := checkSnapshotRecoverySource(id(testSnapshotVolumeGroupID), sourceIn(testSnapshotOtherVolumeGroupID))
		if !diags.HasError() {
			t.Fatal("expected an error")
		}
		sourcePath := path.Root("source_recoverable_snapshot_id")
		want := diag.WithPath(sourcePath, invalidAttributeCombinationDiagnostic(newRecoverySourceInOtherVolumeGroupError(sourcePath, kindSnapshot, testSnapshotOtherVolumeGroupID, testSnapshotVolumeGroupID)))
		if !diags.Contains(want) {
			t.Fatalf("diagnostics = %v, want %v", diags, want)
		}
		requireContains(t, diags[0].Detail(), testSnapshotOtherVolumeGroupID)
		requireContains(t, diags[0].Detail(), testSnapshotVolumeGroupID)
	})

	t.Run("different casing of the same volume group: accepted", func(t *testing.T) {
		diags := checkSnapshotRecoverySource(id(testSnapshotVolumeGroupID), sourceIn(strings.ToUpper(testSnapshotVolumeGroupID)))
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
	})

	t.Run("malformed source ID: rejected", func(t *testing.T) {
		diags := checkSnapshotRecoverySource(id(testSnapshotVolumeGroupID), id("not-an-id"))
		if !diags.HasError() {
			t.Fatal("expected an error")
		}
		requireContains(t, diags[0].Summary(), invalidAttributeValueDiagnostic(detailError{}).Summary())
	})

	t.Run("unknown or null on either side: a no-op", func(t *testing.T) {
		unknown := armids.IDValue{StringValue: types.StringUnknown()}
		null := armids.IDValue{StringValue: types.StringNull()}
		for _, tt := range []struct{ vg, src armids.IDValue }{
			{unknown, sourceIn(testSnapshotOtherVolumeGroupID)},
			{id(testSnapshotVolumeGroupID), unknown},
			{id(testSnapshotVolumeGroupID), null},
		} {
			if diags := checkSnapshotRecoverySource(tt.vg, tt.src); len(diags) != 0 {
				t.Errorf("unexpected diagnostics: %v", diags)
			}
		}
	})
}

func TestIsSnapshotSoftDeletedAndCreatedByPolicy(t *testing.T) {
	destroyed := armpurestorageblock.VolumeGroupSnapshot{Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{
		SoftDeletion:    &armpurestorageblock.DestroyedStateProperties{Destroyed: to.Ptr(true)},
		CreatedByPolicy: to.Ptr(true),
	}}
	live := armpurestorageblock.VolumeGroupSnapshot{Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{
		SoftDeletion:    &armpurestorageblock.DestroyedStateProperties{Destroyed: to.Ptr(false)},
		CreatedByPolicy: to.Ptr(false),
	}}
	bare := armpurestorageblock.VolumeGroupSnapshot{}

	if !isSnapshotSoftDeleted(destroyed) || isSnapshotSoftDeleted(live) || isSnapshotSoftDeleted(bare) {
		t.Error("isSnapshotSoftDeleted gave the wrong answer")
	}
	if !snapshotCreatedByPolicy(destroyed) || snapshotCreatedByPolicy(live) || snapshotCreatedByPolicy(bare) {
		t.Error("snapshotCreatedByPolicy gave the wrong answer")
	}
}

func TestSnapshotItemFrom(t *testing.T) {
	t.Run("maps the shared attributes", func(t *testing.T) {
		item, diags := snapshotItemFrom(armpurestorageblock.VolumeGroupSnapshot{
			ID:   to.Ptr(testSnapshotVolumeGroupID + "/snapshots/17"),
			Name: to.Ptr("17"),
			Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{
				CreatedAt:       to.Ptr(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)),
				CreatedByPolicy: to.Ptr(true),
			},
		})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if item.Name.ValueString() != "17" || item.CreatedAt.ValueString() != "2026-09-01T12:00:00Z" || !item.CreatedByPolicy.ValueBool() {
			t.Fatalf("got %+v", item)
		}
	})

	t.Run("an absent createdByPolicy is false, not null", func(t *testing.T) {
		item, _ := snapshotItemFrom(armpurestorageblock.VolumeGroupSnapshot{Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{}})
		if item.CreatedByPolicy.IsNull() || item.CreatedByPolicy.ValueBool() {
			t.Fatalf("got %v, want false", item.CreatedByPolicy)
		}
	})

	t.Run("nil Properties is an error", func(t *testing.T) {
		if _, diags := snapshotItemFrom(armpurestorageblock.VolumeGroupSnapshot{}); !diags.HasError() {
			t.Fatal("expected an error")
		}
	})
}
