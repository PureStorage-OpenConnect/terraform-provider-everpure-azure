// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

var (
	_ resource.Resource                = &ReservationResource{}
	_ resource.ResourceWithConfigure   = &ReservationResource{}
	_ resource.ResourceWithImportState = &ReservationResource{}
	_ resource.ResourceWithMoveState   = &ReservationResource{}
	_ resource.ResourceWithIdentity    = &ReservationResource{}
	_ resource.ResourceWithModifyPlan  = &ReservationResource{}
)

// Per-operation defaults, overridable per resource instance via the `timeouts`
// attribute. They are well above the durations seen in practice (a create takes a few
// minutes, a plan change about two) because the plan change is the operation with a
// record of timing out.
const (
	defaultReservationCreateTimeout = 30 * time.Minute
	defaultReservationReadTimeout   = 5 * time.Minute
	defaultReservationUpdateTimeout = 30 * time.Minute
	defaultReservationDeleteTimeout = 30 * time.Minute
)

// reservationTermUnitValidators are the two checks on offer.term_unit: the date-duration
// grammar, and a length over one character, which rules out the bare "P" the grammar
// alone would accept. Exposed as a function so a test can run the very validators the
// schema uses.
func reservationTermUnitValidators() []validator.String {
	return []validator.String{
		stringvalidator.RegexMatches(reservationTermUnitPattern,
			"must be an ISO 8601 duration of years, months, weeks and days, such as \"P1M\", \"P1Y\" or \"P3Y\""),
		stringvalidator.LengthAtLeast(2),
	}
}

type ReservationResource struct {
	// clientsHolder supplies Configure and reservationsClient via promotion; see its
	// own doc comment.
	clientsHolder
}

func NewReservationResource() resource.Resource {
	return &ReservationResource{}
}

func (r *ReservationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_reservation"
}

func (r *ReservationResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = armids.ReservationIdentitySchema()
}

// keepStateUnlessPlanChanges is the plan modifier for a computed value that nothing
// but a plan change touches: the state value carries over, except in a plan that
// changes offer.plan_id, where the service may answer differently and the value stays
// unknown. UseStateForUnknown cannot express the exception, and without it the value
// would either show as known-after-apply on every unrelated update or, kept
// unconditionally, fail the apply as inconsistent after a plan change.
type keepStateUnlessPlanChanges struct{}

func (keepStateUnlessPlanChanges) Description(_ context.Context) string {
	return "Keeps the prior state value unless offer.plan_id changes."
}

func (m keepStateUnlessPlanChanges) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (keepStateUnlessPlanChanges) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	// On create there is no prior value; on a null prior value there is nothing to keep.
	if req.State.Raw.IsNull() || req.StateValue.IsNull() || !req.PlanValue.IsUnknown() {
		return
	}

	planIDPath := path.Root("offer").AtName("plan_id")
	var planned, current types.String
	if resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, planIDPath, &planned)...); resp.Diagnostics.HasError() {
		return
	}
	if resp.Diagnostics.Append(req.State.GetAttribute(ctx, planIDPath, &current)...); resp.Diagnostics.HasError() {
		return
	}
	if planned.IsUnknown() || !planned.Equal(current) {
		return
	}
	resp.PlanValue = req.StateValue
}

