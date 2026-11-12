// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

// TestSnapshotModifyPlanUnconfiguredProvider drives SnapshotResource.ModifyPlan directly
// on a zero-value resource, whose r.clients is nil: Configure deferred because
// subscription_id is not known yet in this plan. The recovery source's GET must be
// skipped rather than hard-failing a plan a later Configure could still complete (Create
// runs the same check at apply), and nothing may panic. The check that needs no API call
// still runs.
func TestSnapshotModifyPlanUnconfiguredProvider(t *testing.T) {
	ctx := context.Background()
	r := &SnapshotResource{}
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)

	newPlan := func(data SnapshotResourceModel) tfsdk.Plan {
		plan := tfsdk.Plan{Schema: schemaResp.Schema}
		if diags := plan.Set(ctx, &data); diags.HasError() {
			t.Fatalf("unexpected diagnostics building plan: %v", diags)
		}
		return plan
	}
	modifyPlan := func(plan tfsdk.Plan, state tfsdk.State) *resource.ModifyPlanResponse {
		resp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: schemaResp.Schema}}
		r.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan, State: state}, resp)
		return resp
	}
	source := func(volumeGroupID string) armids.IDValue {
		return armids.IDValue{StringValue: types.StringValue(volumeGroupID + "/snapshots/snap1-20260101000000")}
	}

	t.Run("create with a recovery source: the GET is skipped, no diagnostics", func(t *testing.T) {
		plan := baseSnapshotResourceModel()
		plan.SourceRecoverableSnapshotID = source(testSnapshotVolumeGroupID)

		resp := modifyPlan(newPlan(plan), tfsdk.State{})
		if len(resp.Diagnostics) != 0 {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
	})

	t.Run("create with a recovery source in another volume group: still rejected, since that needs no API call", func(t *testing.T) {
		plan := baseSnapshotResourceModel()
		plan.SourceRecoverableSnapshotID = source(testSnapshotOtherVolumeGroupID)

		resp := modifyPlan(newPlan(plan), tfsdk.State{})
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an error")
		}
		sourcePath := path.Root("source_recoverable_snapshot_id")
		want := diag.WithPath(sourcePath, invalidAttributeCombinationDiagnostic(newRecoverySourceInOtherVolumeGroupError(sourcePath, kindSnapshot, testSnapshotOtherVolumeGroupID, plan.VolumeGroupID.ValueString())))
		if !resp.Diagnostics.Contains(want) {
			t.Fatalf("diagnostics = %v, want %v", resp.Diagnostics, want)
		}
	})

	t.Run("create with a recovery source unknown at plan time: no diagnostics", func(t *testing.T) {
		plan := baseSnapshotResourceModel()
		plan.SourceRecoverableSnapshotID = armids.IDValue{StringValue: types.StringUnknown()}

		resp := modifyPlan(newPlan(plan), tfsdk.State{})
		if len(resp.Diagnostics) != 0 {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
	})

	t.Run("create with no recovery source: no diagnostics", func(t *testing.T) {
		resp := modifyPlan(newPlan(baseSnapshotResourceModel()), tfsdk.State{})
		if len(resp.Diagnostics) != 0 {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
	})

	t.Run("destroy: blocked while the prior state has deletion_protection, allowed once it is off", func(t *testing.T) {
		newState := func(data SnapshotResourceModel) tfsdk.State {
			state := tfsdk.State{Schema: schemaResp.Schema}
			if diags := state.Set(ctx, &data); diags.HasError() {
				t.Fatalf("unexpected diagnostics building state: %v", diags)
			}
			return state
		}
		// A destroy plan is a null plan.
		nullPlan := func() tfsdk.Plan {
			return tfsdk.Plan{
				Schema: schemaResp.Schema,
				Raw:    tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), nil),
			}
		}

		protected := baseSnapshotResourceModel()
		resp := &resource.ModifyPlanResponse{}
		r.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: nullPlan(), State: newState(protected)}, resp)
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected a deletion-protected error")
		}
		requireContains(t, resp.Diagnostics[0].Summary(), deletionProtectedDiagnostic(kindSnapshot, detailError{}).Summary())

		unprotected := baseSnapshotResourceModel()
		unprotected.DeletionProtection = types.BoolValue(false)
		resp = &resource.ModifyPlanResponse{}
		r.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: nullPlan(), State: newState(unprotected)}, resp)
		if len(resp.Diagnostics) != 0 {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
	})
}
