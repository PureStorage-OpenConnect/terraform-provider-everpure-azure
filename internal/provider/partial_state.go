// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// setPartialState saves data, taken from the plan, as the state a create leaves while it
// waits for Azure: if the wait then fails, Terraform keeps the resource as tainted instead
// of losing track of it. Every value still unknown in the plan, such as a computed
// attribute Azure has not returned yet, is saved as null. Terraform rejects an unknown
// value in the state an apply returns, even alongside the create's own error, and reports
// each one as a separate provider bug.
func setPartialState(ctx context.Context, state *tfsdk.State, data any) diag.Diagnostics {
	diags := state.Set(ctx, data)
	if diags.HasError() {
		return diags
	}
	raw, err := tftypes.Transform(state.Raw, func(_ *tftypes.AttributePath, v tftypes.Value) (tftypes.Value, error) {
		if v.IsKnown() {
			return v, nil
		}
		return tftypes.NewValue(v.Type(), nil), nil
	})
	if err != nil {
		diags.Append(providerInternalDiagnostic(newProviderBugError(err.Error())))
		return diags
	}
	state.Raw = raw
	return diags
}
