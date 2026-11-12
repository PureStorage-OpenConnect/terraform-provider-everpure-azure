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
	_ datasource.DataSource              = &ReservationsDataSource{}
	_ datasource.DataSourceWithConfigure = &ReservationsDataSource{}
)

// ReservationsDataSourceModel is the Terraform-side representation of the
// everpure_azure_reservations data source.
type ReservationsDataSourceModel struct {
	ResourceGroupName types.String     `tfsdk:"resource_group_name"`
	Reservations      types.List       `tfsdk:"reservations"`
	Timeouts          dstimeouts.Value `tfsdk:"timeouts"`
}

type ReservationsDataSource struct {
	// dataSourceClientsHolder supplies Configure and reservationsClient via promotion;
	// see its own doc comment.
	dataSourceClientsHolder
}

func NewReservationsDataSource() datasource.DataSource {
	return &ReservationsDataSource{}
}

func (d *ReservationsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_reservations"
}

// reservationsElementNestedObject is the "reservations" list's per-element schema: the
// singular everpure_azure_reservation data source's computed attributes, plus its own
// lookup inputs (name, resource_group_name) added back as Computed, with no validators.
func reservationsElementNestedObject() schema.NestedAttributeObject {
	attrs := reservationComputedAttributes()
	attrs["name"] = schema.StringAttribute{
		Computed:            true,
		MarkdownDescription: "Name of the reservation.",
	}
	attrs["resource_group_name"] = schema.StringAttribute{
		Computed:            true,
		MarkdownDescription: "Name of the resource group the reservation lives in.",
	}
	return schema.NestedAttributeObject{Attributes: attrs}
}

func (d *ReservationsDataSource) Schema(ctx context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists Reservations in a resource group, or across the provider's whole " +
			"`subscription_id` when `resource_group_name` is omitted. The contact details a reservation " +
			"was created with are personal data and are not returned.\n\n" +
			"To look up a single, already-known reservation, use the " +
			"[`everpure_azure_reservation` data source](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/data-sources/azure_reservation) instead.",
		Attributes: map[string]schema.Attribute{
			"resource_group_name": schema.StringAttribute{
				Optional:            true,
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
				MarkdownDescription: "Resource group to list reservations in. Omitted to list every reservation in the provider's own `subscription_id`.",
			},
			"reservations": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The reservations found, sorted by `id`.",
				NestedObject:        reservationsElementNestedObject(),
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": dataSourceTimeoutsBlock(ctx, "the reservations", defaultReservationReadTimeout),
		},
	}
}

func (d *ReservationsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	client, ok := d.reservationsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data ReservationsDataSourceModel
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

	op := operation{action: actionList, kind: kindReservations, in: subscriptionOrResourceGroupScope(d.clients.subscriptionID, data.ResourceGroupName.ValueString()), unnamed: true}
	var reservations []*armpurestorageblock.Reservation
	var err error
	if resourceGroupName := data.ResourceGroupName.ValueString(); resourceGroupName != "" {
		reservations, err = listReservationsByResourceGroup(ctx, client, resourceGroupName)
	} else {
		reservations, err = listReservationsBySubscription(ctx, client)
	}
	if err != nil {
		resp.Diagnostics.Append(d.clients.apiCallError(op, err))
		return
	}

	items := make([]reservationDataSourceItem, 0, len(reservations))
	for _, reservation := range reservations {
		// See ensureListElementProperties for why a nil-ID element is dropped and one
		// with no properties is kept.
		if reservation == nil || reservation.ID == nil {
			continue
		}
		r := *reservation
		ensureListElementProperties(&r.Properties)
		item, diags := reservationItemFrom(ctx, r)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}

		// Elements have no configured resource_group_name to preserve (the data source
		// may be listing across the whole subscription), so it is parsed from each
		// returned ID instead.
		parsed, err := armids.ParseReservationID(item.ID.ValueString())
		if err != nil {
			resp.Diagnostics.Append(unexpectedResponseDiagnostic(newUnparseableResponseIDError(kindReservation, idErrorCause(err))))
			return
		}
		item.ResourceGroupName = types.StringValue(parsed.ResourceGroupName)

		items = append(items, item)
	}

	sort.Slice(items, func(i, j int) bool {
		return idLess(items[i].ID.ValueString(), items[j].ID.ValueString())
	})

	list, listDiags := types.ListValueFrom(ctx, reservationsElementNestedObject().Type(), items)
	resp.Diagnostics.Append(listDiags...)
	data.Reservations = list

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// listReservationsByResourceGroup pages through every reservation in one resource
// group.
func listReservationsByResourceGroup(
	ctx context.Context, client *armpurestorageblock.ReservationsClient, resourceGroupName string,
) ([]*armpurestorageblock.Reservation, error) {
	var all []*armpurestorageblock.Reservation
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

// listReservationsBySubscription pages through every reservation in the provider's own
// subscription.
func listReservationsBySubscription(
	ctx context.Context, client *armpurestorageblock.ReservationsClient,
) ([]*armpurestorageblock.Reservation, error) {
	var all []*armpurestorageblock.Reservation
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
