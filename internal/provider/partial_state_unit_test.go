// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

// TestFailedCreateLeavesKnownState checks that a create whose poll fails returns its
// error with a state that holds no unknown value: the plan's computed attributes are
// unknown, and Terraform rejects any unknown left in the state an apply returns.
func TestFailedCreateLeavesKnownState(t *testing.T) {
	unknownID := armids.IDValue{StringValue: types.StringUnknown()}

	t.Run("storage pool, with unknowns nested in platform_console", func(t *testing.T) {
		r, store := newTestStoragePoolDirectResource(t)
		store.failCreatePoll = true

		data := testStoragePoolStateModel(t)
		data.ID = unknownID
		data.InternalID = types.StringUnknown()
		data.ProvisionedIops = types.Int64Unknown()
		data.DataRetentionPeriodInSeconds = types.Int64Unknown()
		data.PlatformConsole = types.ListValueMust(platformConsoleObjectType, []attr.Value{
			types.ObjectValueMust(platformConsoleObjectType.AttrTypes, map[string]attr.Value{
				"gui_enabled":      types.BoolValue(true),
				"api_enabled":      types.BoolValue(true),
				"cli_enabled":      types.BoolValue(true),
				"subnet_ids":       types.ListValueMust(types.StringType, []attr.Value{types.StringValue(testSubnetID("snet1"))}),
				"default_username": types.StringUnknown(),
				"subnet":           types.ListUnknown(platformConsoleSubnetObjectType),
			}),
		})

		requireFailedCreateLeavesKnownState(t, r, newTestStoragePoolState(t, r, data))
	})

	t.Run("volume group", func(t *testing.T) {
		r, store := newTestVolumeGroupDeleteResource(t)
		store.failCreatePoll = true

		plan := newTestVolumeGroupDeleteState(t, r, func(data *VolumeGroupResourceModel) {
			data.ID = unknownID
			data.InternalID = types.StringUnknown()
		})
		requireFailedCreateLeavesKnownState(t, r, plan)
	})

	t.Run("volume", func(t *testing.T) {
		r, store := newTestVolumeDeleteResource(t)
		store.failCreatePoll = true

		plan := newTestVolumeDeleteState(t, r, func(data *VolumeResourceModel) {
			data.ID = unknownID
			data.ProvisionedSizeInBytes = types.Int64Value(1 << 30)
			data.SerialNumber = types.StringUnknown()
		})
		requireFailedCreateLeavesKnownState(t, r, plan)
	})

	t.Run("snapshot", func(t *testing.T) {
		r, store := newTestSnapshotDirectResource(t)
		store.failCreatePoll = true

		plan := newTestSnapshotDeleteState(t, r, func(data *SnapshotResourceModel) {
			data.ID = unknownID
			data.CreatedAt = types.StringUnknown()
			data.VolumeSnapshots = types.ListUnknown(volumeSnapshotObjectType)
		})
		requireFailedCreateLeavesKnownState(t, r, plan)
	})
}

// requireFailedCreateLeavesKnownState runs Create on plan, whose create poll the caller
// has made fail, and requires an error, a saved state, and no unknown value in it.
func requireFailedCreateLeavesKnownState(t *testing.T, r fwresource.Resource, plan tfsdk.State) {
	t.Helper()
	ctx := context.Background()

	if plan.Raw.IsFullyKnown() {
		t.Fatal("the plan has no unknown value, so the test proves nothing")
	}

	resp := &fwresource.CreateResponse{State: tfsdk.State{
		Schema: plan.Schema,
		Raw:    tftypes.NewValue(plan.Schema.Type().TerraformType(ctx), nil),
	}}
	r.Create(ctx, fwresource.CreateRequest{Plan: tfsdk.Plan(plan)}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected the failed poll's error")
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("Create saved no state, so a failed poll would leave an orphan")
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("the state Create returned holds unknown values: %s", resp.State.Raw)
	}
}

