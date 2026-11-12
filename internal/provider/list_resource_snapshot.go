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
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

var (
	_ list.ListResource              = &SnapshotListResource{}
	_ list.ListResourceWithConfigure = &SnapshotListResource{}
)

// SnapshotListResourceModel is the configuration of a `list "everpure_azure_snapshot"` block.
type SnapshotListResourceModel struct {
	VolumeGroupID armids.IDValue   `tfsdk:"volume_group_id"`
	Timeouts      lstimeouts.Value `tfsdk:"timeouts"`
}

type SnapshotListResource struct {
	// clientsHolder supplies Configure and volumeGroupSnapshotsClient via promotion; its
	// Configure already has the signature list.ListResourceWithConfigure asks for.
	clientsHolder
}

func NewSnapshotListResource() list.ListResource {
	return &SnapshotListResource{}
}

func (l *SnapshotListResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_snapshot"
}

func (l *SnapshotListResource) ListResourceConfigSchema(ctx context.Context, _ list.ListResourceSchemaRequest, resp *list.ListResourceSchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the Snapshots in a Volume Group, so they can be brought under Terraform " +
			"with `terraform query`. Only live, manually created snapshots are listed: the ones the " +
			"`everpure_azure_snapshot` resource can manage.\n\n" +
			"List resources require Terraform 1.14 or later, and importing the results by identity " +
			"requires 1.12 or later. Results are sorted newest first, by `created_at`.\n\n" +
			"`terraform query -generate-config-out=<file>` writes an `import` block and a `resource` " +
			"block for each result. `include_resource = true` additionally shows each result's " +
			"attributes in the query output.\n\n" +
			"A `list` block returns at most 100 results by default and gives no warning when it stops there. " +
			"Set the block's `limit` argument to raise the cap.\n\n" +
			"`volume_snapshots` is null in list results. To see it, import the snapshot or read it with " +
			"the [`everpure_azure_snapshot` data source](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/data-sources/azure_snapshot).",
		Attributes: map[string]schema.Attribute{
			"volume_group_id": schema.StringAttribute{
				Required:   true,
				CustomType: armids.IDType{},
				Validators: []validator.String{
					armids.TypeVolumeGroup.Validator(),
				},
				MarkdownDescription: "Full Azure Resource Manager ID of the volume group to list snapshots in. Must be in the provider's own `subscription_id`.",
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": listTimeoutsBlock(ctx, "the snapshots", defaultSnapshotReadTimeout),
		},
	}
}

func (l *SnapshotListResource) List(ctx context.Context, req list.ListRequest, stream *list.ListResultsStream) {
	var diags diag.Diagnostics
	client, ok := l.volumeGroupSnapshotsClient(&diags)
	if !ok {
		stream.Results = list.ListResultsStreamDiagnostics(diags)
		return
	}

	var config SnapshotListResourceModel
	diags.Append(req.Config.Get(ctx, &config)...)
	if diags.HasError() {
		stream.Results = list.ListResultsStreamDiagnostics(diags)
		return
	}
	listTimeout, timeoutDiags := config.Timeouts.List(ctx, defaultSnapshotReadTimeout)
	diags.Append(timeoutDiags...)
	blank, blankDiags := blankResourceModel[SnapshotResourceModel](ctx, req)
	diags.Append(blankDiags...)
	if diags.HasError() {
		stream.Results = list.ListResultsStreamDiagnostics(diags)
		return
	}

	op := operation{action: actionList, kind: kindSnapshots, in: config.VolumeGroupID.ValueString(), unnamed: true}
	vg, idErr := parseIDInSubscription(armids.ParseVolumeGroupID, config.VolumeGroupID.ValueString(), kindVolumeGroup, l.clients.subscriptionID)
	if idErr != nil {
		diags.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(*idErr)))
		stream.Results = list.ListResultsStreamDiagnostics(diags)
		return
	}

	apiCtx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()

	// Only manually created live snapshots, and only as many pages as the first Limit
	// results sit on, so a volume group with thousands of snapshots isn't read in full.
	entries, err := listSnapshots(apiCtx, client, vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName, manualSnapshotsFilter, req.Limit)
	if err != nil {
		if isNotFound(err) {
			diags.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(newNotFoundError(kindVolumeGroup, vg.FullID(), nil, l.clients.apiFailure(err)))))
		} else {
			diags.Append(l.clients.apiCallError(op, err))
		}
		stream.Results = list.ListResultsStreamDiagnostics(diags)
		return
	}

	stream.Results = listResultsStream(req.Limit, entries, func(s *armpurestorageblock.VolumeGroupSnapshot) list.ListResult {
		if s == nil {
			return errorListResult(unexpectedResponseDiagnostic(newEmptyListEntryError(kindSnapshot)))
		}
		// The list element is always mapped, so a malformed one fails whether or not the
		// resource object was requested.
		model, parsed, diags := importedSnapshotFromAPI(ctx, blank, *s)
		if diags.HasError() {
			return list.ListResult{Diagnostics: diags}
		}
		// The list response never includes member volumes, so null, not the empty list the
		// mapping gives an omitted field: an empty list would claim the snapshot has none.
		// The attribute is read-only, so it never appears in generated config, and the
		// import's own Read fills it.
		model.VolumeSnapshots = types.ListNull(volumeSnapshotObjectType)

		result := req.NewListResult(ctx)
		result.DisplayName = model.Name.ValueString()
		result.Diagnostics.Append(result.Identity.Set(ctx, parsed)...)
		if req.IncludeResource {
			result.Diagnostics.Append(result.Resource.Set(ctx, &model)...)
		}
		return result
	})
}
