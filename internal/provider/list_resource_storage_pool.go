// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	lstimeouts "github.com/hashicorp/terraform-plugin-framework-timeouts/list/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/list"
	"github.com/hashicorp/terraform-plugin-framework/list/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ list.ListResource              = &StoragePoolListResource{}
	_ list.ListResourceWithConfigure = &StoragePoolListResource{}
)

// StoragePoolListResourceModel is the configuration of a `list "everpure_azure_storage_pool"`
// block.
type StoragePoolListResourceModel struct {
	ResourceGroupName types.String     `tfsdk:"resource_group_name"`
	Timeouts          lstimeouts.Value `tfsdk:"timeouts"`
}

type StoragePoolListResource struct {
	// clientsHolder supplies Configure and storagePoolsClient via promotion; its
	// Configure already has the signature list.ListResourceWithConfigure asks for.
	clientsHolder
}

func NewStoragePoolListResource() list.ListResource {
	return &StoragePoolListResource{}
}

func (l *StoragePoolListResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_storage_pool"
}

func (l *StoragePoolListResource) ListResourceConfigSchema(ctx context.Context, _ list.ListResourceSchemaRequest, resp *list.ListResourceSchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists Storage Pools in a resource group, or across the provider's whole " +
			"`subscription_id` when `resource_group_name` is omitted, so they can be brought under Terraform " +
			"with `terraform query`.\n\n" +
			"List resources require Terraform 1.14 or later, and importing the results by identity " +
			"requires 1.12 or later. Results are sorted by `id`.\n\n" +
			"`terraform query -generate-config-out=<file>` writes an `import` block and a `resource` " +
			"block for each result. `include_resource = true` additionally shows each result's " +
			"attributes in the query output.\n\n" +
			"A `list` block returns at most 100 results by default and gives no warning when it stops there. " +
			"Set the block's `limit` argument to raise the cap.",
		Attributes: map[string]schema.Attribute{
			"resource_group_name": schema.StringAttribute{
				Optional:            true,
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
				MarkdownDescription: "Resource group to list pools in. Omitted to list every pool in the provider's own `subscription_id`.",
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": listTimeoutsBlock(ctx, "the pools", defaultStoragePoolReadTimeout),
		},
	}
}

func (l *StoragePoolListResource) List(ctx context.Context, req list.ListRequest, stream *list.ListResultsStream) {
	var diags diag.Diagnostics
	client, ok := l.storagePoolsClient(&diags)
	if !ok {
		stream.Results = list.ListResultsStreamDiagnostics(diags)
		return
	}

	var config StoragePoolListResourceModel
	diags.Append(req.Config.Get(ctx, &config)...)
	if diags.HasError() {
		stream.Results = list.ListResultsStreamDiagnostics(diags)
		return
	}
	listTimeout, timeoutDiags := config.Timeouts.List(ctx, defaultStoragePoolReadTimeout)
	diags.Append(timeoutDiags...)
	blank, blankDiags := blankResourceModel[StoragePoolResourceModel](ctx, req)
	diags.Append(blankDiags...)
	if diags.HasError() {
		stream.Results = list.ListResultsStreamDiagnostics(diags)
		return
	}

	apiCtx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()

	op := operation{action: actionList, kind: kindStoragePools, in: subscriptionOrResourceGroupScope(l.clients.subscriptionID, config.ResourceGroupName.ValueString()), unnamed: true}
	var pools []*armpurestorageblock.StoragePool
	var err error
	if resourceGroupName := config.ResourceGroupName.ValueString(); resourceGroupName != "" {
		pools, err = listStoragePoolsByResourceGroup(apiCtx, client, resourceGroupName)
	} else {
		pools, err = listStoragePoolsBySubscription(apiCtx, client)
	}
	if err != nil {
		diags.Append(l.clients.apiCallError(op, err))
		stream.Results = list.ListResultsStreamDiagnostics(diags)
		return
	}

	sortByAPIID(pools, func(p *armpurestorageblock.StoragePool) *string {
		if p == nil {
			return nil
		}
		return p.ID
	})

	stream.Results = listResultsStream(req.Limit, pools, func(pool *armpurestorageblock.StoragePool) list.ListResult {
		if pool == nil {
			return errorListResult(unexpectedResponseDiagnostic(newEmptyListEntryError(kindStoragePool)))
		}
		model, parsed, diags := importedStoragePoolFromAPI(ctx, blank, *pool)
		if diags.HasError() {
			return list.ListResult{Diagnostics: diags}
		}

		result := req.NewListResult(ctx)
		result.DisplayName = parsed.StoragePoolName
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