// TestFailedCreateStateDoesNotBlockItsReplacement checks that the state a failed create
// leaves carries deletion_protection = false even when the plan said true, so Delete, which
// Terraform runs to replace the tainted resource, is not refused with advice that cannot
// be followed on a tainted resource.
func TestFailedCreateStateDoesNotBlockItsReplacement(t *testing.T) {
	unknownID := armids.IDValue{StringValue: types.StringUnknown()}

	tests := []struct {
		name string
		kind kind
		plan func(t *testing.T) (fwresource.Resource, tfsdk.State)
	}{
		{
			name: "volume group",
			kind: kindVolumeGroup,
			plan: func(t *testing.T) (fwresource.Resource, tfsdk.State) {
				r, store := newTestVolumeGroupDeleteResource(t)
				store.failCreatePoll = true
				return r, newTestVolumeGroupDeleteState(t, r, func(data *VolumeGroupResourceModel) {
					data.ID = unknownID
					data.InternalID = types.StringUnknown()
					data.DeletionProtection = types.BoolValue(true)
					data.EradicateOnDestroy = types.BoolValue(false)
				})
			},
		},
		{
			name: "volume",
			kind: kindVolume,
			plan: func(t *testing.T) (fwresource.Resource, tfsdk.State) {
				r, store := newTestVolumeDeleteResource(t)
				store.failCreatePoll = true
				return r, newTestVolumeDeleteState(t, r, func(data *VolumeResourceModel) {
					data.ID = unknownID
					data.ProvisionedSizeInBytes = types.Int64Value(1 << 30)
					data.SerialNumber = types.StringUnknown()
					data.DeletionProtection = types.BoolValue(true)
					data.EradicateOnDestroy = types.BoolValue(false)
				})
			},
		},
		{
			name: "snapshot",
			kind: kindSnapshot,
			plan: func(t *testing.T) (fwresource.Resource, tfsdk.State) {
				r, store := newTestSnapshotDirectResource(t)
				store.failCreatePoll = true
				return r, newTestSnapshotDeleteState(t, r, func(data *SnapshotResourceModel) {
					data.ID = unknownID
					data.CreatedAt = types.StringUnknown()
					data.VolumeSnapshots = types.ListUnknown(volumeSnapshotObjectType)
					data.DeletionProtection = types.BoolValue(true)
					data.EradicateOnDestroy = types.BoolValue(false)
				})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			r, plan := tt.plan(t)

			var planned types.Bool
			if diags := plan.GetAttribute(ctx, path.Root("deletion_protection"), &planned); diags.HasError() {
				t.Fatalf("unexpected diagnostics: %v", diags)
			}
			if !planned.ValueBool() {
				t.Fatal("the plan has deletion_protection = false, so the test proves nothing")
			}

			createResp := &fwresource.CreateResponse{State: tfsdk.State{
				Schema: plan.Schema,
				Raw:    tftypes.NewValue(plan.Schema.Type().TerraformType(ctx), nil),
			}}
			r.Create(ctx, fwresource.CreateRequest{Plan: tfsdk.Plan(plan)}, createResp)
			if !createResp.Diagnostics.HasError() {
				t.Fatal("expected the failed poll's error")
			}

			var got types.Bool
			if diags := createResp.State.GetAttribute(ctx, path.Root("deletion_protection"), &got); diags.HasError() {
				t.Fatalf("unexpected diagnostics: %v", diags)
			}
			if got.IsNull() || got.IsUnknown() || got.ValueBool() {
				t.Fatalf("got deletion_protection = %s in the state a failed create left, want false", got)
			}

			deleteResp := &fwresource.DeleteResponse{}
			r.Delete(ctx, fwresource.DeleteRequest{State: createResp.State}, deleteResp)
			for _, d := range deleteResp.Diagnostics {
				if deletionProtectedRegexp(tt.kind).MatchString(d.Summary() + "\n" + d.Detail()) {
					t.Fatalf("got a deletion-protected error from Delete, want none: %v", d)
				}
			}
		})
	}
}
