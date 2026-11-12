// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

// setIdentityFromID derives a resource's identity from its full ARM ID and
// stores it. The ID must be the one in the resource's state (or about to be
// written to it) — never the API response's own echo of the ID, which ARM may
// spell with different casing: state keeps the prior spelling through semantic
// equality, and an identity that differs from the previously stored one is a
// hard Terraform error on refresh.
func setIdentityFromID[I any](
	ctx context.Context, identity *tfsdk.ResourceIdentity, id string, fromID func(string) (I, error),
) diag.Diagnostics {
	var diags diag.Diagnostics
	if identity == nil {
		return diags
	}
	derived, err := fromID(id)
	if err != nil {
		diags.Append(diag.WithPath(path.Root("id"), invalidAttributeValueDiagnostic(newInvalidStoredIDError(storedInState, idErrorCause(err)))))
		return diags
	}
	diags.Append(identity.Set(ctx, derived)...)
	return diags
}

// setIdentityFromIDIfNull is setIdentityFromID for a Read that is removing the
// resource from state. Terraform discards the identity of a removed resource,
// but the framework still rejects a null one, and a null one is exactly what
// Read receives for state written before identity existed. A present identity
// is left alone.
func setIdentityFromIDIfNull[I any](
	ctx context.Context, identity *tfsdk.ResourceIdentity, id string, fromID func(string) (I, error),
) diag.Diagnostics {
	if identity == nil || !identity.Raw.IsFullyNull() {
		return nil
	}
	return setIdentityFromID(ctx, identity, id, fromID)
}

// resolveImportID returns the full ARM ID to import: req.ID when the
// practitioner gave one, otherwise the ID rebuilt from the import block's
// identity. Callers then run the result through the same parser and checks as
// any import by ID, so both styles reach the same code path.
func resolveImportID[I comparable](
	ctx context.Context, req resource.ImportStateRequest, k kind, fullID func(I) string, fromID func(string) (I, error),
) (string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if req.ID != "" {
		return req.ID, diags
	}
	if req.Identity == nil || req.Identity.Raw.IsNull() {
		diags.Append(operation{action: actionImport, kind: k, unnamed: true}.fail(newMissingImportIDError()))
		return "", diags
	}
	var model I
	diags.Append(req.Identity.Get(ctx, &model)...)
	if diags.HasError() {
		return "", diags
	}
	id := fullID(model)
	// A component containing a path separator would rebuild an ID that parses
	// to a different resource than the identity names; the round trip catches it.
	if parsed, err := fromID(id); err != nil || parsed != model {
		diags.Append(operation{action: actionImport, kind: k, id: id, unnamed: true}.fail(newInvalidImportIdentityError(id)))
		return "", diags
	}
	return id, diags
}

// idOrFallback returns the state's id, or fallback when it is null or empty: the
// tainted state a failed create leaves behind has a null id.
func idOrFallback(id armids.IDValue, fallback string) string {
	if v := id.ValueString(); v != "" {
		return v
	}
	return fallback
}
