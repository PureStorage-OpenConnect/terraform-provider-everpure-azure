// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"

	dstimeouts "github.com/hashicorp/terraform-plugin-framework-timeouts/datasource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

var (
	_ datasource.DataSource              = &StoragePoolDataSource{}
	_ datasource.DataSourceWithConfigure = &StoragePoolDataSource{}
)

// StoragePoolDataSourceModel is the Terraform-side representation of the
// everpure_azure_storage_pool data source. storagePoolDataSourceItem carries every
// read-side attribute; it is also the plural everpure_azure_storage_pools data
// source's own list element type, so the two schemas' attributes stay identical by
// construction rather than by two hand-kept copies agreeing.
type StoragePoolDataSourceModel struct {
	storagePoolDataSourceItem
	Timeouts dstimeouts.Value `tfsdk:"timeouts"`
}

type StoragePoolDataSource struct {
	// dataSourceClientsHolder supplies Configure and storagePoolsClient via promotion;
	// see its own doc comment.
	dataSourceClientsHolder
}

func NewStoragePoolDataSource() datasource.DataSource {
	return &StoragePoolDataSource{}
}

func (d *StoragePoolDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_storage_pool"
}

// storagePoolComputedAttributes is the Computed attribute map shared by the singular
// everpure_azure_storage_pool data source's own schema and the plural
// everpure_azure_storage_pools data source's list element schema. It never includes the
// lookup inputs (name, resource_group_name) or "timeouts": each caller adds its own.
func storagePoolComputedAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"id": schema.StringAttribute{
			Computed:            true,
			CustomType:          armids.IDType{},
			MarkdownDescription: "The full Azure Resource Manager ID of the storage pool.",
		},
		"location": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Azure region the pool was created in, using its canonical short name, e.g. `eastus2`.",
		},
		"zone": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Availability zone within the region, e.g. `\"1\"`. Empty or null for a region with no availability zones.",
		},
		"reservation_id": schema.StringAttribute{
			Computed:            true,
			CustomType:          armids.IDType{},
			MarkdownDescription: "Full Azure Resource Manager ID of the Reservation that funds this pool.",
		},
		"provisioned_bandwidth_in_mbps": schema.Int64Attribute{
			Computed:            true,
			MarkdownDescription: "Bandwidth provisioned for the pool, in megabytes per second (MB/s).",
		},
		"tags": schema.MapAttribute{
			Computed:            true,
			ElementType:         types.StringType,
			MarkdownDescription: "A mapping of tags assigned to the pool.",
		},
		"internal_id": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Everpure's internal ID of the storage pool.",
		},
		"provisioned_iops": schema.Int64Attribute{
			Computed:            true,
			MarkdownDescription: "Total I/O operations per second (IOPS) provisioned for the pool.",
		},
		"data_retention_period_in_seconds": schema.Int64Attribute{
			Computed:            true,
			MarkdownDescription: "How long a destroyed volume group, volume or snapshot is kept before being eradicated, in seconds.",
		},
		"vnet_injection": schema.ListNestedAttribute{
			Computed:            true,
			MarkdownDescription: "The virtual network and subnet the pool is injected into.",
			NestedObject: schema.NestedAttributeObject{
				Attributes: map[string]schema.Attribute{
					"subnet_id": schema.StringAttribute{
						Computed:            true,
						CustomType:          armids.IDType{},
						MarkdownDescription: "Full Azure Resource Manager ID of the delegated subnet the pool's network interfaces are placed in.",
					},
					"virtual_network_id": schema.StringAttribute{
						Computed:            true,
						CustomType:          armids.IDType{},
						MarkdownDescription: "Full Azure Resource Manager ID of the virtual network `subnet_id` belongs to.",
					},
				},
			},
		},
		"platform_console": schema.ListNestedAttribute{
			Computed:            true,
			MarkdownDescription: "Platform console access (GUI, REST API and CLI) from the injected subnet. Null when console access is disabled.",
			NestedObject: schema.NestedAttributeObject{
				Attributes: map[string]schema.Attribute{
					"gui_enabled": schema.BoolAttribute{
						Computed:            true,
						MarkdownDescription: "Whether GUI access is enabled.",
					},
					"api_enabled": schema.BoolAttribute{
						Computed:            true,
						MarkdownDescription: "Whether REST API access is enabled.",
					},
					"cli_enabled": schema.BoolAttribute{
						Computed:            true,
						MarkdownDescription: "Whether CLI (SSH) access is enabled.",
					},
					"subnet_ids": schema.ListAttribute{
						Computed:            true,
						ElementType:         types.StringType,
						MarkdownDescription: "Subnets enabled for console access.",
					},
					"default_username": schema.StringAttribute{
						Computed:            true,
						MarkdownDescription: "Default username for console access, system-populated.",
					},
					"subnet": schema.ListNestedAttribute{
						Computed:            true,
						MarkdownDescription: "Per-subnet console access details, system-populated.",
						NestedObject: schema.NestedAttributeObject{
							Attributes: map[string]schema.Attribute{
								"id": schema.StringAttribute{
									Computed:            true,
									MarkdownDescription: "Full Azure Resource Manager ID of the subnet.",
								},
								"management_ip_address": schema.StringAttribute{
									Computed:            true,
									MarkdownDescription: "Management IP address assigned to the subnet.",
								},
								"service_backend_ip_addresses": schema.ListAttribute{
									Computed:            true,
									ElementType:         types.StringType,
									MarkdownDescription: "Service backend IP addresses assigned to the subnet.",
								},
							},
						},
					},
				},
			},
		},
	}
}

