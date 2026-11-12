// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"

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
	_ datasource.DataSource              = &VolumeGroupDataSource{}
	_ datasource.DataSourceWithConfigure = &VolumeGroupDataSource{}
)

// VolumeGroupDataSourceModel is the Terraform-side representation of the
// everpure_azure_volume_group data source. volumeGroupDataSourceItem carries every
// read-side attribute; it is also the plural everpure_azure_volume_groups data
// source's own list element type, so the two schemas' attributes stay identical by
// construction rather than by two hand-kept copies agreeing.
type VolumeGroupDataSourceModel struct {
	volumeGroupDataSourceItem
	Timeouts dstimeouts.Value `tfsdk:"timeouts"`
}

type VolumeGroupDataSource struct {
	// dataSourceClientsHolder supplies Configure and volumeGroupsClient via promotion;
	// see its own doc comment.
	dataSourceClientsHolder
}

func NewVolumeGroupDataSource() datasource.DataSource {
	return &VolumeGroupDataSource{}
}

func (d *VolumeGroupDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_volume_group"
}

// volumeGroupComputedAttributes is the Computed attribute map shared by the singular
// everpure_azure_volume_group data source's own schema and the plural
// everpure_azure_volume_groups data source's list element schema. It never includes the
// lookup inputs (name, storage_pool_id) or "timeouts": each caller adds its own.
func volumeGroupComputedAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"id": schema.StringAttribute{
			Computed:            true,
			CustomType:          armids.IDType{},
			MarkdownDescription: "The full Azure Resource Manager ID of the volume group.",
		},
		"bandwidth_limit_in_mbps": schema.Int64Attribute{
			Computed:            true,
			MarkdownDescription: "Bandwidth limit for the volume group, in megabytes per second (MB/s). Null when unlimited.",
		},
		"iops_limit": schema.Int64Attribute{
			Computed:            true,
			MarkdownDescription: "I/O operations per second (IOPS) limit for the volume group. Null when unlimited.",
		},
		"tags": schema.MapAttribute{
			Computed:            true,
			ElementType:         types.StringType,
			MarkdownDescription: "A mapping of tags assigned to the volume group.",
		},
		"internal_id": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Everpure's internal ID of the volume group.",
		},
		"snapshot_protection_policy": schema.ListNestedAttribute{
			Computed:            true,
			MarkdownDescription: "The volume group's snapshot schedule. Null when no schedule is set.",
			NestedObject: schema.NestedAttributeObject{
				Attributes: map[string]schema.Attribute{
					"frequency": schema.StringAttribute{
						Computed:            true,
						CustomType:          durationType{},
						MarkdownDescription: "How often a snapshot is taken, as an ISO 8601 duration using days or smaller units.",
					},
					"retention": schema.StringAttribute{
						Computed:            true,
						CustomType:          durationType{},
						MarkdownDescription: "How long snapshots taken by this schedule are kept, as an ISO 8601 duration using days or smaller units.",
					},
				},
			},
		},
	}
}

func (d *VolumeGroupDataSource) Schema(ctx context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := volumeGroupComputedAttributes()
	attrs["name"] = schema.StringAttribute{
		Required:            true,
		Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
		MarkdownDescription: "Name of the volume group.",
	}
	attrs["storage_pool_id"] = schema.StringAttribute{
		Required:   true,
		CustomType: armids.IDType{},
		Validators: []validator.String{
			armids.TypeStoragePool.Validator(),
		},
		MarkdownDescription: "Full Azure Resource Manager ID of the Storage Pool this volume group lives on. Must be in the provider's own `subscription_id`.",
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up an existing Volume Group by name and its Storage Pool.\n\n" +
			"To create and manage a volume group, use the " +
			"[`everpure_azure_volume_group` resource](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/resources/azure_volume_group).",
		Attributes: attrs,
		Blocks: map[string]schema.Block{
			"timeouts": dataSourceTimeoutsBlock(ctx, "the volume group", defaultVolumeGroupReadTimeout),
		},
	}
}

func (d *VolumeGroupDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	client, ok := d.volumeGroupsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data VolumeGroupDataSourceModel
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

	op := operation{action: actionRetrieve, kind: kindVolumeGroup, name: data.Name.ValueString()}
	pool, idErr := parseIDInSubscription(armids.ParseStoragePoolID, data.StoragePoolID.ValueString(), kindStoragePool, d.clients.subscriptionID)
	if idErr != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(*idErr)))
		return
	}

	name := data.Name.ValueString()
	op.id = pool.VolumeGroup(name).FullID()

	result, err := client.Get(ctx, pool.ResourceGroupName, pool.StoragePoolName, name, nil)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(op.fail(newNotFoundError(kindVolumeGroup, op.id, &kindStoragePool, d.clients.apiFailure(err))))
			return
		}
		resp.Diagnostics.Append(d.clients.apiCallError(op, err))
		return
	}

	item, diags := volumeGroupItemFrom(ctx, result.VolumeGroup)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// name and storage_pool_id are never taken from the API: ARM's volume group name
	// lookup folds case, so a config that spells name differently than the group's own
	// stored casing must still see its own spelling reflected back, not the API's —
	// Terraform core rejects a data source result that changes a configured,
	// non-Computed value.
	item.Name = data.Name
	item.StoragePoolID = data.StoragePoolID
	data.volumeGroupDataSourceItem = item

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
