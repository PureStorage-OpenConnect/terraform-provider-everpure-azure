// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"regexp"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

// reservationPublisherID is the Azure Marketplace publisher account every Everpure
// offer is listed under. The reservation API requires it in the offer details, but
// there is only one publisher, so the provider sends it itself instead of asking for it.
const reservationPublisherID = "purestoragemarketplaceadmin"

var (
	// reservationNamePattern is the reservation's own name grammar: 1-127 characters,
	// letters, digits, hyphens and underscores.
	reservationNamePattern = regexp.MustCompile(`^[a-zA-Z0-9\-_]{1,127}$`)

	// reservationTermUnitPattern is an ISO 8601 date duration made of years, months,
	// weeks and days, each optional. It also accepts the bare "P", which has no
	// component at all; the term_unit attribute rules that out with a separate length
	// validator (see reservationTermUnitValidators).
	reservationTermUnitPattern = regexp.MustCompile(`^P(?:\d+Y)?(?:\d+M)?(?:\d+W)?(?:\d+D)?$`)
)

// ReservationResourceModel is the Terraform-side representation of
// everpure_azure_reservation.
//
// Offer and User are types.Object, not Go structs: either can be unknown while a
// configuration is validated or planned (for example `offer = var.offer`), and a Go
// struct cannot represent that.
type ReservationResourceModel struct {
	ID                            armids.IDValue `tfsdk:"id"`
	Name                          types.String   `tfsdk:"name"`
	ResourceGroupName             types.String   `tfsdk:"resource_group_name"`
	Location                      types.String   `tfsdk:"location"`
	Offer                         types.Object   `tfsdk:"offer"`
	User                          types.Object   `tfsdk:"user"`
	Tags                          types.Map      `tfsdk:"tags"`
	InternalID                    types.String   `tfsdk:"internal_id"`
	MarketplaceSubscriptionID     types.String   `tfsdk:"marketplace_subscription_id"`
	MarketplaceSubscriptionStatus types.String   `tfsdk:"marketplace_subscription_status"`
	Timeouts                      timeouts.Value `tfsdk:"timeouts"`
}

// ReservationOfferModel is the `offer` attribute's object.
type ReservationOfferModel struct {
	OfferID  types.String `tfsdk:"offer_id"`
	PlanID   types.String `tfsdk:"plan_id"`
	TermUnit types.String `tfsdk:"term_unit"`
	PlanName types.String `tfsdk:"plan_name"`
}

var reservationOfferObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"offer_id":  types.StringType,
	"plan_id":   types.StringType,
	"term_unit": types.StringType,
	"plan_name": types.StringType,
}}

// ReservationUserModel is the `user` attribute's object.
type ReservationUserModel struct {
	FirstName    types.String `tfsdk:"first_name"`
	LastName     types.String `tfsdk:"last_name"`
	EmailAddress types.String `tfsdk:"email_address"`
	Upn          types.String `tfsdk:"upn"`
	PhoneNumber  types.String `tfsdk:"phone_number"`
	Company      types.Object `tfsdk:"company"`
}

type ReservationCompanyModel struct {
	CompanyName types.String `tfsdk:"company_name"`
	Address     types.Object `tfsdk:"address"`
}

type ReservationAddressModel struct {
	AddressLine1 types.String `tfsdk:"address_line1"`
	AddressLine2 types.String `tfsdk:"address_line2"`
	City         types.String `tfsdk:"city"`
	State        types.String `tfsdk:"state"`
	Country      types.String `tfsdk:"country"`
	PostalCode   types.String `tfsdk:"postal_code"`
}

var reservationAddressObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"address_line1": types.StringType,
	"address_line2": types.StringType,
	"city":          types.StringType,
	"state":         types.StringType,
	"country":       types.StringType,
	"postal_code":   types.StringType,
}}

var reservationCompanyObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"company_name": types.StringType,
	"address":      reservationAddressObjectType,
}}

// reservationUserObjectType mirrors the `user` attribute's schema.
var reservationUserObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"first_name":    types.StringType,
	"last_name":     types.StringType,
	"email_address": types.StringType,
	"upn":           types.StringType,
	"phone_number":  types.StringType,
	"company":       reservationCompanyObjectType,
}}

// objectIsKnown reports whether obj carries a value to decode: neither null nor
// unknown.
func objectIsKnown(obj types.Object) bool {
	return !obj.IsNull() && !obj.IsUnknown()
}

func decodeReservationOffer(ctx context.Context, obj types.Object) (ReservationOfferModel, diag.Diagnostics) {
	var m ReservationOfferModel
	if !objectIsKnown(obj) {
		return m, nil
	}
	return m, obj.As(ctx, &m, basetypes.ObjectAsOptions{})
}

