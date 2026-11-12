// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

func testReservationOfferObject(t *testing.T, offerID, planID, termUnit string, planName types.String) types.Object {
	t.Helper()
	obj, diags := types.ObjectValueFrom(context.Background(), reservationOfferObjectType.AttrTypes, ReservationOfferModel{
		OfferID:  types.StringValue(offerID),
		PlanID:   types.StringValue(planID),
		TermUnit: types.StringValue(termUnit),
		PlanName: planName,
	})
	if diags.HasError() {
		t.Fatalf("unexpected error: %v", diags)
	}
	return obj
}

func testReservationUserObject(t *testing.T, u ReservationUserModel) types.Object {
	t.Helper()
	obj, diags := types.ObjectValueFrom(context.Background(), reservationUserObjectType.AttrTypes, u)
	if diags.HasError() {
		t.Fatalf("unexpected error: %v", diags)
	}
	return obj
}

func baseReservationModel(t *testing.T) ReservationResourceModel {
	t.Helper()
	return ReservationResourceModel{
		Name:              types.StringValue("res1"),
		ResourceGroupName: types.StringValue("rg"),
		Location:          types.StringValue("eastus2"),
		Offer:             testReservationOfferObject(t, "offer_a", "plan_a", "P1M", types.StringUnknown()),
		User:              types.ObjectNull(reservationUserObjectType.AttrTypes),
		Tags:              types.MapNull(types.StringType),
	}
}

func apiReservation(planID string) armpurestorageblock.Reservation {
	res := fakeSeededReservation(planID)
	res.ID = to.Ptr("/subscriptions/s/resourceGroups/rg/providers/PureStorage.Block/reservations/res1")
	res.Name = to.Ptr("res1")
	return res
}

func TestToReservation(t *testing.T) {
	ctx := context.Background()

	t.Run("sends the fixed publisher and the configured offer, and never a term ID", func(t *testing.T) {
		data := baseReservationModel(t)
		got, diags := toReservation(ctx, data, nil)
		if diags.HasError() {
			t.Fatalf("unexpected error: %v", diags)
		}
		offer := got.Properties.Marketplace.OfferDetails
		if derefString(offer.PublisherID) != "purestoragemarketplaceadmin" {
			t.Fatalf("publisherId = %q", derefString(offer.PublisherID))
		}
		if derefString(offer.OfferID) != "offer_a" || derefString(offer.PlanID) != "plan_a" || derefString(offer.TermUnit) != "P1M" {
			t.Fatalf("offer details = %+v", *offer)
		}
		if offer.TermID != nil || offer.PlanName != nil {
			t.Fatalf("termId = %v, planName = %v, want neither sent", offer.TermID, offer.PlanName)
		}
		if got.Properties.Marketplace.SaaSResourceID != nil {
			t.Fatal("saaSResourceId must never be sent")
		}
		if got.Properties.User != nil {
			t.Fatal("a user was sent without being given one")
		}
		if got.Tags != nil {
			t.Fatalf("tags = %v, want none for a null tags value", got.Tags)
		}
		if derefString(got.Location) != "eastus2" {
			t.Fatalf("location = %q", derefString(got.Location))
		}
	})

	t.Run("carries the planned tags and the given user", func(t *testing.T) {
		data := baseReservationModel(t)
		data.Tags = types.MapValueMust(types.StringType, map[string]attr.Value{"env": types.StringValue("prod")})
		user := &armpurestorageblock.UserDetails{FirstName: to.Ptr("Ada")}
		got, diags := toReservation(ctx, data, user)
		if diags.HasError() {
			t.Fatalf("unexpected error: %v", diags)
		}
		if derefString(got.Tags["env"]) != "prod" {
			t.Fatalf("tags = %v", got.Tags)
		}
		if got.Properties.User != user {
			t.Fatal("the user was not passed through")
		}
	})

	t.Run("an unknown offer is an error, not a request", func(t *testing.T) {
		data := baseReservationModel(t)
		data.Offer = types.ObjectUnknown(reservationOfferObjectType.AttrTypes)
		if _, diags := toReservation(ctx, data, nil); !diags.HasError() {
			t.Fatal("expected an error for an unknown offer")
		}
	})
}

