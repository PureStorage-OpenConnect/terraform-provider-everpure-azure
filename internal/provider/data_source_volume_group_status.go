// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	dstimeouts "github.com/hashicorp/terraform-plugin-framework-timeouts/datasource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

var (
	_ datasource.DataSource              = &VolumeGroupStatusDataSource{}
	_ datasource.DataSourceWithConfigure = &VolumeGroupStatusDataSource{}
)

// VolumeGroupStatusDataSourceModel is the Terraform-side representation of the
// everpure_azure_volume_group_status data source.
type VolumeGroupStatusDataSourceModel struct {
	VolumeGroupID              armids.IDValue   `tfsdk:"volume_group_id"`
	MaxConnectedHostsPerVolume types.Int64      `tfsdk:"max_connected_hosts_per_volume"`
	Space                      types.Object     `tfsdk:"space"`
	Timeouts                   dstimeouts.Value `tfsdk:"timeouts"`
}

// VolumeGroupStatusDataSource reads a volume group's space usage and connected host
// count. It is meant for Terraform `check` blocks.
type VolumeGroupStatusDataSource struct {
	// dataSourceClientsHolder supplies Configure and volumeGroupsClient via promotion;
	// see its own doc comment.
	dataSourceClientsHolder
}

func NewVolumeGroupStatusDataSource() datasource.DataSource {
	return &VolumeGroupStatusDataSource{}
}

func (d *VolumeGroupStatusDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_volume_group_status"
}

func (d *VolumeGroupStatusDataSource) Schema(ctx context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads the status of an existing Volume Group: its space usage and how many hosts its " +
			"volumes are connected to.\n\n" +
			"The values can be used in any condition Terraform evaluates. In a " +
			"[`check` block](https://developer.hashicorp.com/terraform/language/checks), a failed assertion is " +
			"reported as a warning during plan and apply, and the run continues. In a `precondition` or " +
			"`postcondition`, a failed condition stops the run.\n\n" +
			"`max_connected_hosts_per_volume` is the largest number of hosts any single volume in the group is " +
			"connected to, not the number of distinct hosts: two volumes each connected to a different host read " +
			"`1`. It counts configured connections, not active iSCSI sessions, so it does not change when a guest " +
			"logs out. Space values are in bytes.\n\n" +
			"If the volume group cannot be read, the read fails with the service's error. Inside a `check` block " +
			"that error is reported as a warning.",
		Attributes: map[string]schema.Attribute{
			"volume_group_id": schema.StringAttribute{
				Required:   true,
				CustomType: armids.IDType{},
				Validators: []validator.String{
					armids.TypeVolumeGroup.Validator(),
				},
				MarkdownDescription: "Full Azure Resource Manager ID of the Volume Group. Must be in the " +
					"provider's own `subscription_id`.",
			},
			"max_connected_hosts_per_volume": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "The largest number of hosts any single volume in the group is connected to. It " +
					"counts configured connections, not active iSCSI sessions, so it does not change when a guest " +
					"logs out. 0 for a group with no volumes.",
			},
			"space": spaceAttribute(),
		},
		Blocks: map[string]schema.Block{
			"timeouts": dataSourceTimeoutsBlock(ctx, "the volume group's status", defaultVolumeGroupReadTimeout),
		},
	}
}

func (d *VolumeGroupStatusDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	client, ok := d.volumeGroupsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data VolumeGroupStatusDataSourceModel
	if resp.Diagnostics.Append(req.Config.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	readTimeout, diags := data.Timeouts.Read(ctx, defaultVolumeGroupReadTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	vgID := data.VolumeGroupID.ValueString()
	op := operation{action: actionRetrieve, kind: kindVolumeGroupStatus, id: vgID, unnamed: true}

	vg, idErr := parseIDInSubscription(armids.ParseVolumeGroupID, vgID, kindVolumeGroup, d.clients.subscriptionID)
	if idErr != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(*idErr)))
		return
	}

	status, err := client.GetStatus(ctx, vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName, nil)
	if err != nil {
		if isNotFound(err) {
			if isStoragePoolMissing(err) {
				resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(newStoragePoolUnavailableError(vg.StoragePool().FullID(), vgID, d.clients.apiFailure(err)))))
				return
			}
			resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(newNotFoundError(kindVolumeGroup, vgID, nil, d.clients.apiFailure(err)))))
			return
		}
		resp.Diagnostics.Append(d.clients.apiCallError(op, err))
		return
	}

	resp.Diagnostics.Append(applyVolumeGroupStatus(&data, status.VolumeGroupStatus)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// volume_group_id is left untouched from config: Terraform core rejects a data
	// source result that changes a configured, non-Computed value.
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// applyVolumeGroupStatus maps the API response onto every computed field of data. A nil
// host count maps to null and a nil Space to a null object.
func applyVolumeGroupStatus(data *VolumeGroupStatusDataSourceModel, status armpurestorageblock.VolumeGroupStatus) diag.Diagnostics {
	var diags diag.Diagnostics

	if status.ConnectedHostCount != nil {
		data.MaxConnectedHostsPerVolume = types.Int64Value(int64(*status.ConnectedHostCount))
	} else {
		data.MaxConnectedHostsPerVolume = types.Int64Null()
	}

	data.Space, diags = spaceFromAPI(status.Space)
	return diags
}