// reservationUserFromConfig converts the write-only `user` value from the config into
// the request's user details. It returns nil for an absent or still-unknown value:
// apply only ever sees a resolved config, so an unknown one means there is nothing to
// send.
func reservationUserFromConfig(ctx context.Context, obj types.Object) (*armpurestorageblock.UserDetails, diag.Diagnostics) {
	var diags diag.Diagnostics
	if !objectIsKnown(obj) {
		return nil, diags
	}
	var u ReservationUserModel
	diags.Append(obj.As(ctx, &u, basetypes.ObjectAsOptions{})...)
	if diags.HasError() {
		return nil, diags
	}

	details := &armpurestorageblock.UserDetails{
		FirstName:    u.FirstName.ValueStringPointer(),
		LastName:     u.LastName.ValueStringPointer(),
		EmailAddress: u.EmailAddress.ValueStringPointer(),
		Upn:          u.Upn.ValueStringPointer(),
		PhoneNumber:  u.PhoneNumber.ValueStringPointer(),
	}

	if objectIsKnown(u.Company) {
		var c ReservationCompanyModel
		diags.Append(u.Company.As(ctx, &c, basetypes.ObjectAsOptions{})...)
		if diags.HasError() {
			return nil, diags
		}
		details.CompanyDetails = &armpurestorageblock.CompanyDetails{CompanyName: c.CompanyName.ValueStringPointer()}
		if objectIsKnown(c.Address) {
			var a ReservationAddressModel
			diags.Append(c.Address.As(ctx, &a, basetypes.ObjectAsOptions{})...)
			if diags.HasError() {
				return nil, diags
			}
			details.CompanyDetails.Address = &armpurestorageblock.Address{
				AddressLine1: a.AddressLine1.ValueStringPointer(),
				AddressLine2: a.AddressLine2.ValueStringPointer(),
				City:         a.City.ValueStringPointer(),
				State:        a.State.ValueStringPointer(),
				Country:      a.Country.ValueStringPointer(),
				PostalCode:   a.PostalCode.ValueStringPointer(),
			}
		}
	}
	return details, diags
}

// toReservation builds the PUT body shared by Create and the plan change in Update,
// from the planned values. user is supplied by the caller because it is write-only:
// Create takes it from the config, Update echoes the one the service returned.
//
// The offer details never carry a term ID. The service resolves the Marketplace term
// from the plan and the term unit, and a term ID sent along would only be something
// extra for Marketplace to validate, and to go stale when Marketplace re-issues it.
func toReservation(ctx context.Context, data ReservationResourceModel, user *armpurestorageblock.UserDetails) (armpurestorageblock.Reservation, diag.Diagnostics) {
	var diags diag.Diagnostics

	tags, d := tagsToAPI(ctx, data.Tags)
	diags.Append(d...)

	offer, d := decodeReservationOffer(ctx, data.Offer)
	diags.Append(d...)
	if !objectIsKnown(data.Offer) {
		diags.Append(providerInternalDiagnostic(newProviderBugError("offer missing from the plan")))
	}

	return armpurestorageblock.Reservation{
		Location: to.Ptr(data.Location.ValueString()),
		Tags:     tags,
		Properties: &armpurestorageblock.ReservationPropertiesBaseResourceProperties{
			User: user,
			Marketplace: &armpurestorageblock.MarketplaceDetails{
				OfferDetails: &armpurestorageblock.OfferDetails{
					PublisherID: to.Ptr(reservationPublisherID),
					OfferID:     offer.OfferID.ValueStringPointer(),
					PlanID:      offer.PlanID.ValueStringPointer(),
					TermUnit:    offer.TermUnit.ValueStringPointer(),
				},
			},
		},
	}, diags
}

// toReservationUpdate builds the PATCH body: tags only, since the gateway rejects
// anything else on a PATCH.
func toReservationUpdate(ctx context.Context, plan ReservationResourceModel) (armpurestorageblock.ReservationUpdate, diag.Diagnostics) {
	tags, diags := tagsToAPIForUpdate(ctx, plan.Tags)
	return armpurestorageblock.ReservationUpdate{Tags: tags}, diags
}

// canonicalLocation maps a location the way the schema spells it. A GET can answer
// with a region's display name ("East US 2 EUAP", as the Portal stores it) where the
// create was sent the short name ("eastus2euap"); reservations and storage pools cannot
// move, so that difference must not read as drift or force a replacement.
func canonicalLocation(location string) string {
	return strings.ToLower(strings.ReplaceAll(location, " ", ""))
}

