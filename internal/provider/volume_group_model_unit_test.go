// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

func TestToPerformanceParameters(t *testing.T) {
	t.Run("both null returns nil", func(t *testing.T) {
		if got := toPerformanceParameters(types.Int64Null(), types.Int64Null()); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})

	t.Run("only bandwidth set omits iops entirely", func(t *testing.T) {
		got := toPerformanceParameters(types.Int64Value(500), types.Int64Null())
		if got == nil {
			t.Fatal("got nil, want a non-nil object")
		}
		if got.BandwidthLimitMbPerSec == nil || *got.BandwidthLimitMbPerSec != 500 {
			t.Fatalf("BandwidthLimitMbPerSec = %v, want 500", got.BandwidthLimitMbPerSec)
		}
		if got.IopsLimit != nil {
			t.Fatalf("IopsLimit = %v, want nil", got.IopsLimit)
		}
	})

	t.Run("only iops set omits bandwidth entirely", func(t *testing.T) {
		got := toPerformanceParameters(types.Int64Null(), types.Int64Value(200))
		if got == nil {
			t.Fatal("got nil, want a non-nil object")
		}
		if got.BandwidthLimitMbPerSec != nil {
			t.Fatalf("BandwidthLimitMbPerSec = %v, want nil", got.BandwidthLimitMbPerSec)
		}
		if got.IopsLimit == nil || *got.IopsLimit != 200 {
			t.Fatalf("IopsLimit = %v, want 200", got.IopsLimit)
		}
	})

	t.Run("both set", func(t *testing.T) {
		got := toPerformanceParameters(types.Int64Value(500), types.Int64Value(200))
		if got.BandwidthLimitMbPerSec == nil || *got.BandwidthLimitMbPerSec != 500 {
			t.Fatalf("BandwidthLimitMbPerSec = %v, want 500", got.BandwidthLimitMbPerSec)
		}
		if got.IopsLimit == nil || *got.IopsLimit != 200 {
			t.Fatalf("IopsLimit = %v, want 200", got.IopsLimit)
		}
	})
}

func TestToPerformanceParametersUpdate(t *testing.T) {
	t.Run("a null limit sends an explicit 0, the API's clear signal", func(t *testing.T) {
		got := toPerformanceParametersUpdate(types.Int64Null(), types.Int64Null())
		if got.BandwidthLimitMbPerSec == nil || *got.BandwidthLimitMbPerSec != 0 {
			t.Fatalf("BandwidthLimitMbPerSec = %v, want ptr to 0", got.BandwidthLimitMbPerSec)
		}
		if got.IopsLimit == nil || *got.IopsLimit != 0 {
			t.Fatalf("IopsLimit = %v, want ptr to 0", got.IopsLimit)
		}
	})

	t.Run("a set limit passes through unchanged", func(t *testing.T) {
		got := toPerformanceParametersUpdate(types.Int64Value(1000), types.Int64Value(500))
		if got.BandwidthLimitMbPerSec == nil || *got.BandwidthLimitMbPerSec != 1000 {
			t.Fatalf("BandwidthLimitMbPerSec = %v, want 1000", got.BandwidthLimitMbPerSec)
		}
		if got.IopsLimit == nil || *got.IopsLimit != 500 {
			t.Fatalf("IopsLimit = %v, want 500", got.IopsLimit)
		}
	})
}

