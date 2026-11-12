// SPDX-License-Identifier: Apache-2.0

package armids

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestIDValueStringSemanticEquals(t *testing.T) {
	ctx := context.Background()

	t.Run("differently-cased spellings of the same ID are semantically equal", func(t *testing.T) {
		newValue := IDValue{StringValue: types.StringValue(
			"/Subscriptions/sub1/ResourceGroups/RG1/providers/purestorage.block/StoragePools/pool1")}
		priorValue := IDValue{StringValue: types.StringValue(
			"/subscriptions/sub1/resourceGroups/RG1/providers/PureStorage.Block/storagePools/pool1")}

		equal, diags := newValue.StringSemanticEquals(ctx, priorValue)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if !equal {
			t.Fatal("expected the two differently-cased IDs to be semantically equal")
		}
	})

	t.Run("IDs naming different resources are not semantically equal", func(t *testing.T) {
		newValue := IDValue{StringValue: types.StringValue(
			"/subscriptions/sub1/resourceGroups/RG1/providers/PureStorage.Block/storagePools/pool1")}
		priorValue := IDValue{StringValue: types.StringValue(
			"/subscriptions/sub1/resourceGroups/RG1/providers/PureStorage.Block/storagePools/pool2")}

		equal, diags := newValue.StringSemanticEquals(ctx, priorValue)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if equal {
			t.Fatal("expected the two IDs naming different pools not to be semantically equal")
		}
	})

	t.Run("null and unknown values are never treated as equal", func(t *testing.T) {
		known := IDValue{StringValue: types.StringValue(
			"/subscriptions/sub1/resourceGroups/RG1/providers/PureStorage.Block/storagePools/pool1")}
		null := IDValue{StringValue: types.StringNull()}
		unknown := IDValue{StringValue: types.StringUnknown()}

		if equal, _ := known.StringSemanticEquals(ctx, null); equal {
			t.Fatal("expected a known value not to equal a null one")
		}
		if equal, _ := null.StringSemanticEquals(ctx, known); equal {
			t.Fatal("expected a null value not to equal a known one")
		}
		if equal, _ := known.StringSemanticEquals(ctx, unknown); equal {
			t.Fatal("expected a known value not to equal an unknown one")
		}
		if equal, _ := unknown.StringSemanticEquals(ctx, known); equal {
			t.Fatal("expected an unknown value not to equal a known one")
		}
	})
}

func TestPreserveEquivalentIDSpellingPlanModifyString(t *testing.T) {
	ctx := context.Background()

	canonical := "/subscriptions/sub1/resourceGroups/RG1/providers/PureStorage.Block/storagePools/pool1"
	mangled := "/Subscriptions/sub1/ResourceGroups/RG1/providers/purestorage.block/StoragePools/pool1"
	other := "/subscriptions/sub1/resourceGroups/RG1/providers/PureStorage.Block/storagePools/pool2"

	runModify := func(state, config, plan types.String) planmodifier.StringResponse {
		req := planmodifier.StringRequest{
			Path:        path.Root("test"),
			StateValue:  state,
			ConfigValue: config,
			PlanValue:   plan,
		}
		resp := planmodifier.StringResponse{PlanValue: plan}
		PreserveEquivalentIDSpelling().PlanModifyString(ctx, req, &resp)
		return resp
	}

	t.Run("an equivalent spelling in config keeps the state value", func(t *testing.T) {
		resp := runModify(types.StringValue(canonical), types.StringValue(mangled), types.StringValue(mangled))
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		if resp.PlanValue.ValueString() != canonical {
			t.Fatalf("expected plan value to be reset to the canonical spelling, got %q", resp.PlanValue.ValueString())
		}
	})

	t.Run("an ID naming a different resource keeps the plan value", func(t *testing.T) {
		resp := runModify(types.StringValue(canonical), types.StringValue(other), types.StringValue(other))
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		if resp.PlanValue.ValueString() != other {
			t.Fatalf("expected plan value to remain %q, got %q", other, resp.PlanValue.ValueString())
		}
	})

	t.Run("a null state value is a no-op, as on create", func(t *testing.T) {
		resp := runModify(types.StringNull(), types.StringValue(mangled), types.StringValue(mangled))
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		if resp.PlanValue.ValueString() != mangled {
			t.Fatalf("expected plan value to remain %q, got %q", mangled, resp.PlanValue.ValueString())
		}
	})

	t.Run("an unknown plan value is a no-op", func(t *testing.T) {
		resp := runModify(types.StringValue(canonical), types.StringValue(mangled), types.StringUnknown())
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		if !resp.PlanValue.IsUnknown() {
			t.Fatal("expected plan value to remain unknown")
		}
	})

	t.Run("a null config value is a no-op", func(t *testing.T) {
		resp := runModify(types.StringValue(canonical), types.StringNull(), types.StringNull())
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		if !resp.PlanValue.IsNull() {
			t.Fatal("expected plan value to remain null")
		}
	})
}

