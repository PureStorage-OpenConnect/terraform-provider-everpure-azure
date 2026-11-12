// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// softDeletionObjectType mirrors the "soft_deletion" attribute's schema, so
// types.ObjectValue can build one directly and types.ListValueFrom can build a list of
// volumeListItem/recoverableVolumeGroupDataSourceItem values that embed it.
var softDeletionObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"previous_name":         types.StringType,
	"destroyed_at":          types.StringType,
	"eradication_timestamp": types.StringType,
}}

// softDeletionAttribute is the "soft_deletion" attribute shared by the plural
// everpure_azure_volumes data source's volume elements and the
// everpure_azure_recoverable_volume_groups data source's elements. objectNoun is how the
// destroyed object reads in a sentence like "the name this <objectNoun> had before it
// was destroyed" (e.g. "volume", "volume group").
func softDeletionAttribute(objectNoun string) schema.Attribute {
	return schema.SingleNestedAttribute{
		Computed: true,
		MarkdownDescription: fmt.Sprintf(
			"Soft-delete state, present only when this %s is soft-deleted (destroyed, not yet eradicated).",
			objectNoun,
		),
		Attributes: map[string]schema.Attribute{
			"previous_name": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: fmt.Sprintf("The name this %s had before it was destroyed.", objectNoun),
			},
			"destroyed_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When it was destroyed, as an RFC 3339 timestamp.",
			},
			"eradication_timestamp": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Approximate time at which the object will be eradicated automatically, as an " +
					"RFC 3339 timestamp. Derived from the remaining retention when read, so it can change by seconds " +
					"to minutes between reads; do not use it for exact comparisons or as an input to other " +
					"resources. Use `destroyed_at` as the stable reference.",
			},
		},
	}
}

// softDeletionFromAPI maps a volume's destroyed-state object from the API onto the
// shared soft_deletion attribute. A nil pointer, or one whose Destroyed flag is nil or
// false, means the volume is live, which maps to a null soft_deletion rather than an
// object with null fields — the object's own presence is what a recovery config filters
// on.
func softDeletionFromAPI(sd *armpurestorageblock.DestroyedStateProperties) (types.Object, diag.Diagnostics) {
	if sd == nil || sd.Destroyed == nil || !*sd.Destroyed {
		return types.ObjectNull(softDeletionObjectType.AttrTypes), nil
	}
	return softDeletionDetailsFromAPI(sd)
}

// softDeletionDetailsFromAPI maps a destroyed-state object onto a non-null
// soft_deletion regardless of its Destroyed flag. It serves recoverable volume groups,
// which are destroyed by virtue of being listed at all: gating on the flag there would
// null the whole object whenever the API omitted it, turning a config's
// soft_deletion.previous_name lookup into an attribute-of-null error. A nil pointer
// yields an object whose fields are all null. Each nil field maps to a null string,
// never a formatted zero time.
func softDeletionDetailsFromAPI(sd *armpurestorageblock.DestroyedStateProperties) (types.Object, diag.Diagnostics) {
	if sd == nil {
		sd = &armpurestorageblock.DestroyedStateProperties{}
	}

	destroyedAt := types.StringNull()
	if sd.DestroyedAt != nil {
		destroyedAt = types.StringValue(sd.DestroyedAt.Format(time.RFC3339))
	}
	eradicationTimestamp := types.StringNull()
	if sd.EradicationTimestamp != nil {
		eradicationTimestamp = types.StringValue(sd.EradicationTimestamp.Format(time.RFC3339))
	}

	return types.ObjectValue(softDeletionObjectType.AttrTypes, map[string]attr.Value{
		"previous_name":         types.StringPointerValue(sd.PreviousName),
		"destroyed_at":          destroyedAt,
		"eradication_timestamp": eradicationTimestamp,
	})
}
