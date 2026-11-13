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
	_ datasource.DataSource              = &VolumesDataSource{}
	_ datasource.DataSourceWithConfigure = &VolumesDataSource{}
)

// filterAndSortVolumes drops soft-deleted (destroyed) entries unless includeDestroyed
// is true, then sorts what's left by ID (see idLess) — extracted out of Read so the
// filtering and the ordering are both unit-testable independent of any API call or
// Terraform plumbing. A nil entry (defensive: the SDK's own list result type allows it),
// or one with no ID (see ensureListElementProperties), is dropped either way.
func filterAndSortVolumes(volumes []*armpurestorageblock.Volume, includeDestroyed bool) []*armpurestorageblock.Volume {
	kept := make([]*armpurestorageblock.Volume, 0, len(volumes))
	for _, v := range volumes {
		if v == nil || v.ID == nil {
			continue
		}
		if !includeDestroyed && isVolumeSoftDeleted(*v) {
			continue
		}
		kept = append(kept, v)
	}
	sort.Slice(kept, func(i, j int) bool {
		return idLess(derefString(kept[i].ID), derefString(kept[j].ID))
	})
	return kept
}

// volumeListItem is one element of the everpure_azure_volumes data source's volumes
// list: the singular everpure_azure_volume data source's own attribute set, plus
// soft_deletion — a list of volumes can include destroyed (soft-deleted) entries that a
// name+volume_group_id lookup never has to represent, since a soft-deleted volume's
// original name always 404s.
type volumeListItem struct {
	volumeDataSourceItem
	SoftDeletion types.Object `tfsdk:"soft_deletion"`
}

// VolumesDataSourceModel is the Terraform-side representation of the
// everpure_azure_volumes data source.
type VolumesDataSourceModel struct {
	VolumeGroupID    armids.IDValue   `tfsdk:"volume_group_id"`
	IncludeDestroyed types.Bool       `tfsdk:"include_destroyed"`
	Volumes          types.List       `tfsdk:"volumes"`
	Timeouts         dstimeouts.Value `tfsdk:"timeouts"`
}

type VolumesDataSource struct {
	// dataSourceClientsHolder supplies Configure and volumesClient via promotion; see
	// its own doc comment.
	dataSourceClientsHolder
}

func NewVolumesDataSource() datasource.DataSource {
	return &VolumesDataSource{}
}

func (d *VolumesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_volumes"
}

// volumesElementNestedObject is the "volumes" list's per-element schema: the singular
// everpure_azure_volume data source's computed attributes, plus its own lookup inputs
// (name, volume_group_id) added back as Computed, with no validators, plus
// soft_deletion.
func volumesElementNestedObject() schema.NestedAttributeObject {
	attrs := volumeComputedAttributes()
	attrs["name"] = schema.StringAttribute{
		Computed: true,
		MarkdownDescription: "Name of the volume. A soft-deleted volume's name is the timestamped name it " +
			"was renamed to on deletion; see `soft_deletion.previous_name` for its original name.",
	}
	attrs["volume_group_id"] = schema.StringAttribute{
		Computed:   true,
		CustomType: armids.IDType{},
		MarkdownDescription: "Full Azure Resource Manager ID of the volume group this volume lives on. " +
			"Same as the data source's own `volume_group_id`.",
	}
	attrs["soft_deletion"] = softDeletionAttribute("volume")
	return schema.NestedAttributeObject{Attributes: attrs}
}

func (d *VolumesDataSource) Schema(ctx context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the Volumes in a Volume Group. Soft-deleted (destroyed, not yet " +
			"eradicated) volumes are excluded by default; set `include_destroyed = true` to include them.\n\n" +
			"To look up a single, already-known volume, use the " +
			"[`everpure_azure_volume` data source](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/data-sources/volume) instead.",
		Attributes: map[string]schema.Attribute{
			"volume_group_id": schema.StringAttribute{
				Required:   true,
				CustomType: armids.IDType{},
				Validators: []validator.String{
					armids.TypeVolumeGroup.Validator(),
				},
				MarkdownDescription: "Full Azure Resource Manager ID of the volume group to list volumes in. Must be in the provider's own `subscription_id`.",
			},
			"include_destroyed": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Whether to include soft-deleted (destroyed, not yet eradicated) volumes " +
					"in the result. Defaults to `false`.",
			},
			"volumes": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The volumes found, sorted by `id`.",
				NestedObject:        volumesElementNestedObject(),
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": dataSourceTimeoutsBlock(ctx, "the volumes", defaultVolumeReadTimeout),
		},
	}
}

func (d *VolumesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	client, ok := d.volumesClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data VolumesDataSourceModel
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

	op := operation{action: actionList, kind: kindVolumes, in: data.VolumeGroupID.ValueString(), unnamed: true}
	vg, idErr := parseIDInSubscription(armids.ParseVolumeGroupID, data.VolumeGroupID.ValueString(), kindVolumeGroup, d.clients.subscriptionID)
	if idErr != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(*idErr)))
		return
	}

	// A null include_destroyed (the attribute has no schema Default: see
	// datasource/schema.BoolAttribute) means false.
	includeDestroyed := data.IncludeDestroyed.ValueBool()

	var volumes []*armpurestorageblock.Volume
	pager := client.NewListByVolumeGroupPager(vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			if isNotFound(err) {
				resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(newNotFoundError(kindVolumeGroup, vg.FullID(), nil, d.clients.apiFailure(err)))))
				return
			}
			resp.Diagnostics.Append(d.clients.apiCallError(op, err))
			return
		}
		volumes = append(volumes, page.Value...)
	}

	volumes = filterAndSortVolumes(volumes, includeDestroyed)

	items := make([]volumeListItem, 0, len(volumes))
	for _, v := range volumes {
		// A volume with no properties can't be classified as soft-deleted, so it's kept
		// even when include_destroyed is false; see ensureListElementProperties for why
		// it's kept at all.
		vol := *v
		ensureListElementProperties(&vol.Properties)
		item, diags := volumeItemFrom(vol)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		item.VolumeGroupID = data.VolumeGroupID

		softDeletion, diags := softDeletionFromAPI(vol.Properties.SoftDeletion)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}

		items = append(items, volumeListItem{volumeDataSourceItem: item, SoftDeletion: softDeletion})
	}

	list, listDiags := types.ListValueFrom(ctx, volumesElementNestedObject().Type(), items)
	resp.Diagnostics.Append(listDiags...)
	data.Volumes = list

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