// applyReservationComputed maps the service-assigned values of res onto data.
// It is the one mapping every consumer of a reservation response shares.
func applyReservationComputed(data *ReservationResourceModel, res armpurestorageblock.Reservation) {
	data.ID = armids.IDValue{StringValue: types.StringPointerValue(res.ID)}
	props := res.Properties
	if props == nil {
		return
	}
	data.InternalID = types.StringPointerValue(props.ReservationInternalID)
	data.MarketplaceSubscriptionID = types.StringNull()
	data.MarketplaceSubscriptionStatus = types.StringNull()
	if props.Marketplace != nil {
		data.MarketplaceSubscriptionID = types.StringPointerValue(props.Marketplace.SubscriptionID)
		if props.Marketplace.SubscriptionStatus != nil {
			data.MarketplaceSubscriptionStatus = types.StringValue(string(*props.Marketplace.SubscriptionStatus))
		}
	}
}

func reservationOfferDetails(res armpurestorageblock.Reservation) *armpurestorageblock.OfferDetails {
	if res.Properties == nil || res.Properties.Marketplace == nil {
		return nil
	}
	return res.Properties.Marketplace.OfferDetails
}

// reservationOfferFromAPI maps the response's offer details to the `offer` object.
func reservationOfferFromAPI(ctx context.Context, details *armpurestorageblock.OfferDetails) (types.Object, diag.Diagnostics) {
	return types.ObjectValueFrom(ctx, reservationOfferObjectType.AttrTypes, ReservationOfferModel{
		OfferID:  types.StringPointerValue(details.OfferID),
		PlanID:   types.StringPointerValue(details.PlanID),
		TermUnit: types.StringPointerValue(details.TermUnit),
		PlanName: types.StringPointerValue(details.PlanName),
	})
}

// applyReservation maps a response onto data after Create or Update. Unlike
// applyReservationRead it keeps every value the practitioner configured exactly as
// planned (offer_id, plan_id, term_unit, location, name, tags): the framework requires
// state after apply to equal the plan for those, and a service that spells one
// differently (or appends its own tag) must show as drift on the next refresh, not
// fail the apply. Only the values the service assigns are taken from the response.
func applyReservation(ctx context.Context, data *ReservationResourceModel, res armpurestorageblock.Reservation) diag.Diagnostics {
	var diags diag.Diagnostics
	if res.Properties == nil {
		diags.Append(unexpectedResponseDiagnostic(newNoPropertiesError(kindReservation)))
		return diags
	}
	applyReservationComputed(data, res)

	offer, d := decodeReservationOffer(ctx, data.Offer)
	diags.Append(d...)
	offer.PlanName = types.StringNull()
	if details := reservationOfferDetails(res); details != nil {
		offer.PlanName = types.StringPointerValue(details.PlanName)
	}
	newOffer, d := types.ObjectValueFrom(ctx, reservationOfferObjectType.AttrTypes, offer)
	diags.Append(d...)
	data.Offer = newOffer
	data.User = types.ObjectNull(reservationUserObjectType.AttrTypes)
	return diags
}

// applyReservationRead maps a GET response onto data exactly as Read does.
//
// name is not mapped, though the response carries it. It is a Required attribute, so the
// state saved after Create or Update must equal the configured value exactly; ARM treats
// resource names case-insensitively and may echo back a casing the practitioner never
// wrote, which would fail the apply with "Provider produced inconsistent result after
// apply". Nothing is lost by ignoring it: the name is always already on data by the time
// this runs — from the plan on Create and Update, from state on Read, and from the parsed
// ARM ID on import — and a reservation cannot be renamed in place, so the response can
// never carry a name the caller didn't ask for.
func applyReservationRead(ctx context.Context, data *ReservationResourceModel, res armpurestorageblock.Reservation) diag.Diagnostics {
	var diags diag.Diagnostics
	if res.Properties == nil {
		diags.Append(unexpectedResponseDiagnostic(newNoPropertiesError(kindReservation)))
		return diags
	}
	applyReservationComputed(data, res)

	if res.Location != nil {
		data.Location = types.StringValue(canonicalLocation(*res.Location))
	}

	// offer is always written from the response, so a change made by the service
	// shows as drift. A response without offer details leaves the prior value in
	// place: offer is Required, so nulling it would plan an update the practitioner's
	// config can never satisfy, for a reservation whose offer the service simply did
	// not report this time.
	if details := reservationOfferDetails(res); details != nil {
		offer, d := reservationOfferFromAPI(ctx, details)
		diags.Append(d...)
		data.Offer = offer
	} else if data.Offer.IsNull() || data.Offer.IsUnknown() {
		data.Offer = types.ObjectNull(reservationOfferObjectType.AttrTypes)
	}

	// user is write-only: the framework nulls it in every state it stores, so there
	// is nothing to map and the service's copy is never exposed.
	data.User = types.ObjectNull(reservationUserObjectType.AttrTypes)

	tags, d := tagsFromAPI(ctx, res.Tags, data.Tags)
	diags.Append(d...)
	data.Tags = tags
	return diags
}

