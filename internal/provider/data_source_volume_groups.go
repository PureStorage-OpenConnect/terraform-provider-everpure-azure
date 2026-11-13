// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"sort"

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
	_ datasource.DataSource              = &VolumeGroupsDataSource{}
	_ datasource.DataSourceWithConfigure = &VolumeGroupsDataSource{}
)

// VolumeGroupsDataSourceModel is the Terraform-side representation of the
// everpure_azure_volume_groups data source.
type VolumeGroupsDataSourceModel struct {
	StoragePoolID armids.IDValue   `tfsdk:"storage_pool_id"`
	VolumeGroups  types.List       `tfsdk:"volume_groups"`
	Timeouts      dstimeouts.Value `tfsdk:"timeouts"`
}

type VolumeGroupsDataSource struct {
	// dataSourceClientsHolder supplies Configure and volumeGroupsClient via promotion;
	// see its own doc comment.
	dataSourceClientsHolder
}

func NewVolumeGroupsDataSource() datasource.DataSource {
	return &VolumeGroupsDataSource{}
}

func (d *VolumeGroupsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_volume_groups"
}

// volumeGroupsElementNestedObject is the "volume_groups" list's per-element schema: the
// singular everpure_azure_volume_group data source's computed attributes, plus its own
// lookup inputs (name, storage_pool_id) added back as Computed, with no validators.
func volumeGroupsElementNestedObject() schema.NestedAttributeObject {
	attrs := volumeGroupComputedAttributes()
	attrs["name"] = schema.StringAttribute{
		Computed:            true,
		MarkdownDescription: "Name of the volume group.",
	}
	attrs["storage_pool_id"] = schema.StringAttribute{
		Computed:   true,
		CustomType: armids.IDType{},
		MarkdownDescription: "Full Azure Resource Manager ID of the Storage Pool this volume group lives on. " +
			"Same as the data source's own `storage_pool_id`.",
	}
	return schema.NestedAttributeObject{Attributes: attrs}
}

func (d *VolumeGroupsDataSource) Schema(ctx context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the Volume Groups on a Storage Pool.\n\n" +
			"To look up a single, already-known volume group, use the " +
			"[`everpure_azure_volume_group` data source](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/data-sources/volume_group) instead.",
		Attributes: map[string]schema.Attribute{
			"storage_pool_id": schema.StringAttribute{
				Required:   true,
				CustomType: armids.IDType{},
				Validators: []validator.String{
					armids.TypeStoragePool.Validator(),
				},
				MarkdownDescription: "Full Azure Resource Manager ID of the Storage Pool to list volume groups on. Must be in the provider's own `subscription_id`.",
			},
			"volume_groups": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The volume groups found, sorted by `id`.",
				NestedObject:        volumeGroupsElementNestedObject(),
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": dataSourceTimeoutsBlock(ctx, "the volume groups", defaultVolumeGroupReadTimeout),
		},
	}
}

func (d *VolumeGroupsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	client, ok := d.volumeGroupsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data VolumeGroupsDataSourceModel
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

	op := operation{action: actionList, kind: kindVolumeGroups, in: data.StoragePoolID.ValueString(), unnamed: true}
	pool, idErr := parseIDInSubscription(armids.ParseStoragePoolID, data.StoragePoolID.ValueString(), kindStoragePool, d.clients.subscriptionID)
	if idErr != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(*idErr)))
		return
	}

	var groups []*armpurestorageblock.VolumeGroup
	pager := client.NewListByStoragePoolPager(pool.ResourceGroupName, pool.StoragePoolName, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			if isNotFound(err) {
				resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(newNotFoundError(kindStoragePool, pool.FullID(), nil, d.clients.apiFailure(err)))))
				return
			}
			resp.Diagnostics.Append(d.clients.apiCallError(op, err))
			return
		}
		groups = append(groups, page.Value...)
	}

	items := make([]volumeGroupDataSourceItem, 0, len(groups))
	for _, vg := range groups {
		// See ensureListElementProperties for why a nil-ID element is dropped and one
		// with no properties is kept.
		if vg == nil || vg.ID == nil {
			continue
		}
		g := *vg
		ensureListElementProperties(&g.Properties)
		item, diags := volumeGroupItemFrom(ctx, g)
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

	list, listDiags := types.ListValueFrom(ctx, volumeGroupsElementNestedObject().Type(), items)
	resp.Diagnostics.Append(listDiags...)
	data.VolumeGroups = list

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
