// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"sort"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	dstimeouts "github.com/hashicorp/terraform-plugin-framework-timeouts/datasource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

var (
	_ datasource.DataSource              = &StoragePoolsDataSource{}
	_ datasource.DataSourceWithConfigure = &StoragePoolsDataSource{}
)

// StoragePoolsDataSourceModel is the Terraform-side representation of the
// everpure_azure_storage_pools data source.
type StoragePoolsDataSourceModel struct {
	ResourceGroupName types.String     `tfsdk:"resource_group_name"`
	StoragePools      types.List       `tfsdk:"storage_pools"`
	Timeouts          dstimeouts.Value `tfsdk:"timeouts"`
}

type StoragePoolsDataSource struct {
	// dataSourceClientsHolder supplies Configure and storagePoolsClient via promotion;
	// see its own doc comment.
	dataSourceClientsHolder
}

func NewStoragePoolsDataSource() datasource.DataSource {
	return &StoragePoolsDataSource{}
}

func (d *StoragePoolsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_storage_pools"
}

// storagePoolsElementNestedObject is the "storage_pools" list's per-element schema: the
// singular everpure_azure_storage_pool data source's computed attributes, plus its own
// lookup inputs (name, resource_group_name) added back as Computed, with no validators.
func storagePoolsElementNestedObject() schema.NestedAttributeObject {
	attrs := storagePoolComputedAttributes()
	attrs["name"] = schema.StringAttribute{
		Computed:            true,
		MarkdownDescription: "Name of the storage pool.",
	}
	attrs["resource_group_name"] = schema.StringAttribute{
		Computed:            true,
		MarkdownDescription: "Name of the resource group the pool lives in.",
	}
	return schema.NestedAttributeObject{Attributes: attrs}
}

func (d *StoragePoolsDataSource) Schema(ctx context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists Storage Pools in a resource group, or across the provider's whole " +
			"`subscription_id` when `resource_group_name` is omitted.\n\n" +
			"To look up a single, already-known pool, use the " +
			"[`everpure_azure_storage_pool` data source](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/data-sources/storage_pool) instead.",
		Attributes: map[string]schema.Attribute{
			"resource_group_name": schema.StringAttribute{
				Optional:            true,
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
				MarkdownDescription: "Resource group to list pools in. Omitted to list every pool in the provider's own `subscription_id`.",
			},
			"storage_pools": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The storage pools found, sorted by `id`.",
				NestedObject:        storagePoolsElementNestedObject(),
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": dataSourceTimeoutsBlock(ctx, "the pools", defaultStoragePoolReadTimeout),
		},
	}
}

func (d *StoragePoolsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	client, ok := d.storagePoolsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data StoragePoolsDataSourceModel
	if resp.Diagnostics.Append(req.Config.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	readTimeout, diags := data.Timeouts.Read(ctx, defaultStoragePoolReadTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	op := operation{action: actionList, kind: kindStoragePools, in: subscriptionOrResourceGroupScope(d.clients.subscriptionID, data.ResourceGroupName.ValueString()), unnamed: true}
	var pools []*armpurestorageblock.StoragePool
	var err error
	if resourceGroupName := data.ResourceGroupName.ValueString(); resourceGroupName != "" {
		pools, err = listStoragePoolsByResourceGroup(ctx, client, resourceGroupName)
	} else {
		pools, err = listStoragePoolsBySubscription(ctx, client)
	}
	if err != nil {
		resp.Diagnostics.Append(d.clients.apiCallError(op, err))
		return
	}

	items := make([]storagePoolDataSourceItem, 0, len(pools))
	for _, pool := range pools {
		// See ensureListElementProperties for why a nil-ID element is dropped and one
		// with no properties is kept.
		if pool == nil || pool.ID == nil {
			continue
		}
		p := *pool
		ensureListElementProperties(&p.Properties)
		item, diags := storagePoolItemFrom(ctx, p)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}

		// Pool elements have no configured resource_group_name to preserve (the data
		// source may be listing across the whole subscription), so it's parsed from
		// each returned ID instead.
		parsed, err := armids.ParseStoragePoolID(item.ID.ValueString())
		if err != nil {
			resp.Diagnostics.Append(unexpectedResponseDiagnostic(newUnparseableResponseIDError(kindStoragePool, idErrorCause(err))))
			return
		}
		item.ResourceGroupName = types.StringValue(parsed.ResourceGroupName)

		items = append(items, item)
	}

	sort.Slice(items, func(i, j int) bool {
		return idLess(items[i].ID.ValueString(), items[j].ID.ValueString())
	})

	list, listDiags := types.ListValueFrom(ctx, storagePoolsElementNestedObject().Type(), items)
	resp.Diagnostics.Append(listDiags...)
	data.StoragePools = list

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// listStoragePoolsByResourceGroup pages through every storage pool in one resource
// group.
func listStoragePoolsByResourceGroup(
	ctx context.Context, client *armpurestorageblock.StoragePoolsClient, resourceGroupName string,
) ([]*armpurestorageblock.StoragePool, error) {
	var all []*armpurestorageblock.StoragePool
	pager := client.NewListByResourceGroupPager(resourceGroupName, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Value...)
	}
	return all, nil
}

// listStoragePoolsBySubscription pages through every storage pool in the provider's own
// subscription.
func listStoragePoolsBySubscription(
	ctx context.Context, client *armpurestorageblock.StoragePoolsClient,
) ([]*armpurestorageblock.StoragePool, error) {
	var all []*armpurestorageblock.StoragePool
	pager := client.NewListBySubscriptionPager(nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Value...)
	}
	return all, nil
}