func (d *StoragePoolDataSource) Schema(ctx context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := storagePoolComputedAttributes()
	attrs["name"] = schema.StringAttribute{
		Required:            true,
		Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
		MarkdownDescription: "Name of the storage pool.",
	}
	attrs["resource_group_name"] = schema.StringAttribute{
		Required:            true,
		Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
		MarkdownDescription: "Name of the resource group the pool lives in. Looked up in the provider's own `subscription_id`.",
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up an existing Storage Pool by name and resource group.\n\n" +
			"To create and manage a storage pool, use the " +
			"[`everpure_azure_storage_pool` resource](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/resources/storage_pool).",
		Attributes: attrs,
		Blocks: map[string]schema.Block{
			"timeouts": dataSourceTimeoutsBlock(ctx, "the pool", defaultStoragePoolReadTimeout),
		},
	}
}

func (d *StoragePoolDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	client, ok := d.storagePoolsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data StoragePoolDataSourceModel
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

	name := data.Name.ValueString()
	resourceGroupName := data.ResourceGroupName.ValueString()
	poolID := armids.StoragePoolID{
		ResourceGroupID: armids.ResourceGroupID{SubscriptionID: d.clients.subscriptionID, ResourceGroupName: resourceGroupName},
		StoragePoolName: name,
	}.FullID()
	op := operation{action: actionRetrieve, kind: kindStoragePool, name: name, id: poolID}

	result, err := client.Get(ctx, resourceGroupName, name, nil)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(op.fail(newNotFoundError(kindStoragePool, poolID, &kindResourceGroup, d.clients.apiFailure(err))))
			return
		}
		resp.Diagnostics.Append(d.clients.apiCallError(op, err))
		return
	}

	item, diags := storagePoolItemFrom(ctx, result.StoragePool)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// name and resource_group_name are never taken from the API: ARM's storage pool
	// name lookup folds case, so a config that spells name differently than the pool's
	// own stored casing must still see its own spelling reflected back, not the API's
	// — Terraform core rejects a data source result that changes a configured,
	// non-Computed value.
	item.Name = data.Name
	item.ResourceGroupName = data.ResourceGroupName
	data.storagePoolDataSourceItem = item

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
