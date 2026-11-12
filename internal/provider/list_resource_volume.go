// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	lstimeouts "github.com/hashicorp/terraform-plugin-framework-timeouts/list/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/list"
	"github.com/hashicorp/terraform-plugin-framework/list/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

var (
	_ list.ListResource              = &VolumeListResource{}
	_ list.ListResourceWithConfigure = &VolumeListResource{}
)

// VolumeListResourceModel is the configuration of a `list "everpure_azure_volume"` block.
type VolumeListResourceModel struct {
	VolumeGroupID armids.IDValue   `tfsdk:"volume_group_id"`
	Timeouts      lstimeouts.Value `tfsdk:"timeouts"`
}

type VolumeListResource struct {
	// clientsHolder supplies Configure and volumesClient via promotion; its Configure
	// already has the signature list.ListResourceWithConfigure asks for.
	clientsHolder
}

func NewVolumeListResource() list.ListResource {
	return &VolumeListResource{}
}

func (l *VolumeListResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_volume"
}

func (l *VolumeListResource) ListResourceConfigSchema(ctx context.Context, _ list.ListResourceSchemaRequest, resp *list.ListResourceSchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the Volumes in a Volume Group, so they can be brought under Terraform " +
			"with `terraform query`. Soft-deleted (destroyed, not yet eradicated) volumes are never " +
			"listed: the `everpure_azure_volume` resource treats them as absent, so importing one would fail.\n\n" +
			"List resources require Terraform 1.14 or later, and importing the results by identity " +
			"requires 1.12 or later. Results are sorted by `id`.\n\n" +
			"`terraform query -generate-config-out=<file>` writes an `import` block and a `resource` " +
			"block for each result. `include_resource = true` additionally shows each result's " +
			"attributes in the query output.\n\n" +
			"A `list` block returns at most 100 results by default and gives no warning when it stops there. " +
			"Set the block's `limit` argument to raise the cap.",
		Attributes: map[string]schema.Attribute{
			"volume_group_id": schema.StringAttribute{
				Required:   true,
				CustomType: armids.IDType{},
				Validators: []validator.String{
					armids.TypeVolumeGroup.Validator(),
				},
				MarkdownDescription: "Full Azure Resource Manager ID of the volume group to list volumes in. Must be in the provider's own `subscription_id`.",
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": listTimeoutsBlock(ctx, "the volumes", defaultVolumeReadTimeout),
		},
	}
}

func (l *VolumeListResource) List(ctx context.Context, req list.ListRequest, stream *list.ListResultsStream) {
	var diags diag.Diagnostics
	client, ok := l.volumesClient(&diags)
	if !ok {
		stream.Results = list.ListResultsStreamDiagnostics(diags)
		return
	}

	var config VolumeListResourceModel
	diags.Append(req.Config.Get(ctx, &config)...)
	if diags.HasError() {
		stream.Results = list.ListResultsStreamDiagnostics(diags)
		return
	}
	listTimeout, timeoutDiags := config.Timeouts.List(ctx, defaultVolumeReadTimeout)
	diags.Append(timeoutDiags...)
	blank, blankDiags := blankResourceModel[VolumeResourceModel](ctx, req)
	diags.Append(blankDiags...)
	if diags.HasError() {
		stream.Results = list.ListResultsStreamDiagnostics(diags)
		return
	}

	op := operation{action: actionList, kind: kindVolumes, in: config.VolumeGroupID.ValueString(), unnamed: true}
	vg, idErr := parseIDInSubscription(armids.ParseVolumeGroupID, config.VolumeGroupID.ValueString(), kindVolumeGroup, l.clients.subscriptionID)
	if idErr != nil {
		diags.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(*idErr)))
		stream.Results = list.ListResultsStreamDiagnostics(diags)
		return
	}

	apiCtx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()

	var volumes []*armpurestorageblock.Volume
	pager := client.NewListByVolumeGroupPager(vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName, nil)
	for pager.More() {
		page, err := pager.NextPage(apiCtx)
		if err != nil {
			if isNotFound(err) {
				diags.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(newNotFoundError(kindVolumeGroup, vg.FullID(), nil, l.clients.apiFailure(err)))))
			} else {
				diags.Append(l.clients.apiCallError(op, err))
			}
			stream.Results = list.ListResultsStreamDiagnostics(diags)
			return
		}
		volumes = append(volumes, page.Value...)
	}

	// Soft-deleted volumes are dropped here, before sorting and before Limit applies.
	// filterAndSortVolumes isn't used: it also silently drops an entry with no ID,
	// which a list resource must instead report (see the nil check below).
	live := make([]*armpurestorageblock.Volume, 0, len(volumes))
	for _, v := range volumes {
		if v != nil && isVolumeSoftDeleted(*v) {
			continue
		}
		live = append(live, v)
	}
	sortByAPIID(live, func(v *armpurestorageblock.Volume) *string {
		if v == nil {
			return nil
		}
		return v.ID
	})

	stream.Results = listResultsStream(req.Limit, live, func(v *armpurestorageblock.Volume) list.ListResult {
		if v == nil {
			return errorListResult(unexpectedResponseDiagnostic(newEmptyListEntryError(kindVolume)))
		}
		model, parsed, diags := importedVolumeFromAPI(blank, *v)
		if diags.HasError() {
			return list.ListResult{Diagnostics: diags}
		}

		result := req.NewListResult(ctx)
		result.DisplayName = parsed.VolumeName
		if !model.Name.IsNull() {
			result.DisplayName = model.Name.ValueString()
		}
		result.Diagnostics.Append(result.Identity.Set(ctx, parsed)...)
		if req.IncludeResource {
			result.Diagnostics.Append(result.Resource.Set(ctx, &model)...)
		}
		return result
	})
}
