// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"

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
	_ datasource.DataSource              = &VolumeGroupConnectionParametersDataSource{}
	_ datasource.DataSourceWithConfigure = &VolumeGroupConnectionParametersDataSource{}
)

// VolumeGroupConnectionParametersDataSourceModel is the Terraform-side representation
// of the everpure_azure_volume_group_connection_parameters data source.
type VolumeGroupConnectionParametersDataSourceModel struct {
	VolumeGroupID  armids.IDValue   `tfsdk:"volume_group_id"`
	ISCSIEndpoints types.List       `tfsdk:"iscsi_endpoints"`
	Timeouts       dstimeouts.Value `tfsdk:"timeouts"`
}

// VolumeGroupConnectionParametersDataSource looks up a Volume Group's iSCSI connection
// parameters: portal IPs, ports and its target IQN. The response carries only those
// coordinates and no credential of any kind, which is why this is a data source whose
// result may be persisted in state and interpolated elsewhere, rather than an ephemeral
// resource; a future API response that adds a credential (for example a CHAP secret)
// would need to be served through a separate ephemeral resource instead, never through
// this one.
type VolumeGroupConnectionParametersDataSource struct {
	// dataSourceClientsHolder supplies Configure and volumeGroupsClient via promotion;
	// see its own doc comment.
	dataSourceClientsHolder
}

func NewVolumeGroupConnectionParametersDataSource() datasource.DataSource {
	return &VolumeGroupConnectionParametersDataSource{}
}

func (d *VolumeGroupConnectionParametersDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_volume_group_connection_parameters"
}

func (d *VolumeGroupConnectionParametersDataSource) Schema(ctx context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up the iSCSI connection parameters (portal IPs, ports and target IQN) of an " +
			"existing Volume Group.\n\n" +
			"To connect an Azure virtual machine to one or more Volume Groups automatically, instead of reading " +
			"these parameters and configuring the connection yourself, use the " +
			"[`everpure_azure_virtual_machine_connection` resource](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/resources/azure_virtual_machine_connection).",
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
			"iscsi_endpoints": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The Volume Group's iSCSI endpoints, sorted by portal IP and port. Empty " +
					"when the Volume Group currently has none.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"ip": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Portal IP address of the endpoint.",
						},
						"iqn": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "IQN of the endpoint. Every endpoint of one Volume Group shares " +
								"the same IQN.",
						},
						"port": schema.Int64Attribute{
							Computed:            true,
							MarkdownDescription: "Portal port of the endpoint.",
						},
					},
				},
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": dataSourceTimeoutsBlock(ctx, "the volume group's connection parameters", defaultVolumeGroupReadTimeout),
		},
	}
}

func (d *VolumeGroupConnectionParametersDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	client, ok := d.volumeGroupsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data VolumeGroupConnectionParametersDataSourceModel
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
	op := operation{action: actionRetrieve, kind: kindVolumeGroupConnectionParameters, id: vgID, unnamed: true}

	vg, idErr := parseIDInSubscription(armids.ParseVolumeGroupID, vgID, kindVolumeGroup, d.clients.subscriptionID)
	if idErr != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(*idErr)))
		return
	}

	targets, err := listVolumeGroupConnectionEndpoints(ctx, client, vg)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(newNotFoundError(kindVolumeGroup, vgID, nil, d.clients.apiFailure(err)))))
			return
		}
		resp.Diagnostics.Append(d.clients.apiCallError(op, err))
		return
	}

	endpoints, endpointDiags := connectionParametersEndpointsAndWarning(ctx, vgID, targets)
	resp.Diagnostics.Append(endpointDiags...)
	data.ISCSIEndpoints = endpoints

	// volume_group_id is left untouched from config: Terraform core rejects a data
	// source result that changes a configured, non-Computed value.
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// connectionParametersEndpointsAndWarning sorts targets and builds the "iscsi_endpoints"
// list, adding a warning when there are none. A data source reports what currently
// exists rather than treating "no endpoints yet" as an error the way the VM connection
// resource does with the same underlying call.
func connectionParametersEndpointsAndWarning(ctx context.Context, vgID string, targets []iscsiTarget) (types.List, diag.Diagnostics) {
	var diags diag.Diagnostics

	sortISCSITargets(targets)
	list, d := resolvedTargetsToList(ctx, targets)
	diags.Append(d...)

	if len(targets) == 0 {
		diags.Append(noConnectionEndpointsWarning(vgID))
	}

	return list, diags
}
