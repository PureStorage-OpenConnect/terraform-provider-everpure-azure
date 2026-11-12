// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	tfjson "github.com/hashicorp/terraform-json"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/querycheck"
)

func TestReservationListResource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	const address = "list.everpure_azure_reservation.test"
	const listType = "everpure_azure_reservation"

	seedThree := func(store *fakeReservationStore) {
		seedTestReservation(store, "rg-b", "res-a")
		seedTestReservation(store, "rg-a", "res-z")
		seedTestReservation(store, "rg-a", "res-m")
	}

	t.Run("a resource group scope lists only that group's reservations, sorted by id", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()
		seedThree(store)
		store.setPageSize(1)

		resource.Test(t, queryTestCase(factories, queryStep(listType,
			listBlockConfig("", `    resource_group_name = "rg-a"`),
			querycheck.ExpectLength("everpure_azure_reservation.test", 2),
			expectDisplayNames(address, "res-m", "res-z"),
		)))
	})

	t.Run("no resource group lists the whole subscription, sorted by id across groups and pages", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()
		seedThree(store)
		store.setPageSize(1)

		resource.Test(t, queryTestCase(factories, queryStep(listType, listBlockConfig("", ""),
			querycheck.ExpectLength("everpure_azure_reservation.test", 3),
			expectDisplayNames(address, "res-m", "res-z", "res-a"),
			queryResults{address: address, fn: func(results []tfjson.ListResourceFoundData) error {
				want := map[string]any{
					"subscription_id":     testSubscriptionID,
					"resource_group_name": "rg-a",
					"name":                "res-m",
				}
				if fmt.Sprint(results[0].Identity) != fmt.Sprint(want) {
					return fmt.Errorf("first identity = %v, want %v", results[0].Identity, want)
				}
				if results[0].ResourceObject != nil {
					return fmt.Errorf("resource object present without include_resource: %v", results[0].ResourceObject)
				}
				return nil
			}},
		)))
	})

	t.Run("an empty scope gives zero results and no error", func(t *testing.T) {
		factories, _ := newReservationTestProviderFactories()

		resource.Test(t, queryTestCase(factories,
			queryStep(listType, listBlockConfig("", ""), querycheck.ExpectLength("everpure_azure_reservation.test", 0)),
			queryStep(listType, listBlockConfig("", `    resource_group_name = "rg-none"`), querycheck.ExpectLength("everpure_azure_reservation.test", 0)),
		))
	})

	t.Run("include_resource returns the resource object as import and Read would leave it, with user null and offer filled", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()
		seedTestReservation(store, "rg-a", "res-m")
		store.mutate("rg-a", "res-m", func(r *armpurestorageblock.Reservation) {
			r.Tags = map[string]*string{"team": to.Ptr("storage")}
		})
		store.setLocationOverride("rg-a", "res-m", "East US 2 EUAP")

		resource.Test(t, queryTestCase(factories, queryStep(listType,
			"  include_resource = true\n"+listBlockConfig("", `    resource_group_name = "rg-a"`),
			queryResults{address: address, fn: func(results []tfjson.ListResourceFoundData) error {
				if len(results) != 1 {
					return fmt.Errorf("got %d results, want 1", len(results))
				}
				got := results[0].ResourceObject
				if got["id"] != store.fakeReservationID("rg-a", "res-m") || got["name"] != "res-m" ||
					got["resource_group_name"] != "rg-a" || got["location"] != "eastus2euap" ||
					got["internal_id"] != "internal-seeded" {
					return fmt.Errorf("unexpected resource object: %v", got)
				}
				if got["user"] != nil {
					return fmt.Errorf("user = %v, want null", got["user"])
				}
				offer, _ := got["offer"].(map[string]any)
				if fmt.Sprintf("%v|%v|%v|%v", offer["offer_id"], offer["plan_id"], offer["term_unit"], offer["plan_name"]) != "offer_a|plan_res-m|P1M|Plan plan_res-m" {
					return fmt.Errorf("offer = %v", got["offer"])
				}
				if tags, _ := got["tags"].(map[string]any); fmt.Sprint(tags) != "map[team:storage]" {
					return fmt.Errorf("tags = %v", got["tags"])
				}
				return nil
			}},
		)))
	})

	t.Run("limit caps the results after sorting", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()
		seedThree(store)

		resource.Test(t, queryTestCase(factories, queryStep(listType,
			"  limit = 2\n"+listBlockConfig("", `    resource_group_name = "rg-a"`),
			expectDisplayNames(address, "res-m", "res-z"),
		), queryStep(listType,
			"  limit = 2\n"+listBlockConfig("", ""),
			expectDisplayNames(address, "res-m", "res-z"),
		)))
	})

	t.Run("a reservation with no properties fails the query", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()
		seedThree(store)
		store.mutate("rg-a", "res-z", func(r *armpurestorageblock.Reservation) { r.Properties = nil })

		resource.Test(t, queryTestCase(factories, queryErrorStep(listType, listBlockConfig("", ""),
			diagRegexp(unexpectedResponseDiagnostic(newNoPropertiesError(kindReservation))))))
	})

	t.Run("a reservation with no id fails the query", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()
		seedThree(store)
		store.mutate("rg-a", "res-z", func(r *armpurestorageblock.Reservation) { r.ID = nil })

		resource.Test(t, queryTestCase(factories, queryErrorStep(listType, listBlockConfig("", ""),
			diagRegexp(unexpectedResponseDiagnostic(newMissingResponseIDError(kindReservation))))))
	})

	t.Run("a timeouts block is accepted", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()
		seedThree(store)

		resource.Test(t, queryTestCase(factories, queryStep(listType,
			listBlockConfig("", "    timeouts {\n      list = \"2m\"\n    }"),
			querycheck.ExpectLength("everpure_azure_reservation.test", 3),
		)))
	})

	t.Run("an unparseable timeout is rejected", func(t *testing.T) {
		factories, _ := newReservationTestProviderFactories()

		resource.Test(t, queryTestCase(factories, queryErrorStep(listType,
			listBlockConfig("", "    timeouts {\n      list = \"soon\"\n    }"),
			regexp.MustCompile(`(?s)Invalid Attribute Value.*"soon"`))))
	})
}