func (r *ReservationResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	immutable := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	keepUnlessPlanChanges := []planmodifier.String{keepStateUnlessPlanChanges{}}

	resp.Schema = schema.Schema{
		MarkdownDescription: reservationResourceDescription,
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				CustomType:    armids.IDType{},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "The full Azure Resource Manager ID of the reservation, e.g. " +
					"`/subscriptions/.../resourceGroups/.../providers/PureStorage.Block/reservations/res1`.",
			},
			"name": schema.StringAttribute{
				Required:      true,
				PlanModifiers: immutable,
				Validators: []validator.String{
					stringvalidator.RegexMatches(reservationNamePattern,
						"must be 1-127 characters long and contain only letters, numbers, hyphens and underscores"),
				},
				MarkdownDescription: "Name of the reservation. Changing this forces a new reservation to be created.",
			},
			"resource_group_name": schema.StringAttribute{
				Required: true,
				PlanModifiers: append([]planmodifier.String{armids.PreserveEquivalentResourceGroupName()},
					immutable...),
				MarkdownDescription: "Name of the resource group the reservation lives in. Changing this " +
					"forces a new reservation to be created.",
			},
			"location": schema.StringAttribute{
				Required:      true,
				PlanModifiers: immutable,
				Validators: []validator.String{
					stringvalidator.RegexMatches(locationPattern,
						"must be the canonical short region name (e.g. \"eastus2\"), not a display name "+
							"like \"East US 2\""),
				},
				MarkdownDescription: "Azure region the reservation is created in, using the canonical short " +
					"name, e.g. `eastus2`. Storage pools require a reservation in the same region. Changing " +
					"this forces a new reservation to be created.",
			},
			"offer": schema.SingleNestedAttribute{
				Required:            true,
				MarkdownDescription: reservationOfferDescription,
				Attributes: map[string]schema.Attribute{
					"offer_id": schema.StringAttribute{
						Required: true,
						MarkdownDescription: "ID of the Azure Marketplace offer to purchase, e.g. " +
							"`krypton_3_plan`. An opaque string that only the service judges. Cannot be " +
							"changed in place: a different offer is a different purchase.",
					},
					"plan_id": schema.StringAttribute{
						Required: true,
						MarkdownDescription: "ID of the offer's plan, e.g. `psc_capacity_perf_ondemand`. An " +
							"opaque string that only the service judges. Can be changed in place, which " +
							"changes the plan, and so the price, of the same purchase.",
					},
					"term_unit": schema.StringAttribute{
						Required:   true,
						Validators: reservationTermUnitValidators(),
						MarkdownDescription: "Length of the plan's billing term as an ISO 8601 duration, e.g. " +
							"`P1M` (one month), `P1Y` or `P3Y`. Compared exactly as written: `P12M` and `P1Y` " +
							"are different values, because Marketplace knows each term by one spelling. Which " +
							"terms exist for a plan is for the service to say. Cannot be changed in place: a " +
							"different term is a different purchase.",
					},
					"plan_name": schema.StringAttribute{
						Computed:      true,
						PlanModifiers: keepUnlessPlanChanges,
						MarkdownDescription: "Display name of the plan, as the service reports it. It is " +
							"not set by every offer.",
					},
				},
			},
			"user": schema.SingleNestedAttribute{
				Optional:            true,
				WriteOnly:           true,
				MarkdownDescription: reservationUserDescription,
				Attributes: map[string]schema.Attribute{
					"first_name": schema.StringAttribute{
						Required:            true,
						WriteOnly:           true,
						MarkdownDescription: "First name.",
					},
					"last_name": schema.StringAttribute{
						Required:            true,
						WriteOnly:           true,
						MarkdownDescription: "Last name.",
					},
					"email_address": schema.StringAttribute{
						Required:            true,
						WriteOnly:           true,
						MarkdownDescription: "Email address.",
					},
					"upn": schema.StringAttribute{
						Optional:            true,
						WriteOnly:           true,
						MarkdownDescription: "Entra ID user principal name of the contact, e.g. `jane@contoso.com`.",
					},
					"phone_number": schema.StringAttribute{
						Optional:            true,
						WriteOnly:           true,
						MarkdownDescription: "Phone number.",
					},
					"company": schema.SingleNestedAttribute{
						Optional:            true,
						WriteOnly:           true,
						MarkdownDescription: "The user's company.",
						Attributes: map[string]schema.Attribute{
							"company_name": schema.StringAttribute{
								Required:            true,
								WriteOnly:           true,
								MarkdownDescription: "Company name.",
							},
							"address": schema.SingleNestedAttribute{
								Optional:            true,
								WriteOnly:           true,
								MarkdownDescription: "The company's address.",
								Attributes: map[string]schema.Attribute{
									"address_line1": schema.StringAttribute{
										Required:            true,
										WriteOnly:           true,
										MarkdownDescription: "Address line 1.",
									},
									"address_line2": schema.StringAttribute{
										Optional:            true,
										WriteOnly:           true,
										MarkdownDescription: "Address line 2.",
									},
									"city": schema.StringAttribute{
										Required:            true,
										WriteOnly:           true,
										MarkdownDescription: "City.",
									},
									"state": schema.StringAttribute{
										Required:            true,
										WriteOnly:           true,
										MarkdownDescription: "State or province. Use an empty string where the country has none.",
									},
									"country": schema.StringAttribute{
										Required:            true,
										WriteOnly:           true,
										MarkdownDescription: "Country.",
									},
									"postal_code": schema.StringAttribute{
										Required:            true,
										WriteOnly:           true,
										MarkdownDescription: "Postal code.",
									},
								},
							},
						},
					},
				},
			},
			"tags": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "A mapping of tags to assign to the reservation. Can be changed in place. " +
					"Some environments append their own tags outside Terraform (e.g. an Azure Policy); " +
					"including those tags in this configuration, or excluding them via a " +
					"`lifecycle.ignore_changes` block, avoids a persistent diff against them.",
			},
			"internal_id": schema.StringAttribute{
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "Everpure's internal ID of the reservation.",
			},
			"marketplace_subscription_id": schema.StringAttribute{
				Computed:            true,
				PlanModifiers:       keepUnlessPlanChanges,
				MarkdownDescription: "ID of the Azure Marketplace subscription that bills for the reservation.",
			},
			"marketplace_subscription_status": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: keepUnlessPlanChanges,
				MarkdownDescription: "Status of the Azure Marketplace purchase: `PendingFulfillmentStart`, " +
					"`Subscribed`, `Suspended` or `Unsubscribed`. `Suspended` means the payment for the " +
					"purchase is missing. The status can lag behind the purchase: it has read " +
					"`PendingFulfillmentStart` on a purchase that was already activated, so do not wait " +
					"for it to change.",
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeoutsBlock(ctx, "the reservation", timeoutsDefaults{
				Create: defaultReservationCreateTimeout,
				Read:   defaultReservationReadTimeout,
				Update: defaultReservationUpdateTimeout,
				Delete: defaultReservationDeleteTimeout,
			}, ""),
		},
	}
}