// seedImportedReservation sets the attributes ImportState writes before the first
// Read: everything that can be derived from the ID alone.
func seedImportedReservation(data *ReservationResourceModel, importID string, parsed armids.ReservationID) {
	data.ID = armids.IDValue{StringValue: types.StringValue(importID)}
	data.Name = types.StringValue(parsed.ReservationName)
	data.ResourceGroupName = types.StringValue(parsed.ResourceGroupName)
}

// seedReservationState is what ImportState and a move both write to a new state: the
// seedImportedReservation attributes, and nothing else, so that state before the first
// Read has the same shape whichever way the reservation came under management.
func seedReservationState(ctx context.Context, state *tfsdk.State, id string, parsed armids.ReservationID) diag.Diagnostics {
	var seed ReservationResourceModel
	seedImportedReservation(&seed, id, parsed)
	var diags diag.Diagnostics
	diags.Append(state.SetAttribute(ctx, path.Root("id"), seed.ID)...)
	diags.Append(state.SetAttribute(ctx, path.Root("name"), seed.Name)...)
	diags.Append(state.SetAttribute(ctx, path.Root("resource_group_name"), seed.ResourceGroupName)...)
	return diags
}

// importedReservationFromAPI builds the resource object a fresh import of res followed
// by Read would leave in state. blank must be a model whose every attribute is a typed
// null, the shape state has before ImportState runs. The reservation's own ID (as the
// API spells it) plays the part of the import ID, and user stays null: it is
// write-only, so no state ever holds it.
func importedReservationFromAPI(
	ctx context.Context, blank ReservationResourceModel, res armpurestorageblock.Reservation,
) (ReservationResourceModel, armids.ReservationID, diag.Diagnostics) {
	var diags diag.Diagnostics
	data := blank
	if res.ID == nil {
		diags.Append(unexpectedResponseDiagnostic(newMissingResponseIDError(kindReservation)))
		return data, armids.ReservationID{}, diags
	}
	parsed, err := armids.ParseReservationID(*res.ID)
	if err != nil {
		diags.Append(unexpectedResponseDiagnostic(newUnparseableResponseIDError(kindReservation, idErrorCause(err))))
		return data, armids.ReservationID{}, diags
	}
	seedImportedReservation(&data, *res.ID, parsed)
	diags.Append(applyReservationRead(ctx, &data, res)...)
	return data, parsed, diags
}

// reservationDataSourceItem is the read-side attribute set shared by the singular
// everpure_azure_reservation data source and each element of the plural
// everpure_azure_reservations data source's reservations list. It has no user: that
// value is personal data the resource keeps out of state, and the data sources must
// not hand it back out.
type reservationDataSourceItem struct {
	ID                            armids.IDValue `tfsdk:"id"`
	Name                          types.String   `tfsdk:"name"`
	ResourceGroupName             types.String   `tfsdk:"resource_group_name"`
	Location                      types.String   `tfsdk:"location"`
	Offer                         types.Object   `tfsdk:"offer"`
	Tags                          types.Map      `tfsdk:"tags"`
	InternalID                    types.String   `tfsdk:"internal_id"`
	MarketplaceSubscriptionID     types.String   `tfsdk:"marketplace_subscription_id"`
	MarketplaceSubscriptionStatus types.String   `tfsdk:"marketplace_subscription_status"`
}

// reservationItemFrom maps an API response onto a fresh reservationDataSourceItem by
// routing it through the resource's own applyReservationRead, so the resource and every
// data source share one mapping. ResourceGroupName is left unset: the singular data
// source sets Name and ResourceGroupName to its configured values afterwards (ARM
// looks names up case-insensitively, and Terraform core rejects a data source result
// that changes a configured value), while the plural one parses it from each ID. Name is
// filled directly from res.Name rather than through applyReservationRead, which
// deliberately never maps it: the singular data source overwrites it with its own
// configured value afterward, while the plural data source has no configured name of its
// own and keeps the spelling the API returned.
func reservationItemFrom(ctx context.Context, res armpurestorageblock.Reservation) (reservationDataSourceItem, diag.Diagnostics) {
	scratch := ReservationResourceModel{
		Offer: types.ObjectNull(reservationOfferObjectType.AttrTypes),
		User:  types.ObjectNull(reservationUserObjectType.AttrTypes),
		Tags:  types.MapNull(types.StringType),
	}
	diags := applyReservationRead(ctx, &scratch, res)
	if diags.HasError() {
		return reservationDataSourceItem{}, diags
	}
	return reservationDataSourceItem{
		ID:                            scratch.ID,
		Name:                          types.StringPointerValue(res.Name),
		Location:                      scratch.Location,
		Offer:                         scratch.Offer,
		Tags:                          scratch.Tags,
		InternalID:                    scratch.InternalID,
		MarketplaceSubscriptionID:     scratch.MarketplaceSubscriptionID,
		MarketplaceSubscriptionStatus: scratch.MarketplaceSubscriptionStatus,
	}, diags
}
