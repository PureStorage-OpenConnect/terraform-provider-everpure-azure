// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// tagsToAPI returns nil only for a null tags value. The generated SDK's
// marshaler omits the tags key for a nil map but still sends an explicit
// "tags": {} for a non-nil empty map, and ARM treats those differently on
// PATCH: omitted leaves existing tags alone, "{}" clears them. Create relies
// on that directly (a null config on a brand-new resource has nothing to leave
// alone or clear, so omitting is fine); tagsToAPIForUpdate below collapses the
// nil result to a non-nil empty map so an Update PATCH can still clear tags
// when the plan says null.
func tagsToAPI(ctx context.Context, tags types.Map) (map[string]*string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if tags.IsNull() {
		return nil, diags
	}
	var m map[string]string
	diags.Append(tags.ElementsAs(ctx, &m, false)...)
	result := make(map[string]*string, len(m))
	for k, v := range m {
		result[k] = to.Ptr(v)
	}
	return result, diags
}

// tagsToAPIForUpdate builds a PATCH body's tags value from the plan, for any
// resource whose tags attribute is Optional but not Computed: the plan's tags
// value then always equals the config's, so a config with no tags block plans
// as null, never as "whatever is there now". tagsToAPI maps that null to a nil
// map, which the generated SDK's marshaler omits from the request — leaving
// existing tags on the resource untouched instead of clearing them, silently
// breaking the promise the plan just made. Collapsing to a non-nil empty map
// here keeps the two in sync: the PATCH always clears tags when the plan says
// null.
func tagsToAPIForUpdate(ctx context.Context, tags types.Map) (map[string]*string, diag.Diagnostics) {
	apiTags, diags := tagsToAPI(ctx, tags)
	if apiTags == nil {
		apiTags = map[string]*string{}
	}
	return apiTags, diags
}

func tagsFromAPI(ctx context.Context, apiTags map[string]*string, configTags types.Map) (types.Map, diag.Diagnostics) {
	// The API returns nil/empty when no tags are set; treat that as null to
	// match a null config rather than showing a perpetual empty-map diff.
	if len(apiTags) == 0 {
		if configTags.IsNull() {
			return types.MapNull(types.StringType), nil
		}
		return types.MapValueFrom(ctx, types.StringType, map[string]string{})
	}
	m := make(map[string]string, len(apiTags))
	for k, v := range apiTags {
		if v != nil {
			m[k] = *v
		}
	}
	return types.MapValueFrom(ctx, types.StringType, m)
}
