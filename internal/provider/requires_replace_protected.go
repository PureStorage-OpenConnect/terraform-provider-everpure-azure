// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const requiresReplaceProtectedDescription = "If the value of this attribute changes, Terraform will destroy and " +
	"recreate the resource. Blocked while deletion_protection is true."

// requiresReplaceProtected returns a plan modifier that requires replacement exactly
// like stringplanmodifier.RequiresReplace, built as a thin RequiresReplaceIf wrapper
// so create/destroy/equality handling (skip on create, skip on destroy, skip when
// unchanged) is inherited from the framework rather than hand-mirrored. On top of
// that, when this attribute's change is what triggers the replacement and the
// resource's own deletion_protection was true in the prior state, the ifFunc raises
// the same deletion-protected error a blocked destroy would, naming this attribute.
// k names the resource in that error.
//
// A resource applies this to every immutable attribute instead of
// stringplanmodifier.RequiresReplace directly, so the guard covers every
// RequiresReplace attribute by construction: a future one added to the schema is
// automatically protected, with no second, hand-maintained list of attribute names to
// keep in sync.
func requiresReplaceProtected(k kind) planmodifier.String {
	return stringplanmodifier.RequiresReplaceIf(
		func(ctx context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
			// RequiresReplaceIf only calls this once it has already established the
			// attribute genuinely changed on a real update (not create, not destroy), so
			// reaching here always means a replacement.
			resp.RequiresReplace = true
			resp.Diagnostics.Append(checkDeletionProtectedForReplace(ctx, k, req.State, req.Path)...)
		},
		requiresReplaceProtectedDescription, requiresReplaceProtectedDescription,
	)
}

// requiresReplaceProtectedList is requiresReplaceProtected's list-block counterpart,
// for a nested block whose addition, removal or edit must force replacement under the
// same deletion-protection guard — e.g. the volume's source_snapshot block, where the
// block's presence and its two fields all travel together as one unit.
func requiresReplaceProtectedList(k kind) planmodifier.List {
	return listplanmodifier.RequiresReplaceIf(
		func(ctx context.Context, req planmodifier.ListRequest, resp *listplanmodifier.RequiresReplaceIfFuncResponse) {
			resp.RequiresReplace = true
			resp.Diagnostics.Append(checkDeletionProtectedForReplace(ctx, k, req.State, req.Path)...)
		},
		requiresReplaceProtectedDescription, requiresReplaceProtectedDescription,
	)
}

// checkDeletionProtectedForReplace reads deletion_protection and name off the prior
// state and, if deletion_protection was true, returns the same deletion-protected
// diagnostic a blocked destroy would raise, naming attributePath as the trigger. It is
// the shared body behind requiresReplaceProtected and requiresReplaceProtectedList, so
// the two request/response types (string vs. list) cannot drift in what they check or
// how they phrase the error.
func checkDeletionProtectedForReplace(ctx context.Context, k kind, state tfsdk.State, attributePath path.Path) diag.Diagnostics {
	var diags diag.Diagnostics

	var deletionProtection types.Bool
	diags.Append(state.GetAttribute(ctx, path.Root("deletion_protection"), &deletionProtection)...)
	if diags.HasError() {
		return diags
	}
	if !deletionProtection.ValueBool() {
		return diags
	}

	var name types.String
	diags.Append(state.GetAttribute(ctx, path.Root("name"), &name)...)
	if diags.HasError() {
		return diags
	}

	diags.Append(diag.WithPath(attributePath, deletionProtectedDiagnostic(k, newDeletionProtectedError(k, name.ValueString()))))
	return diags
}
