// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// platformConsoleConfigChanged reports whether the platform_console configuration
// differs between prior and plan: the block is present in one and absent in the
// other, or one of its user-configurable attributes (gui_enabled, api_enabled,
// cli_enabled) differs. The computed-only attributes (subnet_ids, default_username,
// subnet) are never compared: they are the API's answer to the configuration, and
// they are unknown in any plan that touches the resource, so comparing them would
// report a change for every update.
//
// An unknown list, or a configurable attribute that is unknown on either side, counts
// as changed: nothing known can show the configuration is the same.
//
// Both the plan modifier below and toStoragePoolUpdate use this, so what counts as
// "changed" (carry computed values forward, and leave the console out of the PATCH
// body) is decided in exactly one place.
func platformConsoleConfigChanged(ctx context.Context, prior, plan types.List) (bool, diag.Diagnostics) {
	var diags diag.Diagnostics
	if prior.IsUnknown() || plan.IsUnknown() {
		return true, diags
	}

	priorElems, d := blockElements[PlatformConsoleModel](ctx, prior)
	diags.Append(d...)
	planElems, d := blockElements[PlatformConsoleModel](ctx, plan)
	diags.Append(d...)
	if diags.HasError() {
		return true, diags
	}

	if len(priorElems) != len(planElems) {
		return true, diags
	}
	for i := range planElems {
		p, n := priorElems[i], planElems[i]
		if !p.GUIEnabled.Equal(n.GUIEnabled) || !p.APIEnabled.Equal(n.APIEnabled) || !p.CLIEnabled.Equal(n.CLIEnabled) {
			return true, diags
		}
	}
	return false, diags
}

// platformConsoleKeepComputed returns a plan modifier for the platform_console block
// that, while the console configuration is unchanged from state, copies the block's
// computed-only attributes (subnet_ids, default_username, subnet) from state into the
// plan.
//
// Without it the framework marks those attributes unknown in every plan that changes
// anything on the resource: the update then shows them as "known after apply", and
// Update's no-op check (plan equals state for every API-backed attribute) can never be
// true for a pool that has the block, so a timeouts-only change sends a PATCH.
//
// It is one modifier on the block, not UseStateForUnknown on each nested attribute,
// because UseStateForUnknown is unconditional: when a sibling setting changes, the
// API can return different values for those attributes, and carrying the old ones
// forward would make the planned values wrong and fail the apply with an inconsistent
// result. When the configuration did change the plan is left alone, so they stay
// unknown and are refreshed from the API response.
func platformConsoleKeepComputed() planmodifier.List {
	return platformConsoleKeepComputedModifier{}
}

type platformConsoleKeepComputedModifier struct{}

func (platformConsoleKeepComputedModifier) Description(context.Context) string {
	return "Keeps the computed platform_console attributes from state while the console settings are unchanged."
}

func (m platformConsoleKeepComputedModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (platformConsoleKeepComputedModifier) PlanModifyList(ctx context.Context, req planmodifier.ListRequest, resp *planmodifier.ListResponse) {
	if req.StateValue.IsNull() || req.StateValue.IsUnknown() || req.PlanValue.IsNull() || req.PlanValue.IsUnknown() {
		return
	}

	changed, diags := platformConsoleConfigChanged(ctx, req.StateValue, req.PlanValue)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() || changed {
		return
	}

	stateElems, d := blockElements[PlatformConsoleModel](ctx, req.StateValue)
	resp.Diagnostics.Append(d...)
	planElems, d := blockElements[PlatformConsoleModel](ctx, req.PlanValue)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() || len(stateElems) != 1 || len(planElems) != 1 {
		return
	}

	planElems[0].SubnetIDs = stateElems[0].SubnetIDs
	planElems[0].DefaultUsername = stateElems[0].DefaultUsername
	planElems[0].Subnet = stateElems[0].Subnet

	merged, d := types.ListValueFrom(ctx, platformConsoleObjectType, planElems)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	resp.PlanValue = merged
}
