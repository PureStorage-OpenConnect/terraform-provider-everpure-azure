// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"regexp"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	dstimeouts "github.com/hashicorp/terraform-plugin-framework-timeouts/datasource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

var (
	_ datasource.DataSource              = &SnapshotDataSource{}
	_ datasource.DataSourceWithConfigure = &SnapshotDataSource{}
)

// snapshotLookupNamePattern is the name grammar of a snapshot in a resource path: 1-64
// characters, letters, digits and hyphens, starting and ending with a letter or digit.
// It is wider than the pattern the resource enforces on create (storageObjectNamePattern,
// which requires a leading letter), because the snapshot schedule names its snapshots
// with digits alone, and a data source has to be able to look those up.
var snapshotLookupNamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,62}[A-Za-z0-9])?$`)

// SnapshotDataSourceModel is the Terraform-side representation of the
// everpure_azure_snapshot data source. snapshotDataSourceItem carries the attributes the
// plural everpure_azure_snapshots data source's elements share; volume_snapshots is
// singular-only, since the list API does not return member volumes.
type SnapshotDataSourceModel struct {
	snapshotDataSourceItem
	VolumeSnapshots types.List       `tfsdk:"volume_snapshots"`
	Timeouts        dstimeouts.Value `tfsdk:"timeouts"`
}

type SnapshotDataSource struct {
	// dataSourceClientsHolder supplies Configure and volumeGroupSnapshotsClient via
	// promotion; see its own doc comment.
	dataSourceClientsHolder
}

func NewSnapshotDataSource() datasource.DataSource {
	return &SnapshotDataSource{}
}

func (d *SnapshotDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_snapshot"
}

// snapshotComputedAttributes is the Computed attribute map shared by the singular
// everpure_azure_snapshot data source's own schema and the plural everpure_azure_snapshots
// data source's list element schema. It never includes the lookup inputs (name,
// volume_group_id), volume_snapshots, or "timeouts": each caller adds its own.
func snapshotComputedAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"id": schema.StringAttribute{
			Computed:            true,
			CustomType:          armids.IDType{},
			MarkdownDescription: "The full Azure Resource Manager ID of the snapshot.",
		},
		"created_at": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "When the snapshot was created, as an RFC 3339 timestamp in UTC. " +
				"Timestamps sort chronologically as strings.",
		},
		"created_by_policy": schema.BoolAttribute{
			Computed: true,
			MarkdownDescription: "Whether the snapshot was taken by the volume group's snapshot schedule, " +
				"rather than manually.",
		},
		"space": spaceAttribute(),
	}
}

func (d *SnapshotDataSource) Schema(ctx context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := snapshotComputedAttributes()
	attrs["name"] = schema.StringAttribute{
		Required:   true,
		Validators: []validator.String{stringvalidator.RegexMatches(snapshotLookupNamePattern, snapshotLookupNameRule)},
		MarkdownDescription: "Name of the snapshot. A snapshot taken by the volume group's snapshot schedule " +
			"has a name made of digits.",
	}
	attrs["volume_group_id"] = schema.StringAttribute{
		Required:   true,
		CustomType: armids.IDType{},
		Validators: []validator.String{
			armids.TypeVolumeGroup.Validator(),
		},
		MarkdownDescription: "Full Azure Resource Manager ID of the volume group the snapshot belongs to. Must be in the provider's own `subscription_id`.",
	}
	attrs["volume_snapshots"] = schema.ListNestedAttribute{
		Computed:            true,
		MarkdownDescription: volumeSnapshotsDescription,
		NestedObject: schema.NestedAttributeObject{
			Attributes: map[string]schema.Attribute{
				"volume_name": schema.StringAttribute{
					Computed:            true,
					MarkdownDescription: volumeSnapshotVolumeNameDescription,
				},
				"provisioned_size_in_bytes": schema.Int64Attribute{
					Computed:            true,
					MarkdownDescription: volumeSnapshotSizeDescription,
				},
				"serial_number": schema.StringAttribute{
					Computed:            true,
					MarkdownDescription: volumeSnapshotSerialDescription,
				},
				"space": spaceAttribute(),
			},
		},
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up an existing Volume Group Snapshot by name and its Volume Group. This " +
			"includes snapshots taken by the volume group's snapshot schedule.\n\n" +
			"To create and manage a manual snapshot, use the " +
			"[`everpure_azure_snapshot` resource](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/resources/snapshot).\n\n" +
			"Soft-deleted snapshots can't be looked up with this data source.",
		Attributes: attrs,
		Blocks: map[string]schema.Block{
			"timeouts": dataSourceTimeoutsBlock(ctx, "the snapshot", defaultSnapshotReadTimeout),
		},
	}
}

func (d *SnapshotDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	client, ok := d.volumeGroupSnapshotsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data SnapshotDataSourceModel
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

	op := operation{action: actionRetrieve, kind: kindSnapshot, name: data.Name.ValueString()}
	vg, idErr := parseIDInSubscription(armids.ParseVolumeGroupID, data.VolumeGroupID.ValueString(), kindVolumeGroup, d.clients.subscriptionID)
	if idErr != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(*idErr)))
		return
	}

	name := data.Name.ValueString()
	op.id = vg.Snapshot(name).FullID()

	result, err := client.Get(ctx, vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName, name, nil)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(op.fail(newNotFoundError(kindSnapshot, op.id, &kindVolumeGroup, d.clients.apiFailure(err))))
			return
		}
		resp.Diagnostics.Append(d.clients.apiCallError(op, err))
		return
	}

	// Soft delete renames a snapshot to "{name}-{YYYYMMDDHHmmss}" and leaves it in the
	// same volume group, so a GET on that timestamped name succeeds. Like a name that
	// doesn't exist, it fails the lookup, with its own message pointing at recovery.
	if isSnapshotSoftDeleted(result.VolumeGroupSnapshot) {
		resp.Diagnostics.Append(op.fail(softDeletedSnapshotCause(vg, name, result.ID, snapshotCreatedByPolicy(result.VolumeGroupSnapshot), result.Properties.SoftDeletion)))
		return
	}

	item, diags := snapshotItemFrom(result.VolumeGroupSnapshot)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	volumeSnapshots, diags := volumeSnapshotsDataSourceFromAPI(ctx, result.Properties.VolumeSnapshots)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// name and volume_group_id are kept as configured: the response may echo a different
	// casing, and a full ARM ID already compares case-insensitively via armids.IDValue.
	item.Name = data.Name
	item.VolumeGroupID = data.VolumeGroupID
	data.snapshotDataSourceItem = item
	data.VolumeSnapshots = volumeSnapshots

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// softDeletedSnapshotCause picks the cause for a lookup that landed on a soft-deleted
// snapshot's own timestamped name. name is what the lookup was configured with; id is
// that same snapshot's full ARM ID, when the response carried one — the value
// source_recoverable_snapshot_id needs to recover it. Without one, the ID is composed
// from vg and name instead, since source_recoverable_snapshot_id only accepts a full ARM
// ID, never a bare name. A snapshot taken by the volume group's snapshot schedule cannot
// be recovered through Terraform, so its cause says so instead of offering the recovery
// configuration.
func softDeletedSnapshotCause(vg armids.VolumeGroupID, name string, id *string, createdByPolicy bool, softDeletion *armpurestorageblock.DestroyedStateProperties) detailError {
	previousName := name
	if softDeletion.PreviousName != nil {
		previousName = *softDeletion.PreviousName
	}
	recoverableID := vg.Snapshot(name).FullID()
	if id != nil {
		recoverableID = *id
	}
	eradicationTime := ""
	if softDeletion.EradicationTimestamp != nil {
		eradicationTime = softDeletion.EradicationTimestamp.Format(time.RFC3339)
	}
	return newSnapshotSoftDeletedError(name, previousName, recoverableID, eradicationTime, createdByPolicy)
}
