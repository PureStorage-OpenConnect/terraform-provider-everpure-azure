// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// spaceObjectType mirrors the "space" attribute's schema, so types.ObjectValue can build
// one directly.
var spaceObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"total_used_in_bytes": types.Int64Type,
	"unique_in_bytes":     types.Int64Type,
	"snapshots_in_bytes":  types.Int64Type,
	"shared_in_bytes":     types.Int64Type,
}}

// spaceAttribute is the computed "space" attribute. The API returns the same Space
// object on every API object that reports space usage, so every data source attribute
// that exposes it shares this one definition.
func spaceAttribute() schema.Attribute {
	return schema.SingleNestedAttribute{
		Computed:            true,
		MarkdownDescription: "Space usage, in bytes. Null when the service reports none.",
		Attributes: map[string]schema.Attribute{
			"total_used_in_bytes": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Total space occupied by customer data, in bytes. This is the space being billed for.",
			},
			"unique_in_bytes": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Unique space occupied by customer data, in bytes.",
			},
			"snapshots_in_bytes": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Space occupied by data unique to one or more snapshots, in bytes.",
			},
			"shared_in_bytes": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "Space occupied by duplicated data, meaning data shared among different volumes and snapshots as a " +
					"result of data deduplication, in bytes.",
			},
		},
	}
}

// spaceFromAPI maps the API's Space onto the shared "space" attribute. A nil Space maps
// to a null object; a nil field inside a present Space maps to a null attribute.
func spaceFromAPI(s *armpurestorageblock.Space) (types.Object, diag.Diagnostics) {
	if s == nil {
		return types.ObjectNull(spaceObjectType.AttrTypes), nil
	}
	return types.ObjectValue(spaceObjectType.AttrTypes, map[string]attr.Value{
		"total_used_in_bytes": types.Int64PointerValue(s.TotalUsed),
		"unique_in_bytes":     types.Int64PointerValue(s.Unique),
		"snapshots_in_bytes":  types.Int64PointerValue(s.Snapshots),
		"shared_in_bytes":     types.Int64PointerValue(s.Shared),
	})
}