func TestPerformanceParametersFromAPI(t *testing.T) {
	t.Run("nil object maps both limits to null", func(t *testing.T) {
		bw, iops := performanceParametersFromAPI(nil)
		if !bw.IsNull() || !iops.IsNull() {
			t.Fatalf("got bandwidth=%v iops=%v, want both null", bw, iops)
		}
	})

	t.Run("only one limit present maps the other to null", func(t *testing.T) {
		bw, iops := performanceParametersFromAPI(&armpurestorageblock.PerformanceParameters{
			BandwidthLimitMbPerSec: to.Ptr(int64(1000)),
		})
		if bw.ValueInt64() != 1000 {
			t.Fatalf("bandwidth = %v, want 1000", bw)
		}
		if !iops.IsNull() {
			t.Fatalf("iops = %v, want null", iops)
		}
	})

	t.Run("both limits present", func(t *testing.T) {
		bw, iops := performanceParametersFromAPI(&armpurestorageblock.PerformanceParameters{
			BandwidthLimitMbPerSec: to.Ptr(int64(1000)),
			IopsLimit:              to.Ptr(int64(500)),
		})
		if bw.ValueInt64() != 1000 || iops.ValueInt64() != 500 {
			t.Fatalf("got bandwidth=%v iops=%v, want 1000/500", bw, iops)
		}
	})
}

func TestToProtectionParametersCreate(t *testing.T) {
	t.Run("absent block returns nil", func(t *testing.T) {
		if got := toProtectionParametersCreate(nil); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})

	t.Run("present block carries both fields", func(t *testing.T) {
		policy := []SnapshotProtectionPolicyModel{{
			Frequency: durationValue{StringValue: types.StringValue("PT1H")},
			Retention: durationValue{StringValue: types.StringValue("P7D")},
		}}
		got := toProtectionParametersCreate(policy)
		if got == nil {
			t.Fatal("got nil, want a non-nil object")
		}
		if got.Frequency == nil || *got.Frequency != "PT1H" {
			t.Fatalf("Frequency = %v, want PT1H", got.Frequency)
		}
		if got.Retention == nil || *got.Retention != "P7D" {
			t.Fatalf("Retention = %v, want P7D", got.Retention)
		}
	})
}

func TestToProtectionParametersUpdate(t *testing.T) {
	t.Run("removing the block sends a non-nil, empty object, not nil", func(t *testing.T) {
		got := toProtectionParametersUpdate(nil)
		if got == nil {
			t.Fatal("got nil, want a non-nil, empty *ProtectionParameters so the PATCH body carries an explicit {}")
		}
		if got.Frequency != nil || got.Retention != nil {
			t.Fatalf("got %+v, want both fields nil", got)
		}
	})

	t.Run("present block carries both fields", func(t *testing.T) {
		policy := []SnapshotProtectionPolicyModel{{
			Frequency: durationValue{StringValue: types.StringValue("PT1H")},
			Retention: durationValue{StringValue: types.StringValue("P7D")},
		}}
		got := toProtectionParametersUpdate(policy)
		if got.Frequency == nil || *got.Frequency != "PT1H" {
			t.Fatalf("Frequency = %v, want PT1H", got.Frequency)
		}
		if got.Retention == nil || *got.Retention != "P7D" {
			t.Fatalf("Retention = %v, want P7D", got.Retention)
		}
	})
}

func TestProtectionParametersFromAPI(t *testing.T) {
	ctx := context.Background()

	t.Run("nil object maps to a null list", func(t *testing.T) {
		got, diags := protectionParametersFromAPI(ctx, nil)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if !got.IsNull() {
			t.Fatalf("got %v, want a null list", got)
		}
	})

	t.Run("only one field present maps to a null list", func(t *testing.T) {
		got, diags := protectionParametersFromAPI(ctx, &armpurestorageblock.ProtectionParameters{
			Frequency: to.Ptr("PT1H"),
		})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if !got.IsNull() {
			t.Fatalf("got %v, want a null list", got)
		}
	})

	t.Run("both fields present maps to a present block", func(t *testing.T) {
		list, diags := protectionParametersFromAPI(ctx, &armpurestorageblock.ProtectionParameters{
			Frequency: to.Ptr("PT1H"),
			Retention: to.Ptr("P7D"),
		})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		var got []SnapshotProtectionPolicyModel
		if d := list.ElementsAs(ctx, &got, false); d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}
		if len(got) != 1 {
			t.Fatalf("got %d blocks, want 1", len(got))
		}
		if got[0].Frequency.ValueString() != "PT1H" || got[0].Retention.ValueString() != "P7D" {
			t.Fatalf("got %+v", got[0])
		}
	})
}

