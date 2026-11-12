// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

// baseVolumeResourceModel returns a fully populated, schema-conformant
// VolumeResourceModel — every field explicitly typed, none left as a Go zero value —
// for tests that build a real tfsdk.Config, tfsdk.Plan or tfsdk.State via Set/Get. A
// zero-value types.String{} or types.List{} carries no element type and fails
// validation against the schema's real type with an opaque "MISSING TYPE" error, so
// every field must be constructed through its typed constructor even when the value is
// null. Individual tests override only the fields their scenario cares about.
func baseVolumeResourceModel() VolumeResourceModel {
	return VolumeResourceModel{
		ID:                        armids.IDValue{StringValue: types.StringNull()},
		Name:                      types.StringValue("vol1"),
		VolumeGroupID:             armids.IDValue{StringValue: types.StringValue(testVolumeGroupIDFixture)},
		ProvisionedSizeInBytes:    types.Int64Null(),
		ProvisionedSizeInGiB:      types.Int64Null(),
		SourceType:                types.StringValue(volumeSourceTypeNone),
		SourceVolumeID:            armids.IDValue{StringValue: types.StringNull()},
		SourceSerialNumber:        types.StringNull(),
		SourceRecoverableVolumeID: armids.IDValue{StringValue: types.StringNull()},
		SourceSnapshot:            types.ListNull(volumeSourceSnapshotObjectType),
		SerialNumber:              types.StringNull(),
		DeletionProtection:        types.BoolValue(true),
		EradicateOnDestroy:        types.BoolValue(false),
		Timeouts: timeouts.Value{Object: types.ObjectNull(map[string]attr.Type{
			"create": types.StringType,
			"read":   types.StringType,
			"update": types.StringType,
			"delete": types.StringType,
		})},
	}
}

// runVolumeInt64Validators builds the resource's real schema, sets data as a
// tfsdk.Config, and runs every validator attached to the named Int64 attribute against
// it — the same mechanism the framework itself uses, so a boundary or a ConflictsWith
// test exercises the schema as actually built rather than a hand-rolled stand-in for it.
func runVolumeInt64Validators(t *testing.T, ctx context.Context, data VolumeResourceModel, attrName string) diag.Diagnostics {
	t.Helper()

	r := &VolumeResource{}
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)

	// tfsdk.Config has no Set method of its own (practitioner config is normally
	// supplied by Terraform Core, never built by the provider) — Plan.Set builds the
	// identical underlying tftypes.Value from the same schema, so its Raw is reused
	// here to construct a Config with data in it.
	plan := tfsdk.Plan{Schema: schemaResp.Schema}
	if diags := plan.Set(ctx, &data); diags.HasError() {
		t.Fatalf("unexpected diagnostics building config: %v", diags)
	}
	cfg := tfsdk.Config{Schema: schemaResp.Schema, Raw: plan.Raw}

	attribute, ok := schemaResp.Schema.Attributes[attrName].(schema.Int64Attribute)
	if !ok {
		t.Fatalf("%s is not a schema.Int64Attribute", attrName)
	}

	var configValue types.Int64
	if diags := cfg.GetAttribute(ctx, path.Root(attrName), &configValue); diags.HasError() {
		t.Fatalf("unexpected diagnostics reading %s: %v", attrName, diags)
	}

	var diags diag.Diagnostics
	for _, v := range attribute.Validators {
		req := validator.Int64Request{
			Path:           path.Root(attrName),
			PathExpression: path.MatchRoot(attrName),
			Config:         cfg,
			ConfigValue:    configValue,
		}
		var resp validator.Int64Response
		v.ValidateInt64(ctx, req, &resp)
		diags.Append(resp.Diagnostics...)
	}
	return diags
}

