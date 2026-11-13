// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"sort"

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
	_ datasource.DataSource              = &RecoverableVolumeGroupsDataSource{}
	_ datasource.DataSourceWithConfigure = &RecoverableVolumeGroupsDataSource{}
)

// RecoverableVolumeGroupsDataSourceModel is the Terraform-side representation of the
// everpure_azure_recoverable_volume_groups data source.
type RecoverableVolumeGroupsDataSourceModel struct {
	StoragePoolID           armids.IDValue   `tfsdk:"storage_pool_id"`
	RecoverableVolumeGroups types.List       `tfsdk:"recoverable_volume_groups"`
	Timeouts                dstimeouts.Value `tfsdk:"timeouts"`
}

type RecoverableVolumeGroupsDataSource struct {
	// dataSourceClientsHolder supplies Configure and recoverableVolumeGroupsClient via
	// promotion; see its own doc comment.
	dataSourceClientsHolder
}

func NewRecoverableVolumeGroupsDataSource() datasource.DataSource {
	return &RecoverableVolumeGroupsDataSource{}
}

func (d *RecoverableVolumeGroupsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_recoverable_volume_groups"
}

// recoverableVolumeGroupsElementNestedObject is the "recoverable_volume_groups" list's
// per-element schema, written by hand (unlike the other three plural data sources)
// since a recoverable volume group has no singular data source or resource of its own
// to share an attribute map with — the schema parity test pins it to the volume group
// element instead.
func recoverableVolumeGroupsElementNestedObject() schema.NestedAttributeObject {
	return schema.NestedAttributeObject{
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				CustomType:          armids.IDType{},
				MarkdownDescription: "The full Azure Resource Manager ID of the recoverable volume group.",
			},
			"name": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The timestamped name this recoverable volume group was renamed to on soft delete.",
			},
			"storage_pool_id": schema.StringAttribute{
				Computed:            true,
				CustomType:          armids.IDType{},
				MarkdownDescription: "Full Azure Resource Manager ID of the Storage Pool this recoverable volume group lives on.",
			},
			"bandwidth_limit_in_mbps": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Bandwidth limit the volume group had, in megabytes per second (MB/s). Null when unlimited.",
			},
			"iops_limit": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "I/O operations per second (IOPS) limit the volume group had. Null when unlimited.",
			},
			"snapshot_protection_policy": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The volume group's snapshot schedule, at the time it was destroyed. Null when no schedule was set.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"frequency": schema.StringAttribute{
							Computed:            true,
							CustomType:          durationType{},
							MarkdownDescription: "How often a snapshot was taken, as an ISO 8601 duration using days or smaller units.",
						},
						"retention": schema.StringAttribute{
							Computed:            true,
							CustomType:          durationType{},
							MarkdownDescription: "How long snapshots taken by this schedule were kept, as an ISO 8601 duration using days or smaller units.",
						},
					},
				},
			},
			"soft_deletion": softDeletionAttribute("volume group"),
		},
	}
}

func (d *RecoverableVolumeGroupsDataSource) Schema(ctx context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the soft-deleted (destroyed, not yet eradicated) Volume Groups on a " +
			"Storage Pool. A volume group appears here instead of in the " +
			"[`everpure_azure_volume_groups` data source](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/data-sources/volume_groups) once it has " +
			"been soft-deleted; several entries can share the same `soft_deletion.previous_name` when a " +
			"volume group has been recreated and deleted again under the same name.\n\n" +
			"To recover one, create an [`everpure_azure_volume_group` resource](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/resources/volume_group) with " +
			"`source_type = \"recoverableVolumeGroup\"` and `source_recoverable_volume_group_id` set to its `id`.",
		Attributes: map[string]schema.Attribute{
			"storage_pool_id": schema.StringAttribute{
				Required:   true,
				CustomType: armids.IDType{},
				Validators: []validator.String{
					armids.TypeStoragePool.Validator(),
				},
				MarkdownDescription: "Full Azure Resource Manager ID of the Storage Pool to list recoverable volume groups on. Must be in the provider's own `subscription_id`.",
			},
			"recoverable_volume_groups": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The recoverable volume groups found, sorted by `id`.",
				NestedObject:        recoverableVolumeGroupsElementNestedObject(),
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": dataSourceTimeoutsBlock(ctx, "the recoverable volume groups", defaultVolumeGroupReadTimeout),
		},
	}
}

func (d *RecoverableVolumeGroupsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	client, ok := d.recoverableVolumeGroupsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data RecoverableVolumeGroupsDataSourceModel
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

	op := operation{action: actionList, kind: kindRecoverableVolumeGroups, in: data.StoragePoolID.ValueString(), unnamed: true}
	pool, idErr := parseIDInSubscription(armids.ParseStoragePoolID, data.StoragePoolID.ValueString(), kindStoragePool, d.clients.subscriptionID)
	if idErr != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(*idErr)))
		return
	}

	groups, err := listRecoverableVolumeGroups(ctx, client, pool.ResourceGroupName, pool.StoragePoolName)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(newNotFoundError(kindStoragePool, pool.FullID(), nil, d.clients.apiFailure(err)))))
			return
		}
		resp.Diagnostics.Append(d.clients.apiCallError(op, err))
		return
	}

	items := make([]recoverableVolumeGroupDataSourceItem, 0, len(groups))
	for _, rvg := range groups {
		// See ensureListElementProperties for why a nil-ID element is dropped and one
		// with no properties is kept.
		if rvg == nil || rvg.ID == nil {
			continue
		}
		g := *rvg
		ensureListElementProperties(&g.Properties)
		item, diags := recoverableVolumeGroupItemFrom(ctx, g)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		item.StoragePoolID = data.StoragePoolID
		items = append(items, item)
	}

	sort.Slice(items, func(i, j int) bool {
		return idLess(items[i].ID.ValueString(), items[j].ID.ValueString())
	})

	list, listDiags := types.ListValueFrom(ctx, recoverableVolumeGroupsElementNestedObject().Type(), items)
	resp.Diagnostics.Append(listDiags...)
	data.RecoverableVolumeGroups = list

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
