// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// blockElements decodes a nested-block list into its model slice. A null list
// (block absent) and, defensively, an unknown one both come back as no elements:
// apply-time callers only run once the plan has resolved the block's value, so
// seeing it unknown there would itself be a bug elsewhere — treating it the same
// as absent is the safe fallback rather than a crash. A dynamic block's for_each
// can make the list unknown at config-validate time; validate/plan-time callers
// that care about that distinction must check IsUnknown before calling this.
func blockElements[T any](ctx context.Context, list types.List) ([]T, diag.Diagnostics) {
	var diags diag.Diagnostics
	if list.IsNull() || list.IsUnknown() {
		return nil, diags
	}
	var elems []T
	diags.Append(list.ElementsAs(ctx, &elems, false)...)
	return elems, diags
}