// ModifyPlan is the offer guard. A reservation's offer is fixed for the life of its
// purchase, and the service can change it underneath a configuration (the refreshed
// state then differs from the config). Terraform cannot tell such a change from an edit
// in the configuration, and applying either would send the service an offer it may no
// longer accept, so any planned change to offer.offer_id or offer.term_unit against an
// existing reservation stops the plan, with a message that names the offer the
// reservation has now. offer.plan_id is not guarded: a plan change is the one update
// the reservation takes.
func (r *ReservationResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || req.State.Raw.IsNull() {
		return
	}

	var plan, state ReservationResourceModel
	if resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...); resp.Diagnostics.HasError() {
		return
	}
	if resp.Diagnostics.Append(req.State.Get(ctx, &state)...); resp.Diagnostics.HasError() {
		return
	}

	// An offer that is unknown while planning (e.g. `offer = var.offer` before the
	// variable resolves) cannot be compared yet; apply plans again with it known.
	// A prior state without an offer (the service reported none) has nothing to guard.
	if !objectIsKnown(plan.Offer) || !objectIsKnown(state.Offer) {
		return
	}
	plannedOffer, diags := decodeReservationOffer(ctx, plan.Offer)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}
	currentOffer, diags := decodeReservationOffer(ctx, state.Offer)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	changed := func(plannedValue, currentValue types.String) bool {
		return !plannedValue.IsUnknown() && !plannedValue.Equal(currentValue)
	}
	var fields []string
	if changed(plannedOffer.OfferID, currentOffer.OfferID) {
		fields = append(fields, "offer.offer_id")
	}
	if changed(plannedOffer.TermUnit, currentOffer.TermUnit) {
		fields = append(fields, "offer.term_unit")
	}
	if len(fields) == 0 {
		return
	}

	resp.Diagnostics.Append(diag.WithPath(path.Root("offer"), invalidAttributeValueDiagnostic(newReservationOfferChangedError(
		plan.Name.ValueString(), fields,
		currentOffer.OfferID.ValueString(), currentOffer.PlanID.ValueString(), currentOffer.TermUnit.ValueString(),
	))))
}

// checkStateSubscription reports, on id, a state ID in another subscription than the
// provider's, and whether op may go on. Read, Update and Delete address the reservation by
// resource_group_name and name in the provider's subscription, so only the state's id
// says which subscription it was created in. An empty id, as a failed create leaves, has
// no subscription to check.
func (r *ReservationResource) checkStateSubscription(op operation, id armids.IDValue, diags *diag.Diagnostics) bool {
	if id.ValueString() == "" {
		return true
	}
	reservation, err := armids.ParseReservationID(id.ValueString())
	if err != nil {
		diags.Append(diag.WithPath(path.Root("id"), op.fail(newInvalidStoredIDError(storedInState, idErrorCause(err)))))
		return false
	}
	if c := subscriptionMismatchCause(reservation, kindReservation, r.clients.subscriptionID); c != nil {
		diags.Append(diag.WithPath(path.Root("id"), op.fail(*c)))
		return false
	}
	return true
}