func TestPreserveEquivalentIDSpellingInSetPlanModifySet(t *testing.T) {
	ctx := context.Background()

	canonical := "/subscriptions/sub1/resourceGroups/RG1/providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg1"
	mangled := "/Subscriptions/sub1/ResourceGroups/RG1/providers/purestorage.block/StoragePools/pool1/volumegroups/vg1"
	other := "/subscriptions/sub1/resourceGroups/RG1/providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg2"

	idSet := func(values ...string) types.Set {
		elems := make([]attr.Value, len(values))
		for i, v := range values {
			elems[i] = IDValue{StringValue: types.StringValue(v)}
		}
		set, diags := types.SetValue(IDType{}, elems)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics building test set: %v", diags)
		}
		return set
	}

	valueStrings := func(t *testing.T, s types.Set) []string {
		t.Helper()
		var out []string
		for _, e := range s.Elements() {
			id, ok := e.(IDValue)
			if !ok {
				t.Fatalf("unexpected element type %T", e)
			}
			out = append(out, id.ValueString())
		}
		return out
	}

	runModify := func(state, config, plan types.Set) planmodifier.SetResponse {
		req := planmodifier.SetRequest{
			Path:        path.Root("test"),
			StateValue:  state,
			ConfigValue: config,
			PlanValue:   plan,
		}
		resp := planmodifier.SetResponse{PlanValue: plan}
		PreserveEquivalentIDSpellingInSet().PlanModifySet(ctx, req, &resp)
		return resp
	}

	t.Run("an equivalent spelling in config keeps the state element's spelling", func(t *testing.T) {
		resp := runModify(idSet(canonical), idSet(mangled), idSet(mangled))
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		got := valueStrings(t, resp.PlanValue)
		if len(got) != 1 || got[0] != canonical {
			t.Fatalf("got %v, want [%s]", got, canonical)
		}
	})

	// Respelling only the matching element would yield a set equal to neither config
	// nor prior state, which Terraform rejects for a non-Computed attribute.
	t.Run("a respelled ID alongside an added one leaves the plan unchanged", func(t *testing.T) {
		plan := idSet(mangled, other)
		resp := runModify(idSet(canonical), idSet(mangled, other), plan)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		if !resp.PlanValue.Equal(plan) {
			t.Fatalf("got %v, want plan unchanged %v", valueStrings(t, resp.PlanValue), valueStrings(t, plan))
		}
	})

	t.Run("a respelled ID alongside a removed one leaves the plan unchanged", func(t *testing.T) {
		plan := idSet(mangled)
		resp := runModify(idSet(canonical, other), idSet(mangled), plan)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		if !resp.PlanValue.Equal(plan) {
			t.Fatalf("got %v, want plan unchanged %v", valueStrings(t, resp.PlanValue), valueStrings(t, plan))
		}
	})

	t.Run("two spellings of one state ID don't both map onto it", func(t *testing.T) {
		plan := idSet(canonical, mangled)
		resp := runModify(idSet(canonical, other), idSet(canonical, mangled), plan)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		if !resp.PlanValue.Equal(plan) {
			t.Fatalf("got %v, want plan unchanged %v", valueStrings(t, resp.PlanValue), valueStrings(t, plan))
		}
	})

	t.Run("every element respelled maps the whole plan onto state", func(t *testing.T) {
		state := idSet(canonical, other)
		resp := runModify(state, idSet(mangled, strings.ToUpper(other)), idSet(mangled, strings.ToUpper(other)))
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		if !resp.PlanValue.Equal(state) {
			t.Fatalf("got %v, want state %v", valueStrings(t, resp.PlanValue), valueStrings(t, state))
		}
	})

	t.Run("a null state value is a no-op, as on create", func(t *testing.T) {
		nullSet := types.SetNull(IDType{})
		resp := runModify(nullSet, idSet(mangled), idSet(mangled))
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		got := valueStrings(t, resp.PlanValue)
		if len(got) != 1 || got[0] != mangled {
			t.Fatalf("got %v, want [%s]", got, mangled)
		}
	})

	t.Run("an unknown plan value is a no-op", func(t *testing.T) {
		resp := runModify(idSet(canonical), idSet(mangled), types.SetUnknown(IDType{}))
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		if !resp.PlanValue.IsUnknown() {
			t.Fatal("expected plan value to remain unknown")
		}
	})

	t.Run("a null config value is a no-op", func(t *testing.T) {
		nullSet := types.SetNull(IDType{})
		resp := runModify(idSet(canonical), nullSet, nullSet)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		if !resp.PlanValue.IsNull() {
			t.Fatal("expected plan value to remain null")
		}
	})
}