func TestToVolumeGroup(t *testing.T) {
	ctx := context.Background()

	// The API's enum documents "none", but the service's create validation rejects
	// it when sent explicitly — a blank volume group must omit sourceType entirely,
	// whether source_type was defaulted or written as "none" in config.
	t.Run("source_type none omits sourceType and every source ID from the create body", func(t *testing.T) {
		for name, sourceType := range map[string]types.String{
			"explicit none": types.StringValue(volumeGroupSourceTypeNone),
			"null":          types.StringNull(),
		} {
			t.Run(name, func(t *testing.T) {
				data := VolumeGroupResourceModel{SourceType: sourceType}
				vg, diags := toVolumeGroup(ctx, data, "eastus2")
				if diags.HasError() {
					t.Fatalf("unexpected diagnostics: %v", diags)
				}
				if vg.Properties.SourceType != nil {
					t.Fatalf("SourceType = %v, want nil (omitted)", *vg.Properties.SourceType)
				}
				if vg.Properties.SourceVolumeGroupResourceID != nil ||
					vg.Properties.SourceSnapshotResourceID != nil ||
					vg.Properties.SourceRecoverableVolumeGroupResourceID != nil {
					t.Fatal("expected all source resource IDs to be nil for a blank volume group")
				}
			})
		}
	})

	t.Run("volume_group source sets sourceVolumeGroupResourceId and nothing else", func(t *testing.T) {
		data := VolumeGroupResourceModel{
			SourceType:          types.StringValue(volumeGroupSourceTypeVolumeGroup),
			SourceVolumeGroupID: armids.IDValue{StringValue: types.StringValue("/id/source-vg")},
		}
		vg, diags := toVolumeGroup(ctx, data, "eastus2")
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if *vg.Properties.SourceType != armpurestorageblock.VolumeGroupSourceTypeVolumeGroup {
			t.Fatalf("SourceType = %v", *vg.Properties.SourceType)
		}
		if vg.Properties.SourceVolumeGroupResourceID == nil || *vg.Properties.SourceVolumeGroupResourceID != "/id/source-vg" {
			t.Fatalf("SourceVolumeGroupResourceID = %v, want /id/source-vg", vg.Properties.SourceVolumeGroupResourceID)
		}
		if vg.Properties.SourceSnapshotResourceID != nil || vg.Properties.SourceRecoverableVolumeGroupResourceID != nil {
			t.Fatal("expected the other two source fields to stay nil")
		}
	})

	// The create is authorized against the snapshot's parent volume group, and is
	// rejected when its ID is missing from the body, so the snapshot row sends both.
	t.Run("snapshot source sets sourceSnapshotResourceId and the snapshot's parent volume group", func(t *testing.T) {
		const sourceSnapshotID = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/" +
			"providers/PureStorage.Block/storagePools/pool1/volumeGroups/source-vg/snapshots/snap1"
		const wantParentID = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/" +
			"providers/PureStorage.Block/storagePools/pool1/volumeGroups/source-vg"

		data := VolumeGroupResourceModel{
			SourceType:       types.StringValue(volumeGroupSourceTypeSnapshot),
			SourceSnapshotID: armids.IDValue{StringValue: types.StringValue(sourceSnapshotID)},
		}
		vg, diags := toVolumeGroup(ctx, data, "eastus2")
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if vg.Properties.SourceSnapshotResourceID == nil || *vg.Properties.SourceSnapshotResourceID != sourceSnapshotID {
			t.Fatalf("SourceSnapshotResourceID = %v, want %q", vg.Properties.SourceSnapshotResourceID, sourceSnapshotID)
		}
		if vg.Properties.SourceVolumeGroupResourceID == nil || *vg.Properties.SourceVolumeGroupResourceID != wantParentID {
			t.Fatalf("SourceVolumeGroupResourceID = %v, want %q", vg.Properties.SourceVolumeGroupResourceID, wantParentID)
		}
		if vg.Properties.SourceRecoverableVolumeGroupResourceID != nil {
			t.Fatal("expected sourceRecoverableVolumeGroupResourceId to stay nil")
		}
	})

	// The schema's regex validator keeps a malformed snapshot ID out of the plan, so
	// this only guards the mapping itself against silently sending a body with no
	// parent volume group, which the service would reject with an opaque 400.
	t.Run("snapshot source with an underivable ID raises a diagnostic", func(t *testing.T) {
		data := VolumeGroupResourceModel{
			SourceType:       types.StringValue(volumeGroupSourceTypeSnapshot),
			SourceSnapshotID: armids.IDValue{StringValue: types.StringValue("not-a-resource-id")},
		}
		_, diags := toVolumeGroup(ctx, data, "eastus2")
		if !diags.HasError() {
			t.Fatal("expected an error diagnostic, got none")
		}
	})

	t.Run("recoverable_volume_group source sets sourceRecoverableVolumeGroupResourceId", func(t *testing.T) {
		data := VolumeGroupResourceModel{
			SourceType:                     types.StringValue(volumeGroupSourceTypeRecoverableVolumeGroup),
			SourceRecoverableVolumeGroupID: armids.IDValue{StringValue: types.StringValue("/id/source-recoverable")},
		}
		vg, diags := toVolumeGroup(ctx, data, "eastus2")
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if vg.Properties.SourceRecoverableVolumeGroupResourceID == nil ||
			*vg.Properties.SourceRecoverableVolumeGroupResourceID != "/id/source-recoverable" {
			t.Fatalf("SourceRecoverableVolumeGroupResourceID = %v", vg.Properties.SourceRecoverableVolumeGroupResourceID)
		}
	})

	t.Run("omitted limits and protection stay nil", func(t *testing.T) {
		data := VolumeGroupResourceModel{SourceType: types.StringValue(volumeGroupSourceTypeNone)}
		vg, diags := toVolumeGroup(ctx, data, "eastus2")
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if vg.Properties.PerformanceParameters != nil {
			t.Fatalf("PerformanceParameters = %+v, want nil", vg.Properties.PerformanceParameters)
		}
		if vg.Properties.ProtectionParameters != nil {
			t.Fatalf("ProtectionParameters = %+v, want nil", vg.Properties.ProtectionParameters)
		}
	})

	t.Run("location is copied from the argument, not any attribute", func(t *testing.T) {
		data := VolumeGroupResourceModel{SourceType: types.StringValue(volumeGroupSourceTypeNone)}
		vg, diags := toVolumeGroup(ctx, data, "westus3")
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if vg.Location == nil || *vg.Location != "westus3" {
			t.Fatalf("Location = %v, want westus3", vg.Location)
		}
	})
}

