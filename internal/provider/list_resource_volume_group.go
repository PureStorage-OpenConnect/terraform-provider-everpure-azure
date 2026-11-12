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
	_ list.ListResource              = &VolumeGroupListResource{}
	_ list.ListResourceWithConfigure = &VolumeGroupListResource{}
)

// VolumeGroupListResourceModel is the configuration of a
// `list "everpure_azure_volume_group"` block.
type VolumeGroupListResourceModel struct {
	StoragePoolID armids.IDValue   `tfsdk:"storage_pool_id"`
	Timeouts      lstimeouts.Value `tfsdk:"timeouts"`
}

type VolumeGroupListResource struct {
	// clientsHolder supplies Configure and volumeGroupsClient via promotion; its
	// Configure already has the signature list.ListResourceWithConfigure asks for.
	clientsHolder
}

func NewVolumeGroupListResource() list.ListResource {
	return &VolumeGroupListResource{}
}

func (l *VolumeGroupListResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_volume_group"
}

func (l *VolumeGroupListResource) ListResourceConfigSchema(ctx context.Context, _ list.ListResourceSchemaRequest, resp *list.ListResourceSchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the Volume Groups on a Storage Pool, so they can be brought under " +
			"Terraform with `terraform query`.\n\n" +
			"List resources require Terraform 1.14 or later, and importing the results by identity " +
			"requires 1.12 or later. Results are sorted by `id`.\n\n" +
			"`terraform query -generate-config-out=<file>` writes an `import` block and a `resource` " +
			"block for each result. `include_resource = true` additionally shows each result's " +
			"attributes in the query output.\n\n" +
			"A `list` block returns at most 100 results by default and gives no warning when it stops there. " +
			"Set the block's `limit` argument to raise the cap.",
		Attributes: map[string]schema.Attribute{
			"storage_pool_id": schema.StringAttribute{
				Required:   true,
				CustomType: armids.IDType{},
				Validators: []validator.String{
					armids.TypeStoragePool.Validator(),
				},
				MarkdownDescription: "Full Azure Resource Manager ID of the Storage Pool to list volume groups on. Must be in the provider's own `subscription_id`.",
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": listTimeoutsBlock(ctx, "the volume groups", defaultVolumeGroupReadTimeout),
		},
	}
}

func (l *VolumeGroupListResource) List(ctx context.Context, req list.ListRequest, stream *list.ListResultsStream) {
	var diags diag.Diagnostics
	client, ok := l.volumeGroupsClient(&diags)
	if !ok {
		stream.Results = list.ListResultsStreamDiagnostics(diags)
		return
	}

	var config VolumeGroupListResourceModel
	diags.Append(req.Config.Get(ctx, &config)...)
	if diags.HasError() {
		stream.Results = list.ListResultsStreamDiagnostics(diags)
		return
	}
	listTimeout, timeoutDiags := config.Timeouts.List(ctx, defaultVolumeGroupReadTimeout)
	diags.Append(timeoutDiags...)
	blank, blankDiags := blankResourceModel[VolumeGroupResourceModel](ctx, req)
	diags.Append(blankDiags...)
	if diags.HasError() {
		stream.Results = list.ListResultsStreamDiagnostics(diags)
		return
	}

	op := operation{action: actionList, kind: kindVolumeGroups, in: config.StoragePoolID.ValueString(), unnamed: true}
	pool, idErr := parseIDInSubscription(armids.ParseStoragePoolID, config.StoragePoolID.ValueString(), kindStoragePool, l.clients.subscriptionID)
	if idErr != nil {
		diags.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(*idErr)))
		stream.Results = list.ListResultsStreamDiagnostics(diags)
		return
	}

	apiCtx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()

	var groups []*armpurestorageblock.VolumeGroup
	pager := client.NewListByStoragePoolPager(pool.ResourceGroupName, pool.StoragePoolName, nil)
	for pager.More() {
		page, err := pager.NextPage(apiCtx)
		if err != nil {
			if isNotFound(err) {
				diags.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(newNotFoundError(kindStoragePool, pool.FullID(), nil, l.clients.apiFailure(err)))))
			} else {
				diags.Append(l.clients.apiCallError(op, err))
			}
			stream.Results = list.ListResultsStreamDiagnostics(diags)
			return
		}
		groups = append(groups, page.Value...)
	}

	sortByAPIID(groups, func(vg *armpurestorageblock.VolumeGroup) *string {
		if vg == nil {
			return nil
		}
		return vg.ID
	})

	stream.Results = listResultsStream(req.Limit, groups, func(vg *armpurestorageblock.VolumeGroup) list.ListResult {
		if vg == nil {
			return errorListResult(unexpectedResponseDiagnostic(newEmptyListEntryError(kindVolumeGroup)))
		}
		model, parsed, diags := importedVolumeGroupFromAPI(ctx, blank, *vg)
		if diags.HasError() {
			return list.ListResult{Diagnostics: diags}
		}

		result := req.NewListResult(ctx)
		result.DisplayName = parsed.VolumeGroupName
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