func TestMultipleOf512Validator(t *testing.T) {
	ctx := context.Background()
	v := multipleOf512()

	tests := []struct {
		name    string
		value   types.Int64
		wantErr bool
	}{
		{"null is skipped, deferred to a later plan/apply", types.Int64Null(), false},
		{"unknown is skipped", types.Int64Unknown(), false},
		{"512 is a multiple", types.Int64Value(512), false},
		{"1048576 is a multiple", types.Int64Value(1048576), false},
		{"1048577 is not a multiple", types.Int64Value(1048577), true},
		{"0 is a multiple of 512 as far as this validator alone is concerned", types.Int64Value(0), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validator.Int64Request{Path: path.Root("provisioned_size_in_bytes"), ConfigValue: tt.value}
			var resp validator.Int64Response
			v.ValidateInt64(ctx, req, &resp)
			if resp.Diagnostics.HasError() != tt.wantErr {
				t.Fatalf("value=%v: HasError() = %v, want %v (%v)", tt.value, resp.Diagnostics.HasError(), tt.wantErr, resp.Diagnostics)
			}
		})
	}
}

// TestProvisionedSizeInBytesSchemaValidators covers the boundary values named in the
// resource plan: the 1 MiB floor, the 512-byte granularity, and that no maximum is
// enforced — plus the ConflictsWith pairing with provisioned_size_in_gib.
func TestProvisionedSizeInBytesSchemaValidators(t *testing.T) {
	ctx := context.Background()

	boundary := []struct {
		name    string
		value   int64
		wantErr bool
	}{
		{"0 is below the 1 MiB floor", 0, true},
		{"1 is below the 1 MiB floor", 1, true},
		{"1048575 is one byte below the 1 MiB floor", 1048575, true},
		{"1048576 (exactly 1 MiB) is accepted", 1048576, false},
		{"1048577 is not a multiple of 512", 1048577, true},
		{"a large value is accepted — no upper bound is imposed", 4398046511104, false}, // 4 TiB
	}
	for _, tt := range boundary {
		t.Run(tt.name, func(t *testing.T) {
			data := baseVolumeResourceModel()
			data.ProvisionedSizeInBytes = types.Int64Value(tt.value)
			diags := runVolumeInt64Validators(t, ctx, data, "provisioned_size_in_bytes")
			if diags.HasError() != tt.wantErr {
				t.Fatalf("provisioned_size_in_bytes = %d: HasError() = %v, want %v (%v)", tt.value, diags.HasError(), tt.wantErr, diags)
			}
		})
	}

	t.Run("conflicts with provisioned_size_in_gib being set at the same time", func(t *testing.T) {
		data := baseVolumeResourceModel()
		data.ProvisionedSizeInBytes = types.Int64Value(1048576)
		data.ProvisionedSizeInGiB = types.Int64Value(1)
		if diags := runVolumeInt64Validators(t, ctx, data, "provisioned_size_in_bytes"); !diags.HasError() {
			t.Fatal("expected a conflict error when both size attributes are set")
		}
	})
}

// TestProvisionedSizeInGiBSchemaValidators covers the gib attribute's own bound (>= 1,
// since every whole GiB already clears the byte attribute's floor and granularity) and
// its ConflictsWith pairing with provisioned_size_in_bytes.
func TestProvisionedSizeInGiBSchemaValidators(t *testing.T) {
	ctx := context.Background()

	boundary := []struct {
		name    string
		value   int64
		wantErr bool
	}{
		{"0 is rejected", 0, true},
		{"a negative value is rejected", -1, true},
		{"1 is accepted", 1, false},
		{"a large value is accepted — no upper bound is imposed", 1 << 20, false},
	}
	for _, tt := range boundary {
		t.Run(tt.name, func(t *testing.T) {
			data := baseVolumeResourceModel()
			data.ProvisionedSizeInGiB = types.Int64Value(tt.value)
			diags := runVolumeInt64Validators(t, ctx, data, "provisioned_size_in_gib")
			if diags.HasError() != tt.wantErr {
				t.Fatalf("provisioned_size_in_gib = %d: HasError() = %v, want %v (%v)", tt.value, diags.HasError(), tt.wantErr, diags)
			}
		})
	}

	t.Run("conflicts with provisioned_size_in_bytes being set at the same time", func(t *testing.T) {
		data := baseVolumeResourceModel()
		data.ProvisionedSizeInGiB = types.Int64Value(1)
		data.ProvisionedSizeInBytes = types.Int64Value(1048576)
		if diags := runVolumeInt64Validators(t, ctx, data, "provisioned_size_in_gib"); !diags.HasError() {
			t.Fatal("expected a conflict error when both size attributes are set")
		}
	})
}

