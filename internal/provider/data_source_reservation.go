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
	_ datasource.DataSource              = &ReservationDataSource{}
	_ datasource.DataSourceWithConfigure = &ReservationDataSource{}
)

// ReservationDataSourceModel is the Terraform-side representation of the
// everpure_azure_reservation data source. reservationDataSourceItem carries every
// read-side attribute; it is also the plural everpure_azure_reservations data source's
// own list element type, so the two schemas' attributes stay identical by construction.
type ReservationDataSourceModel struct {
	reservationDataSourceItem
	Timeouts dstimeouts.Value `tfsdk:"timeouts"`
}

type ReservationDataSource struct {
	// dataSourceClientsHolder supplies Configure and reservationsClient via promotion;
	// see its own doc comment.
	dataSourceClientsHolder
}

func NewReservationDataSource() datasource.DataSource {
	return &ReservationDataSource{}
}

func (d *ReservationDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_reservation"
}

// reservationComputedAttributes is the Computed attribute map shared by the singular
// everpure_azure_reservation data source's own schema and the plural
// everpure_azure_reservations data source's list element schema. It never includes the
// lookup inputs (name, resource_group_name) or "timeouts": each caller adds its own.
// There is deliberately no user attribute: the resource keeps that personal data out of
// its state, and a data source must not return it either.
func reservationComputedAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"id": schema.StringAttribute{
			Computed:            true,
			CustomType:          armids.IDType{},
			MarkdownDescription: "The full Azure Resource Manager ID of the reservation.",
		},
		"location": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Azure region the reservation was created in, using its canonical short name, e.g. `eastus2`.",
		},
		"offer": schema.SingleNestedAttribute{
			Computed: true,
			MarkdownDescription: "The Azure Marketplace offer the reservation purchased, as the service reports it. " +
				"Null when the service reports no offer for the reservation.",
			Attributes: map[string]schema.Attribute{
				"offer_id": schema.StringAttribute{
					Computed:            true,
					MarkdownDescription: "ID of the Azure Marketplace offer.",
				},
				"plan_id": schema.StringAttribute{
					Computed:            true,
					MarkdownDescription: "ID of the offer's plan.",
				},
				"term_unit": schema.StringAttribute{
					Computed:            true,
					MarkdownDescription: "Length of the plan's billing term as an ISO 8601 duration, e.g. `P1M`.",
				},
				"plan_name": schema.StringAttribute{
					Computed:            true,
					MarkdownDescription: "Display name of the plan, as the service reports it. It is not set by every offer.",
				},
			},
		},
		"tags": schema.MapAttribute{
			Computed:            true,
			ElementType:         types.StringType,
			MarkdownDescription: "A mapping of tags assigned to the reservation.",
		},
		"internal_id": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Everpure's internal ID of the reservation.",
		},
		"marketplace_subscription_id": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "ID of the Azure Marketplace subscription that bills for the reservation.",
		},
		"marketplace_subscription_status": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "Status of the Azure Marketplace purchase: `PendingFulfillmentStart`, `Subscribed`, " +
				"`Suspended` or `Unsubscribed`. The status can lag behind the purchase.",
		},
	}
}

func (d *ReservationDataSource) Schema(ctx context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := reservationComputedAttributes()
	attrs["name"] = schema.StringAttribute{
		Required:            true,
		Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
		MarkdownDescription: "Name of the reservation.",
	}
	attrs["resource_group_name"] = schema.StringAttribute{
		Required:            true,
		Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
		MarkdownDescription: "Name of the resource group the reservation lives in. Looked up in the provider's own `subscription_id`.",
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up an existing Reservation by name and resource group. The contact details " +
			"the reservation was created with are personal data and are not returned.\n\n" +
			"To create and manage a reservation, use the " +
			"[`everpure_azure_reservation` resource](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/resources/reservation).",
		Attributes: attrs,
		Blocks: map[string]schema.Block{
			"timeouts": dataSourceTimeoutsBlock(ctx, "the reservation", defaultReservationReadTimeout),
		},
	}
}

func (d *ReservationDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	client, ok := d.reservationsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data ReservationDataSourceModel
	if resp.Diagnostics.Append(req.Config.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	readTimeout, diags := data.Timeouts.Read(ctx, defaultReservationReadTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	name := data.Name.ValueString()
	resourceGroupName := data.ResourceGroupName.ValueString()

	reservationID := armids.ReservationID{
		ResourceGroupID: armids.ResourceGroupID{SubscriptionID: d.clients.subscriptionID, ResourceGroupName: resourceGroupName},
		ReservationName: name,
	}.FullID()
	op := operation{action: actionRetrieve, kind: kindReservation, name: name, id: reservationID}

	result, err := client.Get(ctx, resourceGroupName, name, nil)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(op.fail(newNotFoundError(kindReservation, reservationID, &kindResourceGroup, d.clients.apiFailure(err))))
			return
		}
		resp.Diagnostics.Append(d.clients.apiCallError(op, err))
		return
	}

	item, diags := reservationItemFrom(ctx, result.Reservation)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// name and resource_group_name are never taken from the API: ARM's name lookup
	// folds case, so a config that spells name differently than the reservation's own
	// stored casing must still see its own spelling reflected back. Terraform core
	// rejects a data source result that changes a configured, non-Computed value.
	item.Name = data.Name
	item.ResourceGroupName = data.ResourceGroupName
	data.reservationDataSourceItem = item

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