func TestToVolumeGroupUpdate(t *testing.T) {
	ctx := context.Background()

	t.Run("the properties PATCH carries no tags", func(t *testing.T) {
		plan := VolumeGroupResourceModel{Tags: types.MapValueMust(types.StringType, map[string]attr.Value{"k": types.StringValue("v")})}
		update, diags := toVolumeGroupUpdate(ctx, plan)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if update.Tags != nil {
			t.Fatalf("Tags = %v, want nil", update.Tags)
		}
	})

	t.Run("removing the snapshot_protection_policy block resets it with an empty object", func(t *testing.T) {
		// A zero-value types.List is null, matching a plan where the block was
		// removed from config.
		plan := VolumeGroupResourceModel{Tags: types.MapNull(types.StringType)}
		update, diags := toVolumeGroupUpdate(ctx, plan)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if update.Properties.ProtectionParameters == nil {
			t.Fatal("expected a non-nil, empty ProtectionParameters")
		}
		if update.Properties.ProtectionParameters.Frequency != nil || update.Properties.ProtectionParameters.Retention != nil {
			t.Fatalf("got %+v, want both fields nil", update.Properties.ProtectionParameters)
		}
	})
}

func TestToVolumeGroupTagsUpdate(t *testing.T) {
	ctx := context.Background()

	t.Run("null tags send a non-nil empty map, clearing out-of-band tags", func(t *testing.T) {
		update, diags := toVolumeGroupTagsUpdate(ctx, VolumeGroupResourceModel{Tags: types.MapNull(types.StringType)})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if update.Tags == nil || len(update.Tags) != 0 {
			t.Fatalf("Tags = %v, want a non-nil empty map", update.Tags)
		}
		if update.Properties != nil {
			t.Fatalf("Properties = %+v, want nil", update.Properties)
		}
	})
}