func TestToReservationUpdate(t *testing.T) {
	ctx := context.Background()

	t.Run("null tags become an explicit empty map, so the PATCH clears them", func(t *testing.T) {
		got, diags := toReservationUpdate(ctx, baseReservationModel(t))
		if diags.HasError() {
			t.Fatalf("unexpected error: %v", diags)
		}
		if got.Tags == nil || len(got.Tags) != 0 {
			t.Fatalf("tags = %#v, want a non-nil empty map", got.Tags)
		}
		if got.Properties != nil {
			t.Fatal("a PATCH must carry tags only")
		}
	})
}

func TestReservationUserFromConfig(t *testing.T) {
	ctx := context.Background()

	t.Run("null and unknown yield no user", func(t *testing.T) {
		for _, obj := range []types.Object{
			types.ObjectNull(reservationUserObjectType.AttrTypes),
			types.ObjectUnknown(reservationUserObjectType.AttrTypes),
		} {
			got, diags := reservationUserFromConfig(ctx, obj)
			if diags.HasError() || got != nil {
				t.Fatalf("got %+v (diagnostics %v), want nil", got, diags)
			}
		}
	})

	t.Run("maps every field, including company and address", func(t *testing.T) {
		address, diags := types.ObjectValueFrom(ctx, reservationAddressObjectType.AttrTypes, ReservationAddressModel{
			AddressLine1: types.StringValue("1 Main"), AddressLine2: types.StringNull(), City: types.StringValue("London"),
			State: types.StringValue(""), Country: types.StringValue("UK"), PostalCode: types.StringValue("N1"),
		})
		if diags.HasError() {
			t.Fatal(diags)
		}
		company, diags := types.ObjectValueFrom(ctx, reservationCompanyObjectType.AttrTypes, ReservationCompanyModel{
			CompanyName: types.StringValue("Acme"), Address: address,
		})
		if diags.HasError() {
			t.Fatal(diags)
		}
		obj := testReservationUserObject(t, ReservationUserModel{
			FirstName: types.StringValue("Ada"), LastName: types.StringValue("L"), EmailAddress: types.StringValue("a@x"),
			Upn: types.StringValue("a@upn"), PhoneNumber: types.StringNull(), Company: company,
		})
		got, diags := reservationUserFromConfig(ctx, obj)
		if diags.HasError() {
			t.Fatal(diags)
		}
		if derefString(got.FirstName) != "Ada" || derefString(got.Upn) != "a@upn" || got.PhoneNumber != nil {
			t.Fatalf("user = %+v", got)
		}
		if got.CompanyDetails == nil || derefString(got.CompanyDetails.CompanyName) != "Acme" {
			t.Fatalf("company = %+v", got.CompanyDetails)
		}
		a := got.CompanyDetails.Address
		if a == nil || derefString(a.AddressLine1) != "1 Main" || a.AddressLine2 != nil || a.State == nil || *a.State != "" {
			t.Fatalf("address = %+v", a)
		}
	})

	t.Run("a user without a company has no company details", func(t *testing.T) {
		obj := testReservationUserObject(t, ReservationUserModel{
			FirstName: types.StringValue("Ada"), LastName: types.StringValue("L"), EmailAddress: types.StringValue("a@x"),
			Upn: types.StringNull(), PhoneNumber: types.StringNull(),
			Company: types.ObjectNull(reservationCompanyObjectType.AttrTypes),
		})
		got, diags := reservationUserFromConfig(ctx, obj)
		if diags.HasError() || got.CompanyDetails != nil {
			t.Fatalf("got %+v (diagnostics %v)", got, diags)
		}
	})
}

