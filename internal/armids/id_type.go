// SPDX-License-Identifier: Apache-2.0

package armids

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// IDType is the string type for full-ARM-ID attributes. ARM IDs fold
// case-insensitively, so two spellings differing only in case name the same resource
// and must compare equal. Deliberately not used for "name" attributes: the API has no
// rename, so a case-only edit to a name can only mean replacement, which must stay a
// visible diff.
type IDType struct {
	basetypes.StringType
}

var _ basetypes.StringTypable = IDType{}

func (t IDType) Equal(o attr.Type) bool {
	other, ok := o.(IDType)
	if !ok {
		return false
	}
	return t.StringType.Equal(other.StringType)
}

func (t IDType) String() string {
	return "armids.IDType"
}

func (t IDType) ValueFromString(_ context.Context, in basetypes.StringValue) (basetypes.StringValuable, diag.Diagnostics) {
	return IDValue{StringValue: in}, nil
}

func (t IDType) ValueFromTerraform(ctx context.Context, in tftypes.Value) (attr.Value, error) {
	attrValue, err := t.StringType.ValueFromTerraform(ctx, in)
	if err != nil {
		return nil, err
	}

	stringValue, ok := attrValue.(basetypes.StringValue)
	if !ok {
		return nil, fmt.Errorf("unexpected value type %T when converting to armids.IDValue", attrValue)
	}

	idValuable, diags := t.ValueFromString(ctx, stringValue)
	if diags.HasError() {
		return nil, fmt.Errorf("unexpected error converting StringValue to armids.IDValue: %v", diags)
	}

	return idValuable, nil
}

func (t IDType) ValueType(_ context.Context) attr.Value {
	return IDValue{}
}

// IDValue is a full ARM resource ID that compares equal to another one spelled
// with different casing but naming the same resource, so refreshing state with a
// differently-cased (but equivalent) ID — an ID this package itself composed in
// its own canonical casing, in particular — never reads as configuration drift
// against an equivalent value the practitioner spelled differently.
type IDValue struct {
	basetypes.StringValue
}

var _ basetypes.StringValuableWithSemanticEquals = IDValue{}

func (v IDValue) Type(_ context.Context) attr.Type {
	return IDType{}
}

func (v IDValue) Equal(o attr.Value) bool {
	other, ok := o.(IDValue)
	if !ok {
		return false
	}
	return v.StringValue.Equal(other.StringValue)
}

// StringSemanticEquals reports whether two IDs name the same resource, so the
// framework keeps the prior state value rather than showing a diff. The framework
// calls it from Create, Read and Update but never while building a plan —
// PreserveEquivalentIDSpelling is that half, and changing one usually means
// changing the other.
func (v IDValue) StringSemanticEquals(_ context.Context, priorValuable basetypes.StringValuable) (bool, diag.Diagnostics) {
	var diags diag.Diagnostics

	priorValue, ok := priorValuable.(IDValue)
	if !ok {
		diags.AddError(
			"semantic equality check error",
			fmt.Sprintf("unexpected value type %T for armids.IDValue.StringSemanticEquals", priorValuable),
		)
		return false, diags
	}

	if v.IsNull() || v.IsUnknown() || priorValue.IsNull() || priorValue.IsUnknown() {
		return false, diags
	}

	return strings.EqualFold(v.ValueString(), priorValue.ValueString()), diags
}

// PreserveEquivalentIDSpelling is the plan-time half of IDType, keeping state's
// spelling when config names the same ARM ID differently. Without it, state whose
// spelling came from anywhere other than an earlier plan is proposed for rewriting on
// every plan. Import is the case that matters: it composes parent IDs in this
// package's own canonical casing (ResourceType.nestedID, ResourceGroupID.prefix),
// which need not match how the practitioner's config spells them.
func PreserveEquivalentIDSpelling() planmodifier.String {
	return idEquivalencePlanModifier{}
}