func TestApplyVolumeGroup(t *testing.T) {
	ctx := context.Background()

	t.Run("returns an error instead of panicking when the API response has no properties", func(t *testing.T) {
		data := &VolumeGroupResourceModel{}
		diags := applyVolumeGroup(ctx, data, armpurestorageblock.VolumeGroup{Properties: nil})
		if !diags.HasError() {
			t.Fatal("expected an error diagnostic")
		}
	})

	t.Run("preserves source_type, source_*_id, storage_pool_id and the deletion attributes from prior state", func(t *testing.T) {
		data := &VolumeGroupResourceModel{
			StoragePoolID:                  armids.IDValue{StringValue: types.StringValue("/id/pool")},
			SourceType:                     types.StringValue(volumeGroupSourceTypeVolumeGroup),
			SourceVolumeGroupID:            armids.IDValue{StringValue: types.StringValue("/id/source-vg")},
			SourceSnapshotID:               armids.IDValue{StringValue: types.StringNull()},
			SourceRecoverableVolumeGroupID: armids.IDValue{StringValue: types.StringNull()},
			DeletionProtection:             types.BoolValue(true),
			EradicateOnDestroy:             types.BoolValue(true),
			Tags:                           types.MapNull(types.StringType),
		}

		vg := armpurestorageblock.VolumeGroup{
			ID:   to.Ptr("/id/vg"),
			Name: to.Ptr("vg1"),
			Properties: &armpurestorageblock.VolumeGroupProperties{
				VolumeGroupInternalID: to.Ptr("internal-vg1"),
			},
		}

		diags := applyVolumeGroup(ctx, data, vg)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}

		if data.StoragePoolID.ValueString() != "/id/pool" {
			t.Fatalf("StoragePoolID = %q, want unchanged", data.StoragePoolID.ValueString())
		}
		if data.SourceType.ValueString() != volumeGroupSourceTypeVolumeGroup {
			t.Fatalf("SourceType = %q, want unchanged", data.SourceType.ValueString())
		}
		if data.SourceVolumeGroupID.ValueString() != "/id/source-vg" {
			t.Fatalf("SourceVolumeGroupID = %q, want unchanged", data.SourceVolumeGroupID.ValueString())
		}
		if !data.DeletionProtection.ValueBool() || !data.EradicateOnDestroy.ValueBool() {
			t.Fatal("expected deletion attributes to stay unchanged")
		}
		if data.InternalID.ValueString() != "internal-vg1" {
			t.Fatalf("InternalID = %q, want internal-vg1", data.InternalID.ValueString())
		}
		if data.ID.ValueString() != "/id/vg" {
			t.Fatalf("ID = %q, want /id/vg", data.ID.ValueString())
		}
	})

	t.Run("never touches name, so a response spelled in another letter case cannot change it", func(t *testing.T) {
		data := &VolumeGroupResourceModel{
			Name: types.StringValue("vg1"),
			Tags: types.MapNull(types.StringType),
		}
		vg := armpurestorageblock.VolumeGroup{
			ID:         to.Ptr("/id/vg"),
			Name:       to.Ptr("VG1"),
			Properties: &armpurestorageblock.VolumeGroupProperties{},
		}

		if diags := applyVolumeGroup(ctx, data, vg); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if got := data.Name.ValueString(); got != "vg1" {
			t.Fatalf("got name %q, want %q", got, "vg1")
		}
	})

	t.Run("maps limits and protection from the response", func(t *testing.T) {
		data := &VolumeGroupResourceModel{Tags: types.MapNull(types.StringType)}
		vg := armpurestorageblock.VolumeGroup{
			Properties: &armpurestorageblock.VolumeGroupProperties{
				PerformanceParameters: &armpurestorageblock.PerformanceParameters{
					BandwidthLimitMbPerSec: to.Ptr(int64(1000)),
				},
				ProtectionParameters: &armpurestorageblock.ProtectionParameters{
					Frequency: to.Ptr("PT1H"),
					Retention: to.Ptr("P7D"),
				},
			},
		}
		diags := applyVolumeGroup(ctx, data, vg)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if data.BandwidthLimitInMbps.ValueInt64() != 1000 {
			t.Fatalf("BandwidthLimitInMbps = %v, want 1000", data.BandwidthLimitInMbps)
		}
		if !data.IopsLimit.IsNull() {
			t.Fatalf("IopsLimit = %v, want null", data.IopsLimit)
		}
		policy, d := blockElements[SnapshotProtectionPolicyModel](ctx, data.SnapshotProtectionPolicy)
		if d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}
		if len(policy) != 1 {
			t.Fatalf("got %d snapshot_protection_policy blocks, want 1", len(policy))
		}
	})

	t.Run("never touches tags, even when the response carries tags beyond the plan", func(t *testing.T) {
		planned, d := types.MapValueFrom(ctx, types.StringType, map[string]string{"environment": "test"})
		if d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}
		data := &VolumeGroupResourceModel{Tags: planned}

		// Simulates the RP layer stamping extra tags onto the create/update
		// response (env/org/owner, verified live) beyond what the plan sent.
		vg := armpurestorageblock.VolumeGroup{
			Properties: &armpurestorageblock.VolumeGroupProperties{},
			Tags: map[string]*string{
				"environment":   to.Ptr("test"),
				"stamped-by-rp": to.Ptr("true"),
			},
		}

		diags := applyVolumeGroup(ctx, data, vg)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}

		var got map[string]string
		if d := data.Tags.ElementsAs(ctx, &got, false); d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}
		want := map[string]string{"environment": "test"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v (the planned value, unchanged by the API response)", got, want)
		}
	})
}

