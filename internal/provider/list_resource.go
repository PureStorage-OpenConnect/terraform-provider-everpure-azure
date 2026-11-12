// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"iter"
	"sort"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/list"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// blankResourceModel decodes a resource object whose every attribute is a typed null
// into M: the shape state has just before ImportState runs, and the starting point the
// import-equivalent mappings (importedStoragePoolFromAPI and friends) expect. The
// timeouts attribute and nested blocks come out as nulls of their real types, which a
// zero-value model field cannot be (a zero types.List has no element type, and the
// framework rejects it when the object is written back).
func blankResourceModel[M any](ctx context.Context, req list.ListRequest) (M, diag.Diagnostics) {
	var model M
	schemaType := req.ResourceSchema.Type().TerraformType(ctx)
	objectType, ok := schemaType.(tftypes.Object)
	if !ok {
		var diags diag.Diagnostics
		diags.Append(providerInternalDiagnostic(newProviderBugError(fmt.Sprintf("the resource schema does not describe an object, got %T", schemaType))))
		return model, diags
	}
	attributes := make(map[string]tftypes.Value, len(objectType.AttributeTypes))
	for name, attributeType := range objectType.AttributeTypes {
		attributes[name] = tftypes.NewValue(attributeType, nil)
	}
	state := tfsdk.State{Schema: req.ResourceSchema, Raw: tftypes.NewValue(objectType, attributes)}
	return model, state.Get(ctx, &model)
}

// sortByAPIID orders entries by the ID the API returned for them (see idLess), putting
// an entry with no ID first, where the failure it causes is reported before any result
// that depends on list order.
func sortByAPIID[T any](entries []T, id func(T) *string) {
	sort.SliceStable(entries, func(i, j int) bool {
		return idLess(derefString(id(entries[i])), derefString(id(entries[j])))
	})
}

// listResultsStream turns already-collected, already-sorted entries into the result
// stream: at most limit of them (a limit of zero or less means no limit), each built by
// build. An error diagnostic on a built result ends the stream after that result.
//
// Everything that talks to the API happens before this is called: the framework
// consumes the stream after List has returned, when List's own context and timeout are
// already gone.
func listResultsStream[T any](limit int64, entries []T, build func(T) list.ListResult) iter.Seq[list.ListResult] {
	return func(push func(list.ListResult) bool) {
		for i, entry := range entries {
			if limit > 0 && int64(i) >= limit {
				return
			}
			result := build(entry)
			if !push(result) || result.Diagnostics.HasError() {
				return
			}
		}
	}
}

// errorListResult is a result carrying only the given error.
func errorListResult(d diag.Diagnostic) list.ListResult {
	return list.ListResult{Diagnostics: diag.Diagnostics{d}}
}