// TestVolumeModifyPlanSizePresenceCreateOnly drives VolumeResource.ModifyPlan directly
// (no client, no terraform CLI — pure Go structs) to pin the part of the presence rule
// that checkVolumeSizePresence alone cannot demonstrate: it is only ever consulted on a
// create. The same configuration that ModifyPlan rejects when req.State is null (a
// create) must be accepted unchanged when req.State carries prior data (an update).
func TestVolumeModifyPlanSizePresenceCreateOnly(t *testing.T) {
	ctx := context.Background()
	r := &VolumeResource{}
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)

	newPlan := func(data VolumeResourceModel) tfsdk.Plan {
		plan := tfsdk.Plan{Schema: schemaResp.Schema}
		if diags := plan.Set(ctx, &data); diags.HasError() {
			t.Fatalf("unexpected diagnostics building plan: %v", diags)
		}
		return plan
	}
	newState := func(data VolumeResourceModel) tfsdk.State {
		state := tfsdk.State{Schema: schemaResp.Schema}
		if diags := state.Set(ctx, &data); diags.HasError() {
			t.Fatalf("unexpected diagnostics building state: %v", diags)
		}
		return state
	}
	modifyPlan := func(plan tfsdk.Plan, state tfsdk.State) *resource.ModifyPlanResponse {
		resp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: schemaResp.Schema}}
		r.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan, State: state}, resp)
		return resp
	}

	t.Run("create, source_type none, neither size set: rejected", func(t *testing.T) {
		plan := baseVolumeResourceModel()
		resp := modifyPlan(newPlan(plan), tfsdk.State{})
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected a missing-size error on create")
		}
		requireContains(t, resp.Diagnostics[0].Detail(), "is required when")
	})

	t.Run("create, a non-none source_type with a size set: rejected", func(t *testing.T) {
		plan := baseVolumeResourceModel()
		plan.SourceType = types.StringValue(volumeSourceTypeVolume)
		plan.SourceVolumeID = armids.IDValue{StringValue: types.StringValue(testVolumeIDSamePool)}
		plan.ProvisionedSizeInGiB = types.Int64Value(10)
		resp := modifyPlan(newPlan(plan), tfsdk.State{})
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an unexpected-size error on create")
		}
		requireContains(t, resp.Diagnostics[0].Detail(), "must not be set when source_type")
	})

	t.Run("create, a non-none source_type with neither size set: accepted", func(t *testing.T) {
		plan := baseVolumeResourceModel()
		plan.SourceType = types.StringValue(volumeSourceTypeVolume)
		plan.SourceVolumeID = armids.IDValue{StringValue: types.StringValue(testVolumeIDSamePool)}
		resp := modifyPlan(newPlan(plan), tfsdk.State{})
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
	})

	t.Run("update: the exact configuration rejected on create above (a non-none source_type with a size set) is accepted, because presence is create-only", func(t *testing.T) {
		priorState := baseVolumeResourceModel()
		priorState.SourceType = types.StringValue(volumeSourceTypeVolume)
		priorState.SourceVolumeID = armids.IDValue{StringValue: types.StringValue(testVolumeIDSamePool)}
		priorState.ProvisionedSizeInBytes = types.Int64Value(5 * bytesPerGiB)

		plan := priorState
		plan.ProvisionedSizeInGiB = types.Int64Value(10) // a grow, so the decrease guard doesn't fire either

		resp := modifyPlan(newPlan(plan), newState(priorState))
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics on update: %v", resp.Diagnostics)
		}
	})
}