func TestApplyVolumeGroupTags(t *testing.T) {
	ctx := context.Background()

	t.Run("maps the API's tags, including ones beyond the prior value — this is Read's job", func(t *testing.T) {
		prior, d := types.MapValueFrom(ctx, types.StringType, map[string]string{"environment": "test"})
		if d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}
		data := &VolumeGroupResourceModel{Tags: prior}

		apiTags := map[string]*string{
			"environment":   to.Ptr("test"),
			"stamped-by-rp": to.Ptr("true"),
		}

		diags := applyVolumeGroupTags(ctx, data, apiTags)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}

		var got map[string]string
		if d := data.Tags.ElementsAs(ctx, &got, false); d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}
		want := map[string]string{"environment": "test", "stamped-by-rp": "true"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
}

func TestNewRecoverableVolumeGroups(t *testing.T) {
	twin := func(name string) *armpurestorageblock.RecoverableVolumeGroup {
		return &armpurestorageblock.RecoverableVolumeGroup{Name: to.Ptr(name)}
	}

	t.Run("returns entries present after but not before", func(t *testing.T) {
		before := []*armpurestorageblock.RecoverableVolumeGroup{twin("vg1-20260101000000")}
		after := []*armpurestorageblock.RecoverableVolumeGroup{twin("vg1-20260101000000"), twin("vg1-20260917145432")}

		got := newRecoverableVolumeGroups(before, after)
		if len(got) != 1 {
			t.Fatalf("got %d new twins, want 1", len(got))
		}
		if got[0].Name == nil || *got[0].Name != "vg1-20260917145432" {
			t.Fatalf("got %+v, want the new twin", got[0])
		}
	})

	t.Run("no new twins returns an empty result", func(t *testing.T) {
		before := []*armpurestorageblock.RecoverableVolumeGroup{twin("vg1-20260101000000")}
		after := []*armpurestorageblock.RecoverableVolumeGroup{twin("vg1-20260101000000")}

		if got := newRecoverableVolumeGroups(before, after); len(got) != 0 {
			t.Fatalf("got %d new twins, want 0", len(got))
		}
	})

	t.Run("empty before returns every entry in after", func(t *testing.T) {
		after := []*armpurestorageblock.RecoverableVolumeGroup{twin("vg1-20260101000000")}
		if got := newRecoverableVolumeGroups(nil, after); len(got) != 1 {
			t.Fatalf("got %d new twins, want 1", len(got))
		}
	})

	t.Run("entries missing a name are skipped, not a panic", func(t *testing.T) {
		after := []*armpurestorageblock.RecoverableVolumeGroup{nil, {}, twin("vg1-20260101000000")}
		got := newRecoverableVolumeGroups(nil, after)
		if len(got) != 1 {
			t.Fatalf("got %d new twins, want 1", len(got))
		}
	})
}

