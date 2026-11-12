// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr/xattr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestDurationValueStringSemanticEquals(t *testing.T) {
	ctx := context.Background()

	t.Run("PT60M is semantically equal to prior state PT1H", func(t *testing.T) {
		newValue := durationValue{StringValue: types.StringValue("PT60M")}
		priorValue := durationValue{StringValue: types.StringValue("PT1H")}

		equal, diags := newValue.StringSemanticEquals(ctx, priorValue)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if !equal {
			t.Fatal("expected PT60M and PT1H to be semantically equal")
		}
	})

	t.Run("different durations are not semantically equal", func(t *testing.T) {
		newValue := durationValue{StringValue: types.StringValue("PT2H")}
		priorValue := durationValue{StringValue: types.StringValue("PT1H")}

		equal, diags := newValue.StringSemanticEquals(ctx, priorValue)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if equal {
			t.Fatal("expected PT2H and PT1H not to be semantically equal")
		}
	})

	t.Run("null and unknown values are never treated as equal", func(t *testing.T) {
		known := durationValue{StringValue: types.StringValue("PT1H")}
		null := durationValue{StringValue: types.StringNull()}
		unknown := durationValue{StringValue: types.StringUnknown()}

		if equal, _ := known.StringSemanticEquals(ctx, null); equal {
			t.Fatal("expected a known value not to equal a null one")
		}
		if equal, _ := null.StringSemanticEquals(ctx, known); equal {
			t.Fatal("expected a null value not to equal a known one")
		}
		if equal, _ := known.StringSemanticEquals(ctx, unknown); equal {
			t.Fatal("expected a known value not to equal an unknown one")
		}
	})
}

func TestDurationValueValidateAttribute(t *testing.T) {
	ctx := context.Background()

	runValidate := func(value types.String) xattr.ValidateAttributeResponse {
		v := durationValue{StringValue: value}
		req := xattr.ValidateAttributeRequest{Path: path.Root("test")}
		var resp xattr.ValidateAttributeResponse
		v.ValidateAttribute(ctx, req, &resp)
		return resp
	}

	t.Run("accepts a valid days-or-smaller duration", func(t *testing.T) {
		resp := runValidate(types.StringValue("P7D"))
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
	})

	t.Run("rejects P1W with a days-or-smaller message", func(t *testing.T) {
		resp := runValidate(types.StringValue("P1W"))
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an error diagnostic")
		}
		requireContains(t, resp.Diagnostics[0].Detail(), "days or smaller")
	})

	t.Run("rejects P1DT, a dangling T with no components after it", func(t *testing.T) {
		resp := runValidate(types.StringValue("P1DT"))
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an error diagnostic")
		}
	})

	t.Run("rejects garbage", func(t *testing.T) {
		resp := runValidate(types.StringValue("garbage"))
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an error diagnostic")
		}
	})

	t.Run("skips null and unknown values", func(t *testing.T) {
		if resp := runValidate(types.StringNull()); resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics for null: %v", resp.Diagnostics)
		}
		if resp := runValidate(types.StringUnknown()); resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics for unknown: %v", resp.Diagnostics)
		}
	})
}

func TestPreserveEquivalentDurationSpellingPlanModifyString(t *testing.T) {
	ctx := context.Background()

	runModify := func(state, config, plan types.String) planmodifier.StringResponse {
		req := planmodifier.StringRequest{
			Path:        path.Root("test"),
			StateValue:  state,
			ConfigValue: config,
			PlanValue:   plan,
		}
		resp := planmodifier.StringResponse{PlanValue: plan}
		preserveEquivalentDurationSpelling().PlanModifyString(ctx, req, &resp)
		return resp
	}

	t.Run("an equivalent spelling in config keeps the state value", func(t *testing.T) {
		resp := runModify(types.StringValue("PT1H"), types.StringValue("PT60M"), types.StringValue("PT60M"))
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		if resp.PlanValue.ValueString() != "PT1H" {
			t.Fatalf("expected plan value to be reset to state's PT1H, got %q", resp.PlanValue.ValueString())
		}
	})

	t.Run("a genuinely different duration keeps the plan value", func(t *testing.T) {
		resp := runModify(types.StringValue("PT1H"), types.StringValue("PT2H"), types.StringValue("PT2H"))
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		if resp.PlanValue.ValueString() != "PT2H" {
			t.Fatalf("expected plan value to remain PT2H, got %q", resp.PlanValue.ValueString())
		}
	})

	t.Run("a null state value is a no-op, as on create", func(t *testing.T) {
		resp := runModify(types.StringNull(), types.StringValue("PT1H"), types.StringValue("PT1H"))
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		if resp.PlanValue.ValueString() != "PT1H" {
			t.Fatalf("expected plan value to remain PT1H, got %q", resp.PlanValue.ValueString())
		}
	})

	t.Run("an unknown plan value is a no-op", func(t *testing.T) {
		resp := runModify(types.StringValue("PT1H"), types.StringValue("PT1H"), types.StringUnknown())
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		if !resp.PlanValue.IsUnknown() {
			t.Fatal("expected plan value to remain unknown")
		}
	})

	t.Run("an unparseable value keeps the plan value and raises no diagnostic", func(t *testing.T) {
		resp := runModify(types.StringValue("PT1H"), types.StringValue("garbage"), types.StringValue("garbage"))
		if resp.Diagnostics.HasError() {
			t.Fatalf("expected no diagnostics, validation is ValidateAttribute's job: %v", resp.Diagnostics)
		}
		if resp.PlanValue.ValueString() != "garbage" {
			t.Fatalf("expected plan value to remain \"garbage\", got %q", resp.PlanValue.ValueString())
		}
	})
}
