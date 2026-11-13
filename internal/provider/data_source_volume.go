// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	dstimeouts "github.com/hashicorp/terraform-plugin-framework-timeouts/datasource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

var (
	_ datasource.DataSource              = &VolumeDataSource{}
	_ datasource.DataSourceWithConfigure = &VolumeDataSource{}
)

// VolumeDataSourceModel is the Terraform-side representation of the everpure_azure_volume
// data source. volumeDataSourceItem carries every read-side attribute; it is also
// embedded in the plural everpure_azure_volumes data source's own list element type
// (volumeListItem), alongside soft_deletion, so the two schemas' shared attributes stay
// identical by construction rather than by two hand-kept copies agreeing. There is no
// provisioned_size_in_gib here (see the data source's own schema comment): a volume
// created outside Terraform can be any multiple of 512 bytes, so a computed GiB value
// would have to round or be null either way.
type VolumeDataSourceModel struct {
	volumeDataSourceItem
	Timeouts dstimeouts.Value `tfsdk:"timeouts"`
}

type VolumeDataSource struct {
	// dataSourceClientsHolder supplies Configure and volumesClient via promotion; see
	// its own doc comment.
	dataSourceClientsHolder
}

func NewVolumeDataSource() datasource.DataSource {
	return &VolumeDataSource{}
}

func (d *VolumeDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_volume"
}

// volumeComputedAttributes is the Computed attribute map shared by the singular
// everpure_azure_volume data source's own schema and the plural everpure_azure_volumes
// data source's list element schema. It never includes the lookup inputs (name,
// volume_group_id) or "timeouts": each caller adds its own.
func volumeComputedAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"id": schema.StringAttribute{
			Computed:            true,
			CustomType:          armids.IDType{},
			MarkdownDescription: "The full Azure Resource Manager ID of the volume.",
		},
		"provisioned_size_in_bytes": schema.Int64Attribute{
			Computed:            true,
			MarkdownDescription: "Provisioned size of the volume, in bytes.",
		},
		"serial_number": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "The volume's own serial number. Visible in the VM as the disk/drive serial number.",
		},
		"space": spaceAttribute(),
	}
}

func (d *VolumeDataSource) Schema(ctx context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := volumeComputedAttributes()
	attrs["name"] = schema.StringAttribute{
		Required:            true,
		Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
		MarkdownDescription: "Name of the volume.",
	}
	attrs["volume_group_id"] = schema.StringAttribute{
		Required:   true,
		CustomType: armids.IDType{},
		Validators: []validator.String{
			armids.TypeVolumeGroup.Validator(),
		},
		MarkdownDescription: "Full Azure Resource Manager ID of the volume group this volume lives on. Must be in the provider's own `subscription_id`.",
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up an existing Volume by name and its Volume Group.\n\n" +
			"To create and manage a volume, use the " +
			"[`everpure_azure_volume` resource](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/resources/volume).\n\n" +
			"Soft-deleted volumes can't be looked up with this data source.",
		Attributes: attrs,
		Blocks: map[string]schema.Block{
			"timeouts": dataSourceTimeoutsBlock(ctx, "the volume", defaultVolumeReadTimeout),
		},
	}
}

func (d *VolumeDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	client, ok := d.volumesClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data VolumeDataSourceModel
	if resp.Diagnostics.Append(req.Config.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	readTimeout, diags := data.Timeouts.Read(ctx, defaultVolumeReadTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	op := operation{action: actionRetrieve, kind: kindVolume, name: data.Name.ValueString()}
	vg, idErr := parseIDInSubscription(armids.ParseVolumeGroupID, data.VolumeGroupID.ValueString(), kindVolumeGroup, d.clients.subscriptionID)
	if idErr != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(*idErr)))
		return
	}

	name := data.Name.ValueString()
	op.id = vg.Volume(name).FullID()

	result, err := client.Get(ctx, vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName, name, nil)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(op.fail(newNotFoundError(kindVolume, op.id, &kindVolumeGroup, d.clients.apiFailure(err))))
			return
		}
		resp.Diagnostics.Append(d.clients.apiCallError(op, err))
		return
	}

	// A volume data source, unlike the volume group data source, can reach a
	// soft-deleted copy: soft delete renames a volume to "{name}-{YYYYMMDDHHmmss}" and
	// leaves it in the same volume group, so GET on that timestamped name succeeds. The
	// resource never hits this, since it only ever GETs the original name, which 404s
	// once the volume is gone. This is treated the same as "not found": an error, not a
	// null/empty result, with its own message pointing at recovery.
	if isVolumeSoftDeleted(result.Volume) {
		resp.Diagnostics.Append(op.fail(softDeletedVolumeCause(vg, name, result.ID, result.Properties.SoftDeletion)))
		return
	}

	item, diags := volumeItemFrom(result.Volume)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// name and volume_group_id are never taken from the API: applyVolume doesn't map
	// name at all (see its own doc comment), and volume_group_id has no API
	// representation, so data already holds the right values for both from config.
	item.Name = data.Name
	item.VolumeGroupID = data.VolumeGroupID
	data.volumeDataSourceItem = item

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// isVolumeSoftDeleted reports whether v is itself a soft-deleted (destroyed, not yet
// eradicated) copy, as opposed to a live volume. A nil Properties or SoftDeletion, or a
// SoftDeletion whose Destroyed flag is nil or false, all mean "live" — GET only ever
// returns a soft-deleted entry at all when the configured name happens to already be
// one's own timestamped name.
func isVolumeSoftDeleted(v armpurestorageblock.Volume) bool {
	return v.Properties != nil && isDestroyed(v.Properties.SoftDeletion)
}

// softDeletedVolumeCause picks the cause for a lookup that landed on a soft-deleted volume's
// own timestamped name. name is what the lookup was configured with (the timestamped
// name itself); id is that same volume's full ARM ID, when the response carried one — the
// value source_recoverable_volume_id needs to recover it. Without one, the ID is
// composed from vg and name instead, since source_recoverable_volume_id only accepts a
// full ARM ID, never a bare name.
func softDeletedVolumeCause(vg armids.VolumeGroupID, name string, id *string, softDeletion *armpurestorageblock.DestroyedStateProperties) detailError {
	previousName := name
	if softDeletion.PreviousName != nil {
		previousName = *softDeletion.PreviousName
	}
	recoverableID := vg.Volume(name).FullID()
	if id != nil {
		recoverableID = *id
	}
	eradicationTime := ""
	if softDeletion.EradicationTimestamp != nil {
		eradicationTime = softDeletion.EradicationTimestamp.Format(time.RFC3339)
	}
	return newVolumeSoftDeletedError(name, previousName, recoverableID, eradicationTime)
}