func TestSelectEradicationTarget(t *testing.T) {
	twin := func(previousName, name string, destroyedAt time.Time) *armpurestorageblock.RecoverableVolumeGroup {
		return &armpurestorageblock.RecoverableVolumeGroup{
			Name: to.Ptr(name),
			Properties: &armpurestorageblock.RecoverableVolumeGroupProperties{
				SoftDeletion: &armpurestorageblock.DestroyedStateProperties{
					PreviousName: to.Ptr(previousName),
					DestroyedAt:  to.Ptr(destroyedAt),
				},
			},
		}
	}

	t.Run("empty list returns nil", func(t *testing.T) {
		if got := selectEradicationTarget(nil, "vg1"); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})

	t.Run("a new twin with a matching previousName is selected", func(t *testing.T) {
		newTwins := []*armpurestorageblock.RecoverableVolumeGroup{
			twin("vg1", "vg1-20260917145432", time.Now()),
		}
		got := selectEradicationTarget(newTwins, "vg1")
		if got == nil || got.Name == nil || *got.Name != "vg1-20260917145432" {
			t.Fatalf("got %+v, want the matching new twin", got)
		}
	})

	t.Run("decoys with the same previousName that pre-existed are ignored (already filtered by newRecoverableVolumeGroups)", func(t *testing.T) {
		// selectEradicationTarget only ever sees "new" twins, but it must still
		// only pick ones whose previousName actually matches — a new twin can
		// exist for a completely unrelated volume group deleted concurrently.
		newTwins := []*armpurestorageblock.RecoverableVolumeGroup{
			twin("other-vg", "other-vg-20260917145432", time.Now()),
		}
		if got := selectEradicationTarget(newTwins, "vg1"); got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})

	t.Run("several qualifying twins: newest destroyedAt wins", func(t *testing.T) {
		older := twin("vg1", "vg1-20260101000000", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		newer := twin("vg1", "vg1-20260601000000", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
		// A decoy for a different volume group, destroyed later than every
		// matching twin: selectEradicationTarget must filter by previousName
		// before comparing destroyedAt, not the other way around — computing the
		// global-max destroyedAt first would pick this decoy and eradicate an
		// unrelated volume group's twin instead of vg1's.
		decoy := twin("other-vg", "other-vg-20261231000000", time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC))

		got := selectEradicationTarget([]*armpurestorageblock.RecoverableVolumeGroup{older, newer, decoy}, "vg1")
		if got != newer {
			t.Fatalf("got the wrong twin: %+v", got)
		}
	})

	t.Run("a twin missing softDeletion or previousName is skipped, not a panic", func(t *testing.T) {
		newTwins := []*armpurestorageblock.RecoverableVolumeGroup{
			{},
			{Properties: &armpurestorageblock.RecoverableVolumeGroupProperties{}},
			twin("vg1", "vg1-20260917145432", time.Now()),
		}
		got := selectEradicationTarget(newTwins, "vg1")
		if got == nil {
			t.Fatal("expected the one real match")
		}
	})
}

func TestMissingSourceAttribute(t *testing.T) {
	withSourceVolumeGroupID := func(volumeGroupID types.String) VolumeGroupResourceModel {
		return VolumeGroupResourceModel{
			SourceVolumeGroupID:            armids.IDValue{StringValue: volumeGroupID},
			SourceSnapshotID:               armids.IDValue{StringValue: types.StringNull()},
			SourceRecoverableVolumeGroupID: armids.IDValue{StringValue: types.StringNull()},
		}
	}

	t.Run("flags a null source id as missing — the Create-time guard against an unknown that resolved to null", func(t *testing.T) {
		wantPath, missing := missingSourceAttribute(volumeGroupSourceTypeVolumeGroup, withSourceVolumeGroupID(types.StringNull()))
		if !missing {
			t.Fatal("expected missing = true")
		}
		if wantPath.String() != "source_volume_group_id" {
			t.Fatalf("wantPath = %v, want source_volume_group_id", wantPath)
		}
	})

	t.Run("a known, non-null source id is not missing", func(t *testing.T) {
		_, missing := missingSourceAttribute(volumeGroupSourceTypeVolumeGroup, withSourceVolumeGroupID(types.StringValue("/id/source-vg")))
		if missing {
			t.Fatal("expected missing = false")
		}
	})

	t.Run("source_type = none is never missing, regardless of the model", func(t *testing.T) {
		_, missing := missingSourceAttribute(volumeGroupSourceTypeNone, withSourceVolumeGroupID(types.StringNull()))
		if missing {
			t.Fatal("expected missing = false")
		}
	})
}

func TestToVolumeGroupWithNullSourceID(t *testing.T) {
	// Documents the bug missingSourceAttribute guards against: toVolumeGroup
	// itself has no way to tell "source_type names a source but the id is
	// null" from a legitimate empty string, so it would otherwise send the API
	// an empty-string resource id — an opaque backend 400 — instead of Create
	// catching it first with a clear attribute error.
	ctx := context.Background()
	data := VolumeGroupResourceModel{
		SourceType:          types.StringValue(volumeGroupSourceTypeVolumeGroup),
		SourceVolumeGroupID: armids.IDValue{StringValue: types.StringNull()},
	}

	vg, diags := toVolumeGroup(ctx, data, "eastus2")
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if vg.Properties.SourceVolumeGroupResourceID == nil || *vg.Properties.SourceVolumeGroupResourceID != "" {
		t.Fatalf("SourceVolumeGroupResourceID = %v, want a pointer to an empty string", vg.Properties.SourceVolumeGroupResourceID)
	}
}

func TestVolumeGroupItemFromName(t *testing.T) {
	vg := armpurestorageblock.VolumeGroup{
		ID:         to.Ptr("/id/vg"),
		Name:       to.Ptr("Vg1"),
		Properties: &armpurestorageblock.VolumeGroupProperties{},
	}

	item, diags := volumeGroupItemFrom(context.Background(), vg)
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if got := item.Name.ValueString(); got != "Vg1" {
		t.Fatalf("got name %q, want %q (the API's spelling)", got, "Vg1")
	}
}
