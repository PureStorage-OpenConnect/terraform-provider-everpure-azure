// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"strconv"

	dstimeouts "github.com/hashicorp/terraform-plugin-framework-timeouts/datasource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

// largeSnapshotListWarningThreshold is how many snapshots a read without limit may return
// before it warns: past it, every read pages through the whole volume group (100 snapshots
// per API call) on every plan and refresh, and the result is that large in state too.
const largeSnapshotListWarningThreshold = 1000

var (
	_ datasource.DataSource              = &SnapshotsDataSource{}
	_ datasource.DataSourceWithConfigure = &SnapshotsDataSource{}
)

// snapshotListItem is one element of the everpure_azure_snapshots data source's
// snapshots list: the singular everpure_azure_snapshot data source's attribute set minus
// volume_snapshots, plus soft_deletion — a list can include destroyed (soft-deleted)
// entries that a name+volume_group_id lookup never has to represent, since a
// soft-deleted snapshot's timestamped name is an error there.
type snapshotListItem struct {
	snapshotDataSourceItem
	SoftDeletion types.Object `tfsdk:"soft_deletion"`
}

// SnapshotsDataSourceModel is the Terraform-side representation of the
// everpure_azure_snapshots data source.
type SnapshotsDataSourceModel struct {
	VolumeGroupID    armids.IDValue   `tfsdk:"volume_group_id"`
	IncludeDestroyed types.Bool       `tfsdk:"include_destroyed"`
	Limit            types.Int64      `tfsdk:"limit"`
	Snapshots        types.List       `tfsdk:"snapshots"`
	Timeouts         dstimeouts.Value `tfsdk:"timeouts"`
}

type SnapshotsDataSource struct {
	// dataSourceClientsHolder supplies Configure and volumeGroupSnapshotsClient via
	// promotion; see its own doc comment.
	dataSourceClientsHolder
}

func NewSnapshotsDataSource() datasource.DataSource {
	return &SnapshotsDataSource{}
}

func (d *SnapshotsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_snapshots"
}

// snapshotsElementNestedObject is the "snapshots" list's per-element schema: the
// singular everpure_azure_snapshot data source's computed attributes, plus its own lookup
// inputs (name, volume_group_id) added back as Computed, plus soft_deletion. It has no
// volume_snapshots.
func snapshotsElementNestedObject() schema.NestedAttributeObject {
	attrs := snapshotComputedAttributes()
	attrs["name"] = schema.StringAttribute{
		Computed: true,
		MarkdownDescription: "Name of the snapshot. A soft-deleted snapshot's name is the timestamped name it " +
			"was renamed to on deletion; see `soft_deletion.previous_name` for its original name.",
	}
	attrs["volume_group_id"] = schema.StringAttribute{
		Computed:   true,
		CustomType: armids.IDType{},
		MarkdownDescription: "Full Azure Resource Manager ID of the volume group the snapshot belongs to. " +
			"Same as the data source's own `volume_group_id`.",
	}
	attrs["soft_deletion"] = softDeletionAttribute("snapshot")
	return schema.NestedAttributeObject{Attributes: attrs}
}

func (d *SnapshotsDataSource) Schema(ctx context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the Volume Group Snapshots in a Volume Group, including those taken by the " +
			"volume group's snapshot schedule. Soft-deleted (destroyed, not yet eradicated) snapshots are " +
			"excluded by default; set `include_destroyed = true` to include them.\n\n" +
			"The elements do not include `volume_snapshots`, the snapshot of each member volume: the API does " +
			"not return them when listing. To get them for one snapshot, look it up by name with the " +
			"[`everpure_azure_snapshot` data source](https://registry.terraform.io/providers/" +
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
			"include_destroyed": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Whether to include soft-deleted (destroyed, not yet eradicated) snapshots " +
					"in the result. Defaults to `false`.",
			},
			"limit": schema.Int64Attribute{
				Optional:   true,
				Validators: []validator.Int64{int64validator.AtLeast(1)},
				MarkdownDescription: "The most snapshots to return: the newest ones, so `limit = 1` returns only the " +
					"latest. Unset returns every snapshot in the volume group, which in a volume group with " +
					"many snapshots means many API calls on every read; a read without `limit` that returns " +
					"more than " + strconv.Itoa(largeSnapshotListWarningThreshold) + " snapshots warns.",
			},
			"snapshots": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The snapshots found, sorted newest first, by `created_at`.",
				NestedObject:        snapshotsElementNestedObject(),
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": dataSourceTimeoutsBlock(ctx, "the snapshots", defaultSnapshotReadTimeout),
		},
	}
}

func (d *SnapshotsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	client, ok := d.volumeGroupSnapshotsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data SnapshotsDataSourceModel
	if resp.Diagnostics.Append(req.Config.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	readTimeout, diags := data.Timeouts.Read(ctx, defaultSnapshotReadTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	op := operation{action: actionList, kind: kindSnapshots, in: data.VolumeGroupID.ValueString(), unnamed: true}
	vg, idErr := parseIDInSubscription(armids.ParseVolumeGroupID, data.VolumeGroupID.ValueString(), kindVolumeGroup, d.clients.subscriptionID)
	if idErr != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(*idErr)))
		return
	}

	// A null include_destroyed (the attribute has no schema Default: see
	// datasource/schema.BoolAttribute) means false.
	includeDestroyed := data.IncludeDestroyed.ValueBool()

	filter := liveSnapshotsFilter
	if includeDestroyed {
		filter = ""
	}
	// A null limit means every snapshot; listSnapshots reads a limit of zero the same way.
	snapshots, err := listSnapshots(ctx, client, vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName, filter, data.Limit.ValueInt64())
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(newNotFoundError(kindVolumeGroup, vg.FullID(), nil, d.clients.apiFailure(err)))))
			return
		}
		resp.Diagnostics.Append(d.clients.apiCallError(op, err))
		return
	}

	items := make([]snapshotListItem, 0, len(snapshots))
	for _, s := range snapshots {
		// A nil entry (the SDK's list result type allows it) or one with no ID is dropped;
		// see ensureListElementProperties for why one with no properties is kept.
		if s == nil || s.ID == nil {
			continue
		}
		snap := *s
		ensureListElementProperties(&snap.Properties)
		item, diags := snapshotItemFrom(snap)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		item.VolumeGroupID = data.VolumeGroupID

		softDeletion, diags := softDeletionFromAPI(snap.Properties.SoftDeletion)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}

		items = append(items, snapshotListItem{snapshotDataSourceItem: item, SoftDeletion: softDeletion})
	}

	if data.Limit.IsNull() && len(snapshots) > largeSnapshotListWarningThreshold {
		resp.Diagnostics.Append(diag.WithPath(path.Root("limit"), largeSnapshotListWarning(len(snapshots), vg.FullID())))
	}

	list, listDiags := types.ListValueFrom(ctx, snapshotsElementNestedObject().Type(), items)
	resp.Diagnostics.Append(listDiags...)
	data.Snapshots = list

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