// PreserveEquivalentResourceGroupName is PreserveEquivalentIDSpelling for a resource
// group name, which Azure compares case-insensitively like the IDs it appears in. Run
// before RequiresReplace, it keeps a case-only edit from replacing a resource that
// lives in the same resource group; resource names themselves are left out, for the
// reason on IDType.
func PreserveEquivalentResourceGroupName() planmodifier.String {
	return idEquivalencePlanModifier{}
}

type idEquivalencePlanModifier struct{}

var _ planmodifier.String = idEquivalencePlanModifier{}

func (m idEquivalencePlanModifier) Description(_ context.Context) string {
	return "Keeps the prior state's spelling of this ARM ID when the configured value names the same " +
		"resource, so an equivalent but differently-cased ID in configuration (as commonly happens after " +
		"importing a resource) doesn't show as a permanent diff."
}

func (m idEquivalencePlanModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (m idEquivalencePlanModifier) PlanModifyString(
	_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse,
) {
	if req.StateValue.IsNull() || req.PlanValue.IsUnknown() || req.ConfigValue.IsNull() {
		return
	}

	if strings.EqualFold(req.PlanValue.ValueString(), req.StateValue.ValueString()) {
		resp.PlanValue = req.StateValue
	}
}

// PreserveEquivalentIDSpellingInSet is PreserveEquivalentIDSpelling's set-of-IDs
// counterpart, for a `types.SetType{ElemType: armids.IDType{}}` attribute (e.g. a set
// of volume group IDs): IDType itself is reused as the set's element type — it already
// satisfies basetypes.StringTypable, so no separate element type is needed. Order
// within a set carries no meaning, so this compares element-wise rather than
// position-wise. Without this, state whose spelling came from anywhere other than an
// earlier plan — importing a resource that references such a set is the case that
// matters — is proposed for rewriting on every plan.
//
// The substitution is all-or-nothing: the plan is replaced by the prior state set
// only when its elements pair up one-to-one, case-insensitively, with the state's.
// For an attribute that isn't Computed, Terraform accepts a planned value only if it
// equals either the configured value or the prior state value as a whole, so
// respelling just the matching elements of a set that also gained, lost or
// duplicated an element would produce a plan equal to neither and be rejected.
func PreserveEquivalentIDSpellingInSet() planmodifier.Set {
	return idSetEquivalencePlanModifier{}
}

type idSetEquivalencePlanModifier struct{}

var _ planmodifier.Set = idSetEquivalencePlanModifier{}

func (m idSetEquivalencePlanModifier) Description(_ context.Context) string {
	return "Keeps each prior state element's spelling of an ARM ID when the configured set contains an " +
		"equivalent (case-insensitively identical) value, so a differently-cased ID in configuration " +
		"(as commonly happens after importing a resource) doesn't show as a permanent diff."
}

func (m idSetEquivalencePlanModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (m idSetEquivalencePlanModifier) PlanModifySet(
	_ context.Context, req planmodifier.SetRequest, resp *planmodifier.SetResponse,
) {
	if req.StateValue.IsNull() || req.PlanValue.IsUnknown() || req.ConfigValue.IsNull() {
		return
	}

	stateElems := req.StateValue.Elements()
	planElems := req.PlanValue.Elements()
	if len(planElems) != len(stateElems) || req.PlanValue.Equal(req.StateValue) {
		return
	}

	used := make([]bool, len(stateElems))
	for _, planElem := range planElems {
		planID, ok := planElem.(IDValue)
		if !ok || planID.IsUnknown() || planID.IsNull() {
			return
		}
		matched := false
		for i, stateElem := range stateElems {
			stateID, ok := stateElem.(IDValue)
			if used[i] || !ok || stateID.IsUnknown() || stateID.IsNull() {
				continue
			}
			if strings.EqualFold(planID.ValueString(), stateID.ValueString()) {
				used[i] = true
				matched = true
				break
			}
		}
		if !matched {
			return
		}
	}

	resp.PlanValue = req.StateValue
}
