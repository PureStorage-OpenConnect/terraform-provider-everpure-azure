// SPDX-License-Identifier: Apache-2.0

package provider

// everpure_azure_reservations has no input blocks, so the dynamic-block regression does
// not apply here. Like the storage pools data source it has no parent ID to validate:
// reservations are scoped by an optional resource_group_name (or the whole
// subscription), so an empty or nonexistent resource group is just an empty list.

import (
	"fmt"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func seedTestReservation(store *fakeReservationStore, resourceGroupName, name string) {
	store.seed(resourceGroupName, name, fakeSeededReservation("plan_"+name))
}

func TestReservationsDataSource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	const dsAddr = "data.everpure_azure_reservations."

	t.Run("an empty resource group gives an empty list", func(t *testing.T) {
		factories, _ := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_reservations" "test" {
  resource_group_name = %q
}
`, testResourceGroup),
				Check: resource.TestCheckResourceAttr(dsAddr+"test", "reservations.#", "0"),
			}},
		})
	})

	t.Run("an empty subscription (resource_group_name omitted) gives an empty list", func(t *testing.T) {
		factories, _ := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config: providerBlock() + `
data "everpure_azure_reservations" "test" {}
`,
				Check: resource.TestCheckResourceAttr(dsAddr+"test", "reservations.#", "0"),
			}},
		})
	})

	t.Run("keeps a reservation with no properties, and drops one with no id, without failing the list", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()
		seedTestReservation(store, testResourceGroup, "res-a")
		seedTestReservation(store, testResourceGroup, "res-b")
		seedTestReservation(store, testResourceGroup, "res-c")
		store.mutate(testResourceGroup, "res-b", func(r *armpurestorageblock.Reservation) { r.Properties = nil })
		store.mutate(testResourceGroup, "res-c", func(r *armpurestorageblock.Reservation) { r.ID = nil })

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_reservations" "test" {
  resource_group_name = %q
}
`, testResourceGroup),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(dsAddr+"test", "reservations.#", "2"),
					resource.TestCheckResourceAttr(dsAddr+"test", "reservations.0.name", "res-a"),
					resource.TestCheckResourceAttr(dsAddr+"test", "reservations.0.offer.plan_id", "plan_res-a"),
					resource.TestCheckResourceAttr(dsAddr+"test", "reservations.1.name", "res-b"),
					resource.TestCheckResourceAttr(dsAddr+"test", "reservations.1.location", "eastus2"),
					resource.TestCheckResourceAttr(dsAddr+"test", "reservations.1.resource_group_name", testResourceGroup),
					resource.TestCheckNoResourceAttr(dsAddr+"test", "reservations.1.offer.plan_id"),
					resource.TestCheckNoResourceAttr(dsAddr+"test", "reservations.1.internal_id"),
				),
			}},
		})
	})

	t.Run("lists across two resource groups, sorted by id, over a multi-page response", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()
		seedTestReservation(store, testResourceGroup, "res-c")
		seedTestReservation(store, testResourceGroup, "res-a")
		seedTestReservation(store, "rg-other", "res-b")
		store.setPageSize(1) // forces the pager loop to drain more than one page

		// The subscription-wide list orders by full ARM ID, which puts the resource
		// group name ahead of the reservation name ("rg-other" sorts before "rg-test"),
		// so res-b sorts ahead of both rg-test reservations.
		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_reservations" "scoped" {
  resource_group_name = %q
}

data "everpure_azure_reservations" "all" {}
`, testResourceGroup),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(dsAddr+"scoped", "reservations.#", "2"),
					resource.TestCheckResourceAttr(dsAddr+"scoped", "reservations.0.name", "res-a"),
					resource.TestCheckResourceAttr(dsAddr+"scoped", "reservations.1.name", "res-c"),
					resource.TestCheckResourceAttr(dsAddr+"scoped", "reservations.0.resource_group_name", testResourceGroup),

					resource.TestCheckResourceAttr(dsAddr+"all", "reservations.#", "3"),
					resource.TestCheckResourceAttr(dsAddr+"all", "reservations.0.name", "res-b"),
					resource.TestCheckResourceAttr(dsAddr+"all", "reservations.0.resource_group_name", "rg-other"),
					resource.TestCheckResourceAttr(dsAddr+"all", "reservations.1.name", "res-a"),
					resource.TestCheckResourceAttr(dsAddr+"all", "reservations.1.resource_group_name", testResourceGroup),
					resource.TestCheckResourceAttr(dsAddr+"all", "reservations.2.name", "res-c"),
					resource.TestCheckNoResourceAttr(dsAddr+"all", "reservations.0.user.%"),
				),
			}},
		})
	})

	t.Run("a display-name location is canonicalised in a list element", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()
		seedTestReservation(store, testResourceGroup, "res-a")
		store.setLocationOverride(testResourceGroup, "res-a", "East US 2 EUAP")

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config: providerBlock() + `
data "everpure_azure_reservations" "test" {}
`,
				Check: resource.TestCheckResourceAttr(dsAddr+"test", "reservations.0.location", "eastus2euap"),
			}},
		})
	})

	t.Run("a reservation without offer details has a null offer", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()
		seedTestReservation(store, testResourceGroup, "res-a")
		store.setOmitOfferDetails(testResourceGroup, "res-a", true)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config: providerBlock() + `
data "everpure_azure_reservations" "test" {}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(dsAddr+"test", "reservations.#", "1"),
					resource.TestCheckNoResourceAttr(dsAddr+"test", "reservations.0.offer.offer_id"),
				),
			}},
		})
	})

	t.Run("offer details are omitted per reservation, not for the whole list", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()
		seedTestReservation(store, testResourceGroup, "res-a")
		seedTestReservation(store, testResourceGroup, "res-b")
		store.setOmitOfferDetails(testResourceGroup, "res-b", true)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config: providerBlock() + fmt.Sprintf(`
data "everpure_azure_reservations" "test" {
  resource_group_name = %q
}
`, testResourceGroup),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(dsAddr+"test", "reservations.#", "2"),
					resource.TestCheckResourceAttr(dsAddr+"test", "reservations.0.name", "res-a"),
					resource.TestCheckResourceAttr(dsAddr+"test", "reservations.0.offer.plan_id", "plan_res-a"),
					resource.TestCheckResourceAttr(dsAddr+"test", "reservations.1.name", "res-b"),
					resource.TestCheckNoResourceAttr(dsAddr+"test", "reservations.1.offer.plan_id"),
				),
			}},
		})
	})

	t.Run("split ownership: a list element matches the resource that manages it", func(t *testing.T) {
		factories, _ := newReservationTestProviderFactories()

		config := reservationConfig("psc_capacity_perf_ondemand", reservationUserBlock) + `
data "everpure_azure_reservations" "test" {
  resource_group_name = everpure_azure_reservation.test.resource_group_name
}
`
		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(dsAddr+"test", "reservations.#", "1"),
					resource.TestCheckResourceAttrPair(dsAddr+"test", "reservations.0.id", reservationAddr, "id"),
					resource.TestCheckResourceAttrPair(dsAddr+"test", "reservations.0.internal_id", reservationAddr, "internal_id"),
					resource.TestCheckResourceAttrPair(dsAddr+"test", "reservations.0.offer.plan_id", reservationAddr, "offer.plan_id"),
					resource.TestCheckNoResourceAttr(dsAddr+"test", "reservations.0.user.%"),
				),
			}},
		})
	})
}