// requestedID composes the reservation's ID from the configured components, for the
// state written before the API has returned an ID of its own.
func (r *ReservationResource) requestedID(data ReservationResourceModel) string {
	return armids.ReservationID{
		ResourceGroupID: armids.ResourceGroupID{SubscriptionID: r.clients.subscriptionID, ResourceGroupName: data.ResourceGroupName.ValueString()},
		ReservationName: data.Name.ValueString(),
	}.FullID()
}

func (r *ReservationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	client, ok := r.reservationsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data ReservationResourceModel
	if resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	// user is write-only, so the plan holds null for it; the value to send is in the
	// config.
	var user types.Object
	if resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("user"), &user)...); resp.Diagnostics.HasError() {
		return
	}

	createTimeout, diags := data.Timeouts.Create(ctx, defaultReservationCreateTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()

	userDetails, diags := reservationUserFromConfig(ctx, user)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}
	body, diags := toReservation(ctx, data, userDetails)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	op := operation{action: actionCreate, kind: kindReservation, name: data.Name.ValueString(), id: r.requestedID(data)}
	existing, err := client.Get(ctx, data.ResourceGroupName.ValueString(), data.Name.ValueString(), nil)
	var existingState string
	if err == nil && existing.Properties != nil {
		existingState = provisioningStateName(existing.Properties.ProvisioningState)
	}
	if d := r.clients.existingObjectDiagnostic(op, "everpure_azure_reservation", existing.ID, existingState, err); d != nil {
		resp.Diagnostics.Append(d)
		return
	}

	poller, err := client.BeginCreate(ctx, data.ResourceGroupName.ValueString(), data.Name.ValueString(), body, nil)
	if err != nil {
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	// Save minimal state as soon as the create is accepted, so a failed poll leaves a
	// tainted resource Terraform knows about, which can be destroyed or imported,
	// instead of an orphan holding a purchase. The id is set from the requested
	// components so the tainted state carries an id and an identity that agree.
	partial := data
	partial.ID = armids.IDValue{StringValue: types.StringValue(r.requestedID(data))}
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, partial.ID.ValueString(), armids.ParseReservationID)...)
	resp.Diagnostics.Append(setPartialState(ctx, &resp.State, &partial)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	// The LRO's own result is not used: a GET is the one place that carries the
	// reservation as every later read will see it.
	got, err := client.Get(ctx, data.ResourceGroupName.ValueString(), data.Name.ValueString(), nil)
	if err != nil {
		resp.Diagnostics.Append(op.fail(newApiStepFailedError(op, step{action: actionRetrieve, kind: kindReservation, id: op.id}, r.clients.apiFailure(err))))
		return
	}

	resp.Diagnostics.Append(applyReservation(ctx, &data, got.Reservation)...)
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, data.ID.ValueString(), armids.ParseReservationID)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *ReservationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	client, ok := r.reservationsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data ReservationResourceModel
	if resp.Diagnostics.Append(req.State.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	readTimeout, diags := data.Timeouts.Read(ctx, defaultReservationReadTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	op := operation{action: actionRetrieve, kind: kindReservation, name: data.Name.ValueString(), id: r.requestedID(data)}
	if !r.checkStateSubscription(op, data.ID, &resp.Diagnostics) {
		return
	}
	got, err := client.Get(ctx, data.ResourceGroupName.ValueString(), data.Name.ValueString(), nil)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(setIdentityFromIDIfNull(ctx, resp.Identity, idOrFallback(data.ID, r.requestedID(data)), armids.ParseReservationID)...)
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	// The identity comes from the id already in state, read before applyReservationRead
	// overwrites it with the API's spelling. The id is null in a tainted state from a
	// failed create; the API's id then becomes the state id, so the identity follows it.
	identityID := data.ID.ValueString()
	resp.Diagnostics.Append(applyReservationRead(ctx, &data, got.Reservation)...)
	if identityID == "" {
		identityID = data.ID.ValueString()
	}
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, identityID, armids.ParseReservationID)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *ReservationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data ReservationResourceModel
	if resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	var priorState ReservationResourceModel
	if resp.Diagnostics.Append(req.State.Get(ctx, &priorState)...); resp.Diagnostics.HasError() {
		return
	}

	// An in-place update never changes the id, so the identity is carried over from
	// state unchanged.
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, priorState.ID.ValueString(), armids.ParseReservationID)...)

	planOffer, diags := decodeReservationOffer(ctx, data.Offer)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}
	stateOffer, diags := decodeReservationOffer(ctx, priorState.Offer)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	planChanged := !planOffer.PlanID.Equal(stateOffer.PlanID)
	tagsChanged := !data.Tags.Equal(priorState.Tags)

	if !planChanged && !tagsChanged {
		// Only the provider-side `timeouts` changed: nothing for the API to do. The
		// computed values the plan left unknown are carried over from state, which is
		// what an update that touches nothing leaves them as.
		carryOverReservationComputed(&data, priorState)
		resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
		return
	}

	client, ok := r.reservationsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	updateTimeout, diags := data.Timeouts.Update(ctx, defaultReservationUpdateTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()

	rg, name := data.ResourceGroupName.ValueString(), data.Name.ValueString()
	op := operation{action: actionUpdate, kind: kindReservation, name: name, id: r.requestedID(data)}
	if !r.checkStateSubscription(op, priorState.ID, &resp.Diagnostics) {
		return
	}

	if !planChanged {
		update, diags := toReservationUpdate(ctx, data)
		if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
			return
		}
		poller, err := client.BeginUpdate(ctx, rg, name, update, nil)
		if err != nil {
			if isNotFound(err) {
				resp.Diagnostics.Append(op.fail(newDeletedOutsideTerraformError(kindReservation, op.id)))
				return
			}
			resp.Diagnostics.Append(r.clients.apiCallError(op, err))
			return
		}
		if _, err := poller.PollUntilDone(ctx, nil); err != nil {
			if isNotFound(err) {
				resp.Diagnostics.Append(op.fail(newDeletedOutsideTerraformError(kindReservation, op.id)))
				return
			}
			resp.Diagnostics.Append(r.clients.apiCallError(op, err))
			return
		}
		// A tags PATCH changes nothing else about the reservation, and the status of
		// the purchase can move on its own: the computed values stay as planned and
		// the next refresh reports whatever changed.
		carryOverReservationComputed(&data, priorState)
		resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
		return
	}

	// A plan change is a re-PUT of the whole reservation. user is create-only and not in
	// state (it is write-only), but the body must still carry it, so it is echoed from
	// the reservation as the service holds it now. The same PUT carries the planned
	// tags, so it also covers a tags change in the same apply.
	current, err := client.Get(ctx, rg, name, nil)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(op.fail(newDeletedOutsideTerraformError(kindReservation, op.id)))
			return
		}
		resp.Diagnostics.Append(op.fail(newApiStepFailedError(op, step{action: actionRetrieve, kind: kindReservation, id: op.id}, r.clients.apiFailure(err))))
		return
	}
	var echoedUser *armpurestorageblock.UserDetails
	if current.Properties != nil {
		echoedUser = current.Properties.User
	}

	body, diags := toReservation(ctx, data, echoedUser)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	poller, err := client.BeginCreate(ctx, rg, name, body, nil)
	if err != nil {
		resp.Diagnostics.Append(op.fail(newReservationPlanChangeFailedError(op.id, r.clients.apiFailure(err))))
		return
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(op.fail(newDeletedOutsideTerraformError(kindReservation, op.id)))
			return
		}
		resp.Diagnostics.Append(op.fail(newReservationPlanChangeFailedError(op.id, r.clients.apiFailure(err))))
		return
	}

	got, err := client.Get(ctx, rg, name, nil)
	if err != nil {
		resp.Diagnostics.Append(op.fail(newApiStepFailedError(op, step{action: actionRetrieve, kind: kindReservation, id: op.id}, r.clients.apiFailure(err))))
		return
	}
	resp.Diagnostics.Append(applyReservation(ctx, &data, got.Reservation)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// carryOverReservationComputed copies every computed value from prior state into data,
// for an update that leaves them untouched.
func carryOverReservationComputed(data *ReservationResourceModel, prior ReservationResourceModel) {
	data.ID = prior.ID
	data.InternalID = prior.InternalID
	data.MarketplaceSubscriptionID = prior.MarketplaceSubscriptionID
	data.MarketplaceSubscriptionStatus = prior.MarketplaceSubscriptionStatus
	data.Offer = prior.Offer
	data.User = types.ObjectNull(reservationUserObjectType.AttrTypes)
}

func (r *ReservationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	client, ok := r.reservationsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data ReservationResourceModel
	if resp.Diagnostics.Append(req.State.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	deleteTimeout, diags := data.Timeouts.Delete(ctx, defaultReservationDeleteTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()

	op := operation{action: actionDelete, kind: kindReservation, name: data.Name.ValueString(), id: r.requestedID(data)}
	if !r.checkStateSubscription(op, data.ID, &resp.Diagnostics) {
		return
	}
	addDeleteError := func(err error) {
		if isNotFound(err) {
			return
		}
		f := r.clients.apiFailure(err)
		if isReservationHasStoragePools(err) {
			resp.Diagnostics.Append(op.fail(newReservationHasStoragePoolsError(op.id, f)))
			return
		}
		resp.Diagnostics.Append(op.fail(newApiCallFailedError(op, f)))
	}

	poller, err := client.BeginDelete(ctx, data.ResourceGroupName.ValueString(), data.Name.ValueString(), nil)
	if err != nil {
		addDeleteError(err)
		return
	}

	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		addDeleteError(err)
	}
}

func (r *ReservationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if r.clients == nil {
		resp.Diagnostics.Append(operationConfigureProvider.fail(newProviderNotConfiguredError()))
		return
	}
	expectedSubscriptionID := r.clients.subscriptionID

	importID, diags := resolveImportID(ctx, req, kindReservation, armids.ReservationID.FullID, armids.ParseReservationID)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	importOp := operation{action: actionImport, kind: kindReservation, id: importID, unnamed: true}

	parsed, idErr := parseIDInSubscription(armids.ParseReservationID, importID, kindReservation, expectedSubscriptionID)
	if idErr != nil {
		resp.Diagnostics.Append(importOp.fail(*idErr))
		return
	}

	resp.Diagnostics.Append(resp.Identity.Set(ctx, parsed)...)
	resp.Diagnostics.Append(seedReservationState(ctx, &resp.State, importID, parsed)...)
}

// MoveState takes over an azapi_resource of type PureStorage.Block/reservations.
func (r *ReservationResource) MoveState(context.Context) []resource.StateMover {
	return resourceMove[armids.ReservationID]{
		kind: kindReservation, sources: []moveSource{azapiResourceSource}, parse: armids.ParseReservationID, seed: seedReservationState,
	}.stateMovers()
}

const reservationResourceDescription = `Manages a Reservation: the Everpure resource that storage pools in an Azure subscription and region are billed through. Creating a reservation purchases the Azure Marketplace offer named in ` + "`offer`" + `; the reservation holds that purchase, and storage pools in the same region reference it through their ` + "`reservation_id`" + `.

Creation is asynchronous and takes a few minutes.

Only one reservation can exist in an Azure subscription per region.

Every storage pool of the Azure subscription in that region, and everything stored in those pools, depends on the reservation. Protect it with ` + "`lifecycle { prevent_destroy = true }`" + `, as the example does, so that no plan destroys or replaces it unnoticed.

~> **Destroying a reservation cancels its Marketplace purchase only if the purchase is managed through the reservation.** A purchase that exists as its own Azure resource, of type SaaS in the reservation's resource group, outlives the reservation and keeps billing until you delete that resource.

~> **A reservation can be imported, but its ` + "`user`" + ` is never read back.** ` + "`user`" + ` is write-only, so an imported reservation plans clean whether or not the configuration carries it.

Only ` + "`offer.plan_id`" + ` can be changed on an existing reservation. It changes the plan, and so the price, of the existing purchase in place; it is the same purchase afterwards.`

const reservationOfferDescription = `The Azure Marketplace offer to purchase. Only ` + "`plan_id`" + ` can be changed in place; ` + "`offer_id`" + ` and ` + "`term_unit`" + ` cannot, because a different offer or term is a different purchase.

Offer and plan IDs are opaque strings that only the service judges: new offers and plans appear over time, and private plans exist per customer. The Everpure public offer on the Azure Marketplace is ` + "`krypton_3_plan`" + `; its public plan with a monthly term is ` + "`psc_capacity_perf_ondemand`" + `. The publisher is fixed and not configurable.`

const reservationUserDescription = `Contact details for the purchase. Required by the service when creating a reservation; optional here only so that an imported reservation can be managed without it.

Write-only: sent on create, never stored in plan or state, and ignored if changed later. Needs Terraform 1.11 or newer.`
