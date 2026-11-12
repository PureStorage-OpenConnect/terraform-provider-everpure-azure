// SPDX-License-Identifier: Apache-2.0

package provider

// everpure_azure_reservation has no input blocks, so the dynamic-block regression
// covered by TestReservationResourceUnknownAtValidate does not apply to it: its only
// inputs are the two lookup strings. Like the storage pool it is looked up by
// resource_group_name alone, in the provider's own subscription.

import (
	"fmt"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-testing/compare"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

func TestReservationDataSource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	const dsAddr = "data.everpure_azure_reservation.test"

	t.Run("looks up a reservation managed by a resource in the same configuration, and has no user", func(t *testing.T) {
		factories, _ := newReservationTestProviderFactories()

		config := reservationConfig("psc_capacity_perf_ondemand", reservationUserBlock+`  tags = { environment = "test" }
`) + `
data "everpure_azure_reservation" "test" {
  name                = everpure_azure_reservation.test.name
  resource_group_name = everpure_azure_reservation.test.resource_group_name
}
`

		pair := func(attr string) statecheck.StateCheck {
			return statecheck.CompareValuePairs(
				reservationAddr, tfjsonpath.New(attr),
				dsAddr, tfjsonpath.New(attr),
				compare.ValuesSame(),
			)
		}
		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config: config,
				ConfigStateChecks: []statecheck.StateCheck{
					pair("id"), pair("location"), pair("offer"), pair("tags"), pair("internal_id"),
					pair("marketplace_subscription_id"), pair("marketplace_subscription_status"),
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					// The contact details are personal data: nothing of the user the
					// resource sent comes back through the data source.
					resource.TestCheckNoResourceAttr(dsAddr, "user.%"),
					resource.TestCheckNoResourceAttr(dsAddr, "user.first_name"),
					resource.TestCheckNoResourceAttr(dsAddr, "user.email_address"),
				),
			}},
		})
	})

	t.Run("looks up a reservation seeded directly", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()
		store.seed(testResourceGroup, "seeded", fakeSeededReservation("plan_s"))

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_reservation" "test" {
  name                = "seeded"
  resource_group_name = %q
}
`, testResourceGroup),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(dsAddr, "id", store.fakeReservationID(testResourceGroup, "seeded")),
					resource.TestCheckResourceAttr(dsAddr, "location", "eastus2"),
					resource.TestCheckResourceAttr(dsAddr, "internal_id", "internal-seeded"),
					resource.TestCheckResourceAttr(dsAddr, "marketplace_subscription_id", "purchase-seeded"),
					resource.TestCheckResourceAttr(dsAddr, "marketplace_subscription_status", "Subscribed"),
					resource.TestCheckResourceAttr(dsAddr, "offer.offer_id", "offer_a"),
					resource.TestCheckResourceAttr(dsAddr, "offer.plan_id", "plan_s"),
					resource.TestCheckResourceAttr(dsAddr, "offer.term_unit", "P1M"),
					resource.TestCheckResourceAttr(dsAddr, "offer.plan_name", "Plan plan_s"),
					resource.TestCheckNoResourceAttr(dsAddr, "tags.%"),
					resource.TestCheckNoResourceAttr(dsAddr, "user.%"),
				),
			}},
		})
	})

	t.Run("a GET without offer details gives a null offer", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()
		store.seed(testResourceGroup, "seeded", fakeSeededReservation("plan_s"))
		store.setOmitOfferDetails(testResourceGroup, "seeded", true)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_reservation" "test" {
  name                = "seeded"
  resource_group_name = %q
}
`, testResourceGroup),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(dsAddr, tfjsonpath.New("offer"), knownvalue.Null()),
				},
				Check: resource.TestCheckResourceAttr(dsAddr, "internal_id", "internal-seeded"),
			}},
		})
	})

	t.Run("a display-name location is canonicalised", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()
		store.seed(testResourceGroup, "seeded", fakeSeededReservation("plan_s"))
		store.setLocationOverride(testResourceGroup, "seeded", "East US 2 EUAP")

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_reservation" "test" {
  name                = "seeded"
  resource_group_name = %q
}
`, testResourceGroup),
				Check: resource.TestCheckResourceAttr(dsAddr, "location", "eastus2euap"),
			}},
		})
	})

	t.Run("returns tags as a map when the reservation has some", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()
		store.seed(testResourceGroup, "seeded", fakeSeededReservation("plan_s"))
		store.mutate(testResourceGroup, "seeded", func(r *armpurestorageblock.Reservation) {
			r.Tags = map[string]*string{"environment": to.Ptr("prod")}
		})

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_reservation" "test" {
  name                = "seeded"
  resource_group_name = %q
}
`, testResourceGroup),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(dsAddr, "tags.%", "1"),
					resource.TestCheckResourceAttr(dsAddr, "tags.environment", "prod"),
				),
			}},
		})
	})

	t.Run("errors when no reservation matches the name", func(t *testing.T) {
		factories, _ := newReservationTestProviderFactories()
		reservationDSID := fmt.Sprintf(
			"/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/reservations/does-not-exist",
			testSubscriptionID, testResourceGroup)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_reservation" "test" {
  name                = "does-not-exist"
  resource_group_name = %q
}
`, testResourceGroup),
				ExpectError: diagRegexp(operation{action: actionRetrieve, kind: kindReservation, name: "does-not-exist", id: reservationDSID}.fail(newNotFoundError(kindReservation, reservationDSID, &kindResourceGroup, apiFailure{}))),
			}},
		})
	})

	t.Run("succeeds when the configured name differs in case from the reservation's own, and keeps the configured spelling", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()
		store.seed(testResourceGroup, "cased", fakeSeededReservation("plan_s"))

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_reservation" "test" {
  name                = "CASED"
  resource_group_name = %q
}
`, testResourceGroup),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(dsAddr, "name", "CASED"),
					resource.TestCheckResourceAttrSet(dsAddr, "id"),
				),
			}},
		})
	})
}
