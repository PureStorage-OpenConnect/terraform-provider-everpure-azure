// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// useStateWhenSiblingUnchanged keeps a Computed attribute's prior state value when the
// attribute it is derived from is planned to the same value. The framework marks every
// Computed attribute without a configured value unknown as soon as the plan differs from
// state anywhere, before the attribute plan modifiers run. A difference that those
// modifiers later erase (a case-only edit of an ID, which they respell back to state)
// would therefore leave such an attribute unknown and the resource planned for an
// update that changes nothing, on every plan thereafter.
func useStateWhenSiblingUnchanged(sibling path.Path) planmodifier.Int64 {
	return siblingUnchangedModifier{sibling: sibling}
}

type siblingUnchangedModifier struct {
	sibling path.Path
}

var _ planmodifier.Int64 = siblingUnchangedModifier{}

func (m siblingUnchangedModifier) Description(context.Context) string {
	return "Keeps the prior state value while " + m.sibling.String() + " is unchanged."
}

func (m siblingUnchangedModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (m siblingUnchangedModifier) PlanModifyInt64(ctx context.Context, req planmodifier.Int64Request, resp *planmodifier.Int64Response) {
	if req.State.Raw.IsNull() || req.StateValue.IsNull() || !req.PlanValue.IsUnknown() {
		return
	}

	var planned, prior types.Int64
	if resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, m.sibling, &planned)...); resp.Diagnostics.HasError() {
		return
	}
	if resp.Diagnostics.Append(req.State.GetAttribute(ctx, m.sibling, &prior)...); resp.Diagnostics.HasError() {
		return
	}
	if planned.IsUnknown() || !planned.Equal(prior) {
		return
	}

	resp.PlanValue = req.StateValue
}