func TestCanonicalLocation(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"already canonical", "eastus2", "eastus2"},
		{"display name", "East US 2", "eastus2"},
		{"display name with a suffix word", "East US 2 EUAP", "eastus2euap"},
		{"two-word display name", "West Europe", "westeurope"},
		{"empty", "", ""},
		{"upper case and a hyphen", "Already-Lowered", "already-lowered"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canonicalLocation(tt.in); got != tt.want {
				t.Fatalf("canonicalLocation(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestApplyReservationRead(t *testing.T) {
	ctx := context.Background()

	t.Run("maps the computed values, the offer, the canonical location and no user", func(t *testing.T) {
		data := baseReservationModel(t)
		res := apiReservation("plan_b")
		res.Location = to.Ptr("East US 2 EUAP")
		if diags := applyReservationRead(ctx, &data, res); diags.HasError() {
			t.Fatalf("unexpected error: %v", diags)
		}
		if data.ID.ValueString() != *res.ID || data.InternalID.ValueString() != "internal-seeded" {
			t.Fatalf("id = %q, internal_id = %q", data.ID.ValueString(), data.InternalID.ValueString())
		}
		if data.MarketplaceSubscriptionID.ValueString() != "purchase-seeded" || data.MarketplaceSubscriptionStatus.ValueString() != "Subscribed" {
			t.Fatalf("marketplace = %q / %q", data.MarketplaceSubscriptionID.ValueString(), data.MarketplaceSubscriptionStatus.ValueString())
		}
		if data.Location.ValueString() != "eastus2euap" {
			t.Fatalf("location = %q", data.Location.ValueString())
		}
		offer, _ := decodeReservationOffer(ctx, data.Offer)
		if offer.PlanID.ValueString() != "plan_b" || offer.PlanName.ValueString() != "Plan plan_b" || offer.TermUnit.ValueString() != "P1M" {
			t.Fatalf("offer = %+v", offer)
		}
		if !data.User.IsNull() {
			t.Fatal("user must stay null")
		}
	})

	t.Run("a response without offer details keeps the prior offer", func(t *testing.T) {
		data := baseReservationModel(t)
		data.Offer = testReservationOfferObject(t, "offer_a", "plan_a", "P1M", types.StringValue("Plan A"))
		res := apiReservation("plan_b")
		res.Properties.Marketplace.OfferDetails = nil
		if diags := applyReservationRead(ctx, &data, res); diags.HasError() {
			t.Fatalf("unexpected error: %v", diags)
		}
		offer, _ := decodeReservationOffer(ctx, data.Offer)
		if offer.PlanID.ValueString() != "plan_a" || offer.PlanName.ValueString() != "Plan A" {
			t.Fatalf("offer = %+v, want the prior one", offer)
		}
	})

	t.Run("a response without offer details leaves an imported reservation's offer null", func(t *testing.T) {
		data := baseReservationModel(t)
		data.Offer = types.ObjectNull(reservationOfferObjectType.AttrTypes)
		res := apiReservation("plan_b")
		res.Properties.Marketplace.OfferDetails = nil
		if diags := applyReservationRead(ctx, &data, res); diags.HasError() {
			t.Fatalf("unexpected error: %v", diags)
		}
		if !data.Offer.IsNull() {
			t.Fatal("offer must stay null when there was none and the service reports none")
		}
	})

	t.Run("a response without marketplace data is nil-safe", func(t *testing.T) {
		data := baseReservationModel(t)
		res := apiReservation("plan_b")
		res.Properties.Marketplace = nil
		if diags := applyReservationRead(ctx, &data, res); diags.HasError() {
			t.Fatalf("unexpected error: %v", diags)
		}
		if !data.MarketplaceSubscriptionID.IsNull() || !data.MarketplaceSubscriptionStatus.IsNull() {
			t.Fatal("marketplace values must be null")
		}
	})

	t.Run("a response without properties is an error", func(t *testing.T) {
		data := baseReservationModel(t)
		if diags := applyReservationRead(ctx, &data, armpurestorageblock.Reservation{}); !diags.HasError() {
			t.Fatal("expected an error")
		}
	})

	t.Run("name is never taken from the response", func(t *testing.T) {
		data := baseReservationModel(t)
		data.Name = types.StringValue("Res1")
		res := apiReservation("plan_a")
		res.Name = to.Ptr("something-else")
		if diags := applyReservationRead(ctx, &data, res); diags.HasError() {
			t.Fatalf("unexpected error: %v", diags)
		}
		if data.Name.ValueString() != "Res1" {
			t.Fatalf("name = %q, want it kept", data.Name.ValueString())
		}
	})

	t.Run("tags come from the response", func(t *testing.T) {
		data := baseReservationModel(t)
		res := apiReservation("plan_a")
		res.Tags = map[string]*string{"env": to.Ptr("prod")}
		if diags := applyReservationRead(ctx, &data, res); diags.HasError() {
			t.Fatalf("unexpected error: %v", diags)
		}
		if data.Tags.Elements()["env"] == nil {
			t.Fatalf("tags = %v", data.Tags)
		}
	})
}

func TestApplyReservation(t *testing.T) {
	ctx := context.Background()

	t.Run("keeps the planned offer fields and location, takes plan_name and the computed values from the response", func(t *testing.T) {
		data := baseReservationModel(t)
		data.ID = armids.IDValue{StringValue: types.StringUnknown()}
		res := apiReservation("a_plan_the_service_spells_differently")
		res.Location = to.Ptr("East US 2")
		if diags := applyReservation(ctx, &data, res); diags.HasError() {
			t.Fatalf("unexpected error: %v", diags)
		}
		offer, _ := decodeReservationOffer(ctx, data.Offer)
		if offer.PlanID.ValueString() != "plan_a" {
			t.Fatalf("plan_id = %q, want the planned one", offer.PlanID.ValueString())
		}
		if offer.PlanName.ValueString() != "Plan a_plan_the_service_spells_differently" {
			t.Fatalf("plan_name = %q", offer.PlanName.ValueString())
		}
		if data.Location.ValueString() != "eastus2" {
			t.Fatalf("location = %q, want the planned one", data.Location.ValueString())
		}
		if data.ID.IsUnknown() || data.InternalID.ValueString() != "internal-seeded" {
			t.Fatal("computed values not resolved")
		}
	})

	t.Run("a response without offer details resolves plan_name to null, never unknown", func(t *testing.T) {
		data := baseReservationModel(t)
		res := apiReservation("plan_a")
		res.Properties.Marketplace.OfferDetails = nil
		if diags := applyReservation(ctx, &data, res); diags.HasError() {
			t.Fatalf("unexpected error: %v", diags)
		}
		offer, _ := decodeReservationOffer(ctx, data.Offer)
		if !offer.PlanName.IsNull() {
			t.Fatalf("plan_name = %v, want null", offer.PlanName)
		}
	})
}

func TestSeedImportedReservation(t *testing.T) {
	parsed, err := armids.ParseReservationID("/subscriptions/s/resourceGroups/rg/providers/PureStorage.Block/reservations/res1")
	if err != nil {
		t.Fatal(err)
	}
	var data ReservationResourceModel
	seedImportedReservation(&data, "the-id", parsed)
	if data.ID.ValueString() != "the-id" || data.Name.ValueString() != "res1" || data.ResourceGroupName.ValueString() != "rg" {
		t.Fatalf("seed = %+v", data)
	}
}

func TestReservationItemFrom(t *testing.T) {
	ctx := context.Background()
	id := "/subscriptions/" + testSubscriptionID + "/resourceGroups/rg-a/providers/PureStorage.Block/reservations/res1"
	full := armpurestorageblock.Reservation{
		ID:       to.Ptr(id),
		Name:     to.Ptr("res1"),
		Location: to.Ptr("East US 2 EUAP"),
		Tags:     map[string]*string{"team": to.Ptr("storage")},
		Properties: &armpurestorageblock.ReservationPropertiesBaseResourceProperties{
			ReservationInternalID: to.Ptr("internal-1"),
			User:                  &armpurestorageblock.UserDetails{FirstName: to.Ptr("Ada"), EmailAddress: to.Ptr("ada@example.com")},
			Marketplace: &armpurestorageblock.MarketplaceDetails{
				SubscriptionID:     to.Ptr("purchase-1"),
				SubscriptionStatus: to.Ptr(armpurestorageblock.MarketplaceSubscriptionStatusSubscribed),
				OfferDetails: &armpurestorageblock.OfferDetails{
					OfferID: to.Ptr("offer_a"), PlanID: to.Ptr("plan_a"), TermUnit: to.Ptr("P1M"), PlanName: to.Ptr("Plan A"),
				},
			},
		},
	}

	t.Run("maps every returned attribute and canonicalises a display-name location", func(t *testing.T) {
		item, diags := reservationItemFrom(ctx, full)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if item.ID.ValueString() != id || item.Name.ValueString() != "res1" || item.Location.ValueString() != "eastus2euap" ||
			item.InternalID.ValueString() != "internal-1" || item.MarketplaceSubscriptionID.ValueString() != "purchase-1" ||
			item.MarketplaceSubscriptionStatus.ValueString() != "Subscribed" {
			t.Errorf("unexpected item: %+v", item)
		}
		var offer ReservationOfferModel
		if diags := item.Offer.As(ctx, &offer, basetypes.ObjectAsOptions{}); diags.HasError() {
			t.Fatalf("offer: %v", diags)
		}
		if offer.OfferID.ValueString() != "offer_a" || offer.PlanID.ValueString() != "plan_a" ||
			offer.TermUnit.ValueString() != "P1M" || offer.PlanName.ValueString() != "Plan A" {
			t.Errorf("unexpected offer: %+v", offer)
		}
		if item.Tags.IsNull() || len(item.Tags.Elements()) != 1 {
			t.Errorf("tags = %v, want the one tag", item.Tags)
		}
	})

	t.Run("a response without offer details gives a null offer", func(t *testing.T) {
		noOffer := full
		props := *full.Properties
		marketplace := *props.Marketplace
		marketplace.OfferDetails = nil
		props.Marketplace = &marketplace
		noOffer.Properties = &props

		item, diags := reservationItemFrom(ctx, noOffer)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if !item.Offer.IsNull() {
			t.Errorf("offer = %v, want null", item.Offer)
		}
	})

	t.Run("a response with no tags gives null tags", func(t *testing.T) {
		untagged := full
		untagged.Tags = nil
		item, _ := reservationItemFrom(ctx, untagged)
		if !item.Tags.IsNull() {
			t.Errorf("tags = %v, want null", item.Tags)
		}
	})

	t.Run("a response with no properties is an error", func(t *testing.T) {
		if _, diags := reservationItemFrom(ctx, armpurestorageblock.Reservation{ID: to.Ptr(id)}); !diags.HasError() {
			t.Error("expected an error")
		}
	})
}

// The data sources must not hand back the contact details the resource keeps out of
// state: no user attribute at any level of either schema.
func TestReservationDataSourcesCarryNoUser(t *testing.T) {
	ctx := context.Background()

	var singular datasource.SchemaResponse
	NewReservationDataSource().Schema(ctx, datasource.SchemaRequest{}, &singular)
	if _, ok := singular.Schema.Attributes["user"]; ok {
		t.Error("everpure_azure_reservation has a user attribute")
	}

	var plural datasource.SchemaResponse
	NewReservationsDataSource().Schema(ctx, datasource.SchemaRequest{}, &plural)
	if _, ok := plural.Schema.Attributes["user"]; ok {
		t.Error("everpure_azure_reservations has a user attribute")
	}
	if _, ok := reservationsElementNestedObject().Attributes["user"]; ok {
		t.Error("everpure_azure_reservations elements have a user attribute")
	}
}
