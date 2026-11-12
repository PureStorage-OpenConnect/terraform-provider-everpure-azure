// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/attr/xattr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/isoduration"
)

// durationType is the custom string type for snapshot_protection_policy's
// frequency/retention attributes: ISO 8601 durations limited to days-or-smaller
// units (grammar and parsing live in the isoduration package), compared for
// semantic equality (see durationValue.StringSemanticEquals) rather than
// byte-for-byte, since the API canonicalizes durations on read.
type durationType struct {
	basetypes.StringType
}

var _ basetypes.StringTypable = durationType{}

func (t durationType) Equal(o attr.Type) bool {
	other, ok := o.(durationType)
	if !ok {
		return false
	}
	return t.StringType.Equal(other.StringType)
}

func (t durationType) String() string {
	return "provider.durationType"
}

func (t durationType) ValueFromString(_ context.Context, in basetypes.StringValue) (basetypes.StringValuable, diag.Diagnostics) {
	return durationValue{StringValue: in}, nil
}

func (t durationType) ValueFromTerraform(ctx context.Context, in tftypes.Value) (attr.Value, error) {
	attrValue, err := t.StringType.ValueFromTerraform(ctx, in)
	if err != nil {
		return nil, err
	}

	stringValue, ok := attrValue.(basetypes.StringValue)
	if !ok {
		return nil, newProviderBugError(fmt.Sprintf("unexpected value type %T when converting to durationValue", attrValue))
	}

	durationValuable, diags := t.ValueFromString(ctx, stringValue)
	if diags.HasError() {
		return nil, newProviderBugError(fmt.Sprintf("unexpected error converting StringValue to durationValue: %v", diags))
	}

	return durationValuable, nil
}

func (t durationType) ValueType(_ context.Context) attr.Value {
	return durationValue{}
}

// durationValue is an ISO 8601 duration string that compares equal to another one
// spelled differently but denoting the same length of time (e.g. "PT60M" and "PT1H"),
// so refreshing state with the API's canonicalized spelling never shows as drift
// against a config that used an equivalent, uncanonicalized one.
type durationValue struct {
	basetypes.StringValue
}

var (
	_ basetypes.StringValuableWithSemanticEquals = durationValue{}
	_ xattr.ValidateableAttribute                = durationValue{}
)

func (v durationValue) Type(_ context.Context) attr.Type {
	return durationType{}
}

func (v durationValue) Equal(o attr.Value) bool {
	other, ok := o.(durationValue)
	if !ok {
		return false
	}
	return v.StringValue.Equal(other.StringValue)
}

// StringSemanticEquals is called by the framework with v as the proposed new value
// (typically from config) and priorValuable as the value already in state (typically
// the API's last, canonicalized response); returning true tells the framework to keep
// the prior state value instead of showing a diff. It covers only the values the
// provider returns; preserveEquivalentDurationSpelling below is the plan-time half,
// and changing one usually means changing the other.
func (v durationValue) StringSemanticEquals(_ context.Context, priorValuable basetypes.StringValuable) (bool, diag.Diagnostics) {
	var diags diag.Diagnostics

	priorValue, ok := priorValuable.(durationValue)
	if !ok {
		diags.Append(providerInternalDiagnostic(newProviderBugError(fmt.Sprintf("unexpected value type %T for durationValue.StringSemanticEquals", priorValuable))))
		return false, diags
	}

	if v.IsNull() || v.IsUnknown() || priorValue.IsNull() || priorValue.IsUnknown() {
		return false, diags
	}

	// An unparseable value on either side can't happen through the schema (the
	// grammar is enforced at validation time via ValidateAttribute below), but
	// StringSemanticEquals can still run against a value that hasn't been
	// validated yet; isoduration.Equal's error in that case is treated the same
	// as "not equal", not surfaced as a diagnostic, since ValidateAttribute is
	// what's responsible for reporting a malformed duration to the user.
	equal, err := isoduration.Equal(v.ValueString(), priorValue.ValueString())
	if err != nil {
		return false, diags
	}

	return equal, diags
}

// ValidateAttribute implements xattr.ValidateableAttribute, so grammar
// validation travels with durationType wherever it's used as an attribute's
// CustomType, without needing a validator attached separately on each
// attribute.
func (v durationValue) ValidateAttribute(_ context.Context, req xattr.ValidateAttributeRequest, resp *xattr.ValidateAttributeResponse) {
	if v.IsNull() || v.IsUnknown() {
		return
	}
	if _, err := isoduration.Parse(v.ValueString()); err != nil {
		resp.Diagnostics.Append(attributeValidatorError(req.Path, notISODurationRule, v.ValueString()))
	}
}

// preserveEquivalentDurationSpelling keeps the prior state's spelling of a
// durationType attribute when config spells the same length of time differently.
//
// StringSemanticEquals above only covers values the provider itself returns, from
// Create, Read and Update; the framework never consults it while building a plan.
// So state that didn't come from an earlier plan holds whatever spelling its source
// used — an imported resource carries the service's own — and every plan proposes
// rewriting it to the config's equivalent spelling until an apply finally does.
func preserveEquivalentDurationSpelling() planmodifier.String {
	return durationEquivalencePlanModifier{}
}

type durationEquivalencePlanModifier struct{}

var _ planmodifier.String = durationEquivalencePlanModifier{}

func (m durationEquivalencePlanModifier) Description(_ context.Context) string {
	return "Keeps the prior state's spelling of this duration when the configured value denotes the same " +
		"length of time, so an equivalent but differently-spelled duration in configuration (as commonly " +
		"happens after importing a resource) doesn't show as a permanent diff."
}

func (m durationEquivalencePlanModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (m durationEquivalencePlanModifier) PlanModifyString(
	_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse,
) {
	if req.StateValue.IsNull() || req.PlanValue.IsUnknown() || req.ConfigValue.IsNull() {
		return
	}

	// An unparseable value can't happen through the schema (the grammar is
	// enforced at validation time via ValidateAttribute above), but this can
	// still run against a value that hasn't been validated yet; isoduration.Equal's
	// error in that case is treated the same as "not equal", not surfaced as a
	// diagnostic, since ValidateAttribute is what's responsible for reporting a
	// malformed duration to the user.
	equal, err := isoduration.Equal(req.PlanValue.ValueString(), req.StateValue.ValueString())
	if err != nil {
		return
	}

	if equal {
		resp.PlanValue = req.StateValue
	}
}
