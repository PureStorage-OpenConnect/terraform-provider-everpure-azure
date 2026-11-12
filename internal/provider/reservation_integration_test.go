// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azcorefake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	psbfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock/fake"
	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/config"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

const reservationAddr = "everpure_azure_reservation.test"

// newReservationTestProviderFactories builds an isolated provider instance wired to a
// fresh in-memory fake reservations server.
func newReservationTestProviderFactories() (map[string]func() (tfprotov6.ProviderServer, error), *fakeReservationStore) {
	store := newFakeReservationStore(testSubscriptionID)
	srv := store.newReservationsServer()
	opts := &arm.ClientOptions{
		ClientOptions: azcore.ClientOptions{Transport: psbfake.NewReservationsServerTransport(&srv)},
	}
	p := newProvider("test", fixedCredentialFactory(&azcorefake.TokenCredential{}), opts)()
	return map[string]func() (tfprotov6.ProviderServer, error){
		"everpure": providerserver.NewProtocol6WithError(p),
	}, store
}

const reservationUserBlock = `
  user = {
    first_name    = "Ada"
    last_name     = "Lovelace"
    email_address = "ada@example.com"
    company = {
      company_name = "Analytical Engines"
      address = {
        address_line1 = "1 Main Street"
        city          = "London"
        state         = ""
        country       = "UK"
        postal_code   = "N1 1AA"
      }
    }
  }
`

const reservationEditedUserBlock = `
  user = {
    first_name    = "Grace"
    last_name     = "Hopper"
    email_address = "grace@example.com"
  }
`

// reservationConfig renders a reservation named "res1". extra is spliced into the
// resource body: a user block, tags, or both.
func reservationConfig(planID, extra string) string {
	return providerBlock() + fmt.Sprintf(`
resource "everpure_azure_reservation" "test" {
  name                = "res1"
  resource_group_name = %[1]q
  location            = "eastus2"

  offer = {
    offer_id  = "offer_a"
    plan_id   = %[2]q
    term_unit = "P1M"
  }
%[3]s}
`, testResourceGroup, planID, extra)
}

// otherSubscriptionReservationID is a reservation ID in testOtherSubscriptionID.
const otherSubscriptionReservationID = "/subscriptions/" + testOtherSubscriptionID + "/resourceGroups/rg-test/providers/PureStorage.Block/reservations/res1"

// reservationOperation is the operation of action on the test reservation, as the resource
// reports it.
func reservationOperation(action verb) operation {
	return operation{action: action, kind: kindReservation, name: "res1", id: reservationAPIID()}
}

func reservationAPIID() string {
	return fmt.Sprintf(
		"/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/reservations/res1",
		testSubscriptionID, testResourceGroup,
	)
}

func TestReservationResource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	const plan1, plan2 = "psc_capacity_perf_ondemand", "psc_capacity_perf_1year"

	t.Run("creates a reservation: sends the configured user and a fixed publisher, never a term id, and keeps the user out of state", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config: reservationConfig(plan1, reservationUserBlock+`  tags = { environment = "test" }
`),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(reservationAddr, tfjsonpath.New("user"), knownvalue.Null()),
					statecheck.ExpectKnownValue(reservationAddr, tfjsonpath.New("id"), knownvalue.StringExact(reservationAPIID())),
					statecheck.ExpectKnownValue(reservationAddr, tfjsonpath.New("internal_id"), knownvalue.StringExact("internal-res1")),
					statecheck.ExpectKnownValue(reservationAddr, tfjsonpath.New("marketplace_subscription_id"), knownvalue.StringExact("purchase-res1")),
					statecheck.ExpectKnownValue(reservationAddr, tfjsonpath.New("marketplace_subscription_status"), knownvalue.StringExact("PendingFulfillmentStart")),
					statecheck.ExpectKnownValue(reservationAddr, tfjsonpath.New("offer").AtMapKey("plan_name"), knownvalue.StringExact("Plan "+plan1)),
					statecheck.ExpectKnownValue(reservationAddr, tfjsonpath.New("offer").AtMapKey("term_unit"), knownvalue.StringExact("P1M")),
					statecheck.ExpectKnownValue(reservationAddr, tfjsonpath.New("tags").AtMapKey("environment"), knownvalue.StringExact("test")),
				},
				Check: func(_ *terraform.State) error {
					body := store.putBody(0)
					props := body.Properties
					offer := props.Marketplace.OfferDetails
					if derefString(offer.PublisherID) != reservationPublisherID {
						return fmt.Errorf("publisherId = %q, want %q", derefString(offer.PublisherID), reservationPublisherID)
					}
					if offer.TermID != nil {
						return fmt.Errorf("termId = %q, want it never sent", *offer.TermID)
					}
					if derefString(offer.OfferID) != "offer_a" || derefString(offer.PlanID) != plan1 || derefString(offer.TermUnit) != "P1M" {
						return fmt.Errorf("offer details = %+v", *offer)
					}
					u := props.User
					if u == nil || derefString(u.FirstName) != "Ada" || derefString(u.EmailAddress) != "ada@example.com" {
						return fmt.Errorf("user sent = %+v, want the configured one", u)
					}
					if u.CompanyDetails == nil || derefString(u.CompanyDetails.CompanyName) != "Analytical Engines" ||
						u.CompanyDetails.Address == nil || derefString(u.CompanyDetails.Address.PostalCode) != "N1 1AA" ||
						derefString(u.CompanyDetails.Address.State) != "" {
						return fmt.Errorf("company details sent = %+v", u.CompanyDetails)
					}
					if derefString(body.Location) != "eastus2" {
						return fmt.Errorf("location = %q", derefString(body.Location))
					}
					return nil
				},
			}},
		})
	})

	t.Run("creates a reservation without user, as an offer that needs none allows", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config: reservationConfig(plan1, ""),
				Check: func(_ *terraform.State) error {
					if store.putBody(0).Properties.User != nil {
						return errors.New("a user was sent without one in the configuration")
					}
					return nil
				},
			}},
		})
	})

	t.Run("an edit to user plans no change and sends nothing", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: reservationConfig(plan1, reservationUserBlock)},
				{Config: reservationConfig(plan1, reservationEditedUserBlock), PlanOnly: true},
				{Config: reservationConfig(plan1, ""), PlanOnly: true},
				{
					Config: reservationConfig(plan1, reservationEditedUserBlock),
					Check: func(_ *terraform.State) error {
						if store.putCount() != 1 || store.patchCount() != 0 {
							return fmt.Errorf("puts = %d, patches = %d, want only the create", store.putCount(), store.patchCount())
						}
						return nil
					},
				},
			},
		})
	})

	t.Run("reads a display-name location from the service as the canonical one", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: reservationConfig(plan1, "")},
				{
					PreConfig: func() { store.setLocationOverride(testResourceGroup, "res1", "East US 2") },
					Config:    reservationConfig(plan1, ""),
					PlanOnly:  true,
				},
			},
		})
	})

	t.Run("changes tags in place with a PATCH, without a PUT", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: reservationConfig(plan1, `  tags = { environment = "test" }
`)},
				{
					Config: reservationConfig(plan1, `  tags = { environment = "prod" }
`),
					ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(reservationAddr, plancheck.ResourceActionUpdate),
					}},
					ConfigStateChecks: []statecheck.StateCheck{
						statecheck.ExpectKnownValue(reservationAddr, tfjsonpath.New("tags").AtMapKey("environment"), knownvalue.StringExact("prod")),
					},
					Check: func(_ *terraform.State) error {
						if store.putCount() != 1 || store.patchCount() != 1 {
							return fmt.Errorf("puts = %d, patches = %d, want 1 and 1", store.putCount(), store.patchCount())
						}
						return nil
					},
				},
				{
					// Removing the tags clears them: the PATCH sends an empty map, and
					// nothing but tags.
					Config: reservationConfig(plan1, ""),
					Check: func(_ *terraform.State) error {
						if store.patchCount() != 2 {
							return fmt.Errorf("patches = %d, want 2", store.patchCount())
						}
						body := store.patchBody(1)
						if body.Tags == nil || len(body.Tags) != 0 {
							return fmt.Errorf("PATCH tags = %#v, want a non-nil empty map", body.Tags)
						}
						if body.Properties != nil {
							return fmt.Errorf("PATCH properties = %+v, want none (no user)", body.Properties)
						}
						return nil
					},
				},
			},
		})
	})

	t.Run("a plan_id change re-PUTs the whole reservation and echoes the user the service holds, not the configured one", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: reservationConfig(plan1, reservationUserBlock+`  tags = { environment = "test" }
`)},
				{
					// The configuration's user differs from what the service holds; the
					// re-PUT must carry the service's.
					Config: reservationConfig(plan2, reservationEditedUserBlock+`  tags = { environment = "test" }
`),
					ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(reservationAddr, plancheck.ResourceActionUpdate),
						plancheck.ExpectUnknownValue(reservationAddr, tfjsonpath.New("offer").AtMapKey("plan_name")),
					}},
					ConfigStateChecks: []statecheck.StateCheck{
						statecheck.ExpectKnownValue(reservationAddr, tfjsonpath.New("offer").AtMapKey("plan_id"), knownvalue.StringExact(plan2)),
						statecheck.ExpectKnownValue(reservationAddr, tfjsonpath.New("offer").AtMapKey("plan_name"), knownvalue.StringExact("Plan "+plan2)),
						statecheck.ExpectKnownValue(reservationAddr, tfjsonpath.New("user"), knownvalue.Null()),
					},
					Check: func(_ *terraform.State) error {
						if store.putCount() != 2 || store.patchCount() != 0 {
							return fmt.Errorf("puts = %d, patches = %d, want 2 and 0", store.putCount(), store.patchCount())
						}
						body := store.putBody(1)
						offer := body.Properties.Marketplace.OfferDetails
						if derefString(offer.PlanID) != plan2 || derefString(offer.OfferID) != "offer_a" ||
							derefString(offer.TermUnit) != "P1M" || derefString(offer.PublisherID) != reservationPublisherID || offer.TermID != nil {
							return fmt.Errorf("re-PUT offer details = %+v", *offer)
						}
						u := body.Properties.User
						if u == nil || derefString(u.FirstName) != "Ada" || u.CompanyDetails == nil {
							return fmt.Errorf("re-PUT user = %+v, want the one the service holds (Ada, with company)", u)
						}
						if derefString(body.Location) != "eastus2" || derefString(body.Tags["environment"]) != "test" {
							return fmt.Errorf("re-PUT location/tags = %q / %v", derefString(body.Location), body.Tags)
						}
						return nil
					},
				},
			},
		})
	})

	t.Run("a plan_id re-PUT is nil-safe when the service holds no user", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: reservationConfig(plan1, "")},
				{
					Config: reservationConfig(plan2, ""),
					Check: func(_ *terraform.State) error {
						if store.putCount() != 2 || store.putBody(1).Properties.User != nil {
							return fmt.Errorf("puts = %d, user = %+v", store.putCount(), store.putBody(1).Properties.User)
						}
						return nil
					},
				},
			},
		})
	})

	t.Run("tags and plan_id changed in one apply are a single PUT and no PATCH", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: reservationConfig(plan1, `  tags = { environment = "test" }
`)},
				{
					Config: reservationConfig(plan2, `  tags = { environment = "prod" }
`),
					ConfigStateChecks: []statecheck.StateCheck{
						statecheck.ExpectKnownValue(reservationAddr, tfjsonpath.New("tags").AtMapKey("environment"), knownvalue.StringExact("prod")),
						statecheck.ExpectKnownValue(reservationAddr, tfjsonpath.New("offer").AtMapKey("plan_id"), knownvalue.StringExact(plan2)),
					},
					Check: func(_ *terraform.State) error {
						if store.putCount() != 2 || store.patchCount() != 0 {
							return fmt.Errorf("puts = %d, patches = %d, want 2 and 0", store.putCount(), store.patchCount())
						}
						if derefString(store.putBody(1).Tags["environment"]) != "prod" {
							return fmt.Errorf("re-PUT tags = %v", store.putBody(1).Tags)
						}
						return nil
					},
				},
			},
		})
	})

	t.Run("a change to the plan_id the service rejects fails with the hint about the SaaS resource", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: reservationConfig(plan1, "")},
				{
					PreConfig:   func() { store.failNextPut(testResourceGroup, "res1", fakeFailure{status: 400, code: "BadRequest"}) },
					Config:      reservationConfig(plan2, ""),
					ExpectError: diagRegexp(reservationOperation(actionUpdate).fail(newReservationPlanChangeFailedError(reservationAPIID(), apiFailure{}))),
				},
			},
		})
	})

	t.Run("imports by full ID, filling offer from the service, and plans clean with or without user in the configuration", func(t *testing.T) {
		for name, extra := range map[string]string{
			"without user": "",
			"with user":    reservationUserBlock,
		} {
			t.Run(name, func(t *testing.T) {
				factories, store := newReservationTestProviderFactories()
				store.seed(testResourceGroup, "res1", fakeSeededReservation(plan1))

				resource.Test(t, resource.TestCase{
					IsUnitTest:               true,
					ProtoV6ProviderFactories: factories,
					Steps: []resource.TestStep{
						{
							Config:             reservationConfig(plan1, extra),
							ResourceName:       reservationAddr,
							ImportState:        true,
							ImportStatePersist: true,
							ImportStateId:      reservationAPIID(),
						},
						{
							Config:   reservationConfig(plan1, extra),
							PlanOnly: true,
						},
					},
				})
			})
		}
	})

	t.Run("imports a reservation whose GET has no offer details, leaving offer null", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()
		store.seed(testResourceGroup, "res1", fakeSeededReservation(plan1))
		store.setOmitOfferDetails(testResourceGroup, "res1", true)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config:        reservationConfig(plan1, ""),
				ResourceName:  reservationAddr,
				ImportState:   true,
				ImportStateId: reservationAPIID(),
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("imported %d instances, want 1", len(states))
					}
					attrs := states[0].Attributes
					if attrs["id"] != reservationAPIID() {
						return fmt.Errorf("id = %q, want %q", attrs["id"], reservationAPIID())
					}
					for k := range attrs {
						if strings.HasPrefix(k, "offer.") {
							return fmt.Errorf("offer attribute %q is set, want offer null", k)
						}
					}
					return nil
				},
			}},
		})
	})

	t.Run("import verifies against an applied reservation", func(t *testing.T) {
		factories, _ := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: reservationConfig(plan1, reservationUserBlock)},
				{
					Config:            reservationConfig(plan1, reservationUserBlock),
					ResourceName:      reservationAddr,
					ImportState:       true,
					ImportStateVerify: true,
					ImportStateId:     reservationAPIID(),
				},
			},
		})
	})

	t.Run("rejects an import ID from a different subscription", func(t *testing.T) {
		factories, _ := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: reservationConfig(plan1, "")},
				{
					Config:        reservationConfig(plan1, ""),
					ResourceName:  reservationAddr,
					ImportState:   true,
					ImportStateId: otherSubscriptionReservationID,
					ExpectError:   subscriptionMismatchRegexp(kindReservation, otherSubscriptionReservationID),
				},
			},
		})
	})

	t.Run("a reservation deleted outside Terraform is removed from state and planned for creation", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: reservationConfig(plan1, "")},
				{
					PreConfig:          func() { store.remove(testResourceGroup, "res1") },
					Config:             reservationConfig(plan1, ""),
					PlanOnly:           true,
					ExpectNonEmptyPlan: true,
				},
			},
		})
	})

	t.Run("a GET without offer details keeps the prior offer and plans clean", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: reservationConfig(plan1, "")},
				{
					PreConfig: func() { store.setOmitOfferDetails(testResourceGroup, "res1", true) },
					Config:    reservationConfig(plan1, ""),
					PlanOnly:  true,
				},
				{
					Config: reservationConfig(plan1, ""),
					ConfigStateChecks: []statecheck.StateCheck{
						statecheck.ExpectKnownValue(reservationAddr, tfjsonpath.New("offer").AtMapKey("offer_id"), knownvalue.StringExact("offer_a")),
					},
				},
			},
		})
	})

	t.Run("offer guard: a server-side change", func(t *testing.T) {
		tests := []struct {
			name   string
			mutate func(*armpurestorageblock.OfferDetails)
			expect string
		}{
			{
				// offer_id alone: the message must name offer_id and not term_unit as
				// the changed field, which pins how the changed fields are joined.
				name:   "to offer_id alone names the current offer",
				mutate: func(o *armpurestorageblock.OfferDetails) { o.OfferID = to.Ptr("new_offer") },
				expect: `(?s)Invalid\s+Attribute\s+Value.*\soffer\.offer_id\s+of\s+reservation.*offer_id\s+"new_offer",\s+plan_id\s+"` + plan1 + `",\s+term_unit\s+"P1M".*ignore_changes\s+=\s+\[offer\]`,
			},
			{
				name:   "to term_unit alone names the current term",
				mutate: func(o *armpurestorageblock.OfferDetails) { o.TermUnit = to.Ptr("P1Y") },
				expect: `(?s)\soffer\.term_unit\s+of\s+reservation.*offer_id\s+"offer_a",\s+plan_id\s+"` + plan1 + `",\s+term_unit\s+"P1Y"`,
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				factories, store := newReservationTestProviderFactories()

				resource.Test(t, resource.TestCase{
					IsUnitTest:               true,
					ProtoV6ProviderFactories: factories,
					Steps: []resource.TestStep{
						{Config: reservationConfig(plan1, "")},
						{
							PreConfig: func() {
								store.mutate(testResourceGroup, "res1", func(r *armpurestorageblock.Reservation) {
									tt.mutate(r.Properties.Marketplace.OfferDetails)
								})
							},
							Config:   reservationConfig(plan1, ""),
							PlanOnly: true,
							// The error wraps at 78 columns; the pattern collapses whitespace.
							ExpectError: regexp.MustCompile(tt.expect),
						},
					},
				})
			})
		}
	})

	t.Run("offer guard: ignore_changes on offer lets a server-side offer change pass", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()
		ignoring := reservationConfig(plan1, `
  lifecycle {
    ignore_changes = [offer]
  }
`)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: ignoring},
				{
					PreConfig: func() {
						store.mutate(testResourceGroup, "res1", func(r *armpurestorageblock.Reservation) {
							r.Properties.Marketplace.OfferDetails.OfferID = to.Ptr("new_offer")
						})
					},
					Config:   ignoring,
					PlanOnly: true,
				},
			},
		})
	})

	t.Run("offer guard: a configured change is a plan error", func(t *testing.T) {
		tests := []struct {
			name, offerID, termUnit, expect string
		}{
			{"offer_id", "offer_b", "P1M", `(?s)\soffer\.offer_id\s+of\s+reservation.*offer_id\s+"offer_a"`},
			{"term_unit", "offer_a", "P1Y", `(?s)offer\.term_unit.*term_unit\s+"P1M"`},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				factories, _ := newReservationTestProviderFactories()
				edited := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_reservation" "test" {
  name                = "res1"
  resource_group_name = %[1]q
  location            = "eastus2"

  offer = {
    offer_id  = %[2]q
    plan_id   = %[3]q
    term_unit = %[4]q
  }
}
`, testResourceGroup, tt.offerID, plan1, tt.termUnit)

				resource.Test(t, resource.TestCase{
					IsUnitTest:               true,
					ProtoV6ProviderFactories: factories,
					Steps: []resource.TestStep{
						{Config: reservationConfig(plan1, "")},
						{
							Config:      edited,
							PlanOnly:    true,
							ExpectError: regexp.MustCompile(tt.expect),
						},
					},
				})
			})
		}
	})

	t.Run("offer guard: a plan_id changed by the service alone is an in-place re-PUT back to the configured plan", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: reservationConfig(plan1, "")},
				{
					PreConfig: func() {
						store.mutate(testResourceGroup, "res1", func(r *armpurestorageblock.Reservation) {
							r.Properties.Marketplace.OfferDetails.PlanID = to.Ptr("other_plan")
						})
					},
					Config: reservationConfig(plan1, ""),
					ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(reservationAddr, plancheck.ResourceActionUpdate),
					}},
					Check: func(_ *terraform.State) error {
						if store.putCount() != 2 || derefString(store.putBody(1).Properties.Marketplace.OfferDetails.PlanID) != plan1 {
							return fmt.Errorf("puts = %d, want a re-PUT of %q", store.putCount(), plan1)
						}
						return nil
					},
				},
			},
		})
	})

	t.Run("a delete that answers 404 succeeds and leaves nothing in state", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: reservationConfig(plan1, "")},
				{
					PreConfig: func() {
						store.failNextDelete(testResourceGroup, "res1", fakeFailure{status: 404, code: "NotFound"})
					},
					Config: providerBlock(),
					Check: func(s *terraform.State) error {
						if _, ok := s.RootModule().Resources[reservationAddr]; ok {
							return errors.New("the reservation is still in state after a delete that answered 404")
						}
						return nil
					},
				},
			},
		})
	})

	t.Run("destroys the reservation", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: reservationConfig(plan1, "")},
				{Config: providerBlock(), Check: func(_ *terraform.State) error {
					if store.has(testResourceGroup, "res1") {
						return errors.New("the reservation still exists after destroy")
					}
					return nil
				}},
			},
		})
	})

	t.Run("the delete guard's 400 names the storage pools", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: reservationConfig(plan1, "")},
				{
					PreConfig: func() {
						store.failNextDelete(testResourceGroup, "res1", fakeFailure{status: 400, code: errCodeResourceDeletionValidateFailed})
					},
					Config:      providerBlock(),
					ExpectError: diagRegexp(reservationOperation(actionDelete).fail(newReservationHasStoragePoolsError(reservationAPIID(), apiFailure{}))),
				},
			},
		})
	})

	t.Run("a Forbidden delete surfaces the backend error unchanged", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: reservationConfig(plan1, "")},
				{
					PreConfig: func() {
						store.failNextDelete(testResourceGroup, "res1", fakeFailure{status: 403, code: "Forbidden"})
					},
					Config:      providerBlock(),
					ExpectError: regexp.MustCompile(`(?s)` + summaryRegexp(reservationOperation(actionDelete).fail(detailError{})).String() + `.*Forbidden`),
				},
			},
		})
	})

	t.Run("rejects invalid configuration at plan time", func(t *testing.T) {
		tests := []struct {
			name, resName, location, expect string
		}{
			{"an invalid name", "bad name", "eastus2", "must be 1-127 characters"},
			{"a display-name location", "res1", "East US 2", "canonical short region name"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				factories, _ := newReservationTestProviderFactories()
				cfg := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_reservation" "test" {
  name                = %[2]q
  resource_group_name = %[1]q
  location            = %[3]q

  offer = {
    offer_id  = "offer_a"
    plan_id   = "p"
    term_unit = "P1M"
  }
}
`, testResourceGroup, tt.resName, tt.location)
				resource.Test(t, resource.TestCase{
					IsUnitTest:               true,
					ProtoV6ProviderFactories: factories,
					Steps: []resource.TestStep{{
						Config:      cfg,
						PlanOnly:    true,
						ExpectError: regexp.MustCompile(`(?s)` + tt.expect),
					}},
				})
			})
		}
	})

	t.Run("rejects a term_unit outside the duration grammar", func(t *testing.T) {
		for _, bad := range []string{"1M", "P", "PT1H", "monthly"} {
			t.Run(bad, func(t *testing.T) {
				factories, _ := newReservationTestProviderFactories()
				cfg := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_reservation" "test" {
  name                = "res1"
  resource_group_name = %[1]q
  location            = "eastus2"

  offer = {
    offer_id  = "offer_a"
    plan_id   = "p"
    term_unit = %[2]q
  }
}
`, testResourceGroup, bad)
				resource.Test(t, resource.TestCase{
					IsUnitTest:               true,
					ProtoV6ProviderFactories: factories,
					Steps: []resource.TestStep{{
						Config:      cfg,
						PlanOnly:    true,
						ExpectError: regexp.MustCompile(`(?s)(ISO\s+8601|string\s+length)`),
					}},
				})
			})
		}
	})

	t.Run("accepts custom timeouts alongside the resource's own attributes", func(t *testing.T) {
		factories, _ := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config: reservationConfig(plan1, `
  timeouts {
    create = "5m"
  }
`),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(reservationAddr, tfjsonpath.New("timeouts").AtMapKey("create"), knownvalue.StringExact("5m")),
				},
			}},
		})
	})
}

// TestReservationResourceUnknownAtValidate covers offer and user values that Terraform
// cannot resolve while it validates the configuration (a variable, or a value another
// resource produces). Both are nested attributes held as types.Object in the model, and
// a bare Go struct there would fail with "Received unknown value, however the target
// type cannot handle unknown values". The dynamic-block case every block gets has no
// counterpart for a nested attribute except these.
func TestReservationResourceUnknownAtValidate(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	variableConfig := providerBlock() + fmt.Sprintf(`
variable "offer" {
  type = object({
    offer_id  = string
    plan_id   = string
    term_unit = string
  })
}

variable "user" {
  type = object({
    first_name    = string
    last_name     = string
    email_address = string
    upn           = optional(string)
    phone_number  = optional(string)
    company = optional(object({
      company_name = string
      address = optional(object({
        address_line1 = string
        address_line2 = optional(string)
        city          = string
        state         = string
        country       = string
        postal_code   = string
      }))
    }))
  })
  default = null
}

resource "everpure_azure_reservation" "test" {
  name                = "res1"
  resource_group_name = %[1]q
  location            = "eastus2"
  offer               = var.offer
  user                = var.user
}
`, testResourceGroup)

	offerVars := func(plan string) config.Variables {
		return config.Variables{
			"offer": config.ObjectVariable(map[string]config.Variable{
				"offer_id":  config.StringVariable("offer_a"),
				"plan_id":   config.StringVariable(plan),
				"term_unit": config.StringVariable("P1M"),
			}),
		}
	}

	t.Run("offer and user from variables: create sends them, a later plan change re-PUTs", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()

		vars := offerVars("psc_capacity_perf_ondemand")
		vars["user"] = config.ObjectVariable(map[string]config.Variable{
			"first_name":    config.StringVariable("Ada"),
			"last_name":     config.StringVariable("Lovelace"),
			"email_address": config.StringVariable("ada@example.com"),
		})

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config:          variableConfig,
					ConfigVariables: vars,
					ConfigStateChecks: []statecheck.StateCheck{
						statecheck.ExpectKnownValue(reservationAddr, tfjsonpath.New("offer").AtMapKey("offer_id"), knownvalue.StringExact("offer_a")),
						statecheck.ExpectKnownValue(reservationAddr, tfjsonpath.New("user"), knownvalue.Null()),
					},
					Check: func(_ *terraform.State) error {
						u := store.putBody(0).Properties.User
						if u == nil || derefString(u.FirstName) != "Ada" {
							return fmt.Errorf("user sent = %+v", u)
						}
						return nil
					},
				},
				{
					Config:          variableConfig,
					ConfigVariables: func() config.Variables { v := offerVars("psc_capacity_perf_1year"); v["user"] = vars["user"]; return v }(),
					Check: func(_ *terraform.State) error {
						if store.putCount() != 2 {
							return fmt.Errorf("puts = %d, want 2", store.putCount())
						}
						body := store.putBody(1)
						if got := derefString(body.Properties.Marketplace.OfferDetails.PlanID); got != "psc_capacity_perf_1year" {
							return fmt.Errorf("re-PUT plan_id = %q, want the new plan", got)
						}
						if u := body.Properties.User; u == nil || derefString(u.FirstName) != "Ada" {
							return fmt.Errorf("re-PUT user = %+v, want the user first sent (Ada)", u)
						}
						return nil
					},
				},
			},
		})
	})

	t.Run("offer values from another resource are unknown at plan time and the offer guard waits for them", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()

		cfg := func(planInput string) string {
			return providerBlock() + fmt.Sprintf(`
resource "terraform_data" "offer_id" {
  input = "offer_a"
}

resource "terraform_data" "plan_id" {
  input = %[2]q
}

resource "terraform_data" "term" {
  input = "P1M"
}

resource "everpure_azure_reservation" "test" {
  name                = "res1"
  resource_group_name = %[1]q
  location            = "eastus2"

  offer = {
    offer_id  = terraform_data.offer_id.output
    plan_id   = terraform_data.plan_id.output
    term_unit = terraform_data.term.output
  }
}
`, testResourceGroup, planInput)
		}

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: cfg("psc_capacity_perf_ondemand")},
				{
					Config: cfg("psc_capacity_perf_1year"),
					ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(reservationAddr, plancheck.ResourceActionUpdate),
					}},
					Check: func(_ *terraform.State) error {
						if store.putCount() != 2 {
							return fmt.Errorf("puts = %d, want 2", store.putCount())
						}
						return nil
					},
				},
			},
		})
	})

	t.Run("user from another resource is unknown at plan time, and create sends the resolved value", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()

		cfg := providerBlock() + fmt.Sprintf(`
resource "terraform_data" "user" {
  input = {
    first_name    = "Ada"
    last_name     = "Lovelace"
    email_address = "ada@example.com"
  }
}

resource "everpure_azure_reservation" "test" {
  name                = "res1"
  resource_group_name = %[1]q
  location            = "eastus2"

  offer = {
    offer_id  = "offer_a"
    plan_id   = "psc_capacity_perf_ondemand"
    term_unit = "P1M"
  }

  user = terraform_data.user.output
}
`, testResourceGroup)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config: cfg,
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(reservationAddr, tfjsonpath.New("user"), knownvalue.Null()),
				},
				Check: func(_ *terraform.State) error {
					u := store.putBody(0).Properties.User
					if u == nil || derefString(u.FirstName) != "Ada" || derefString(u.LastName) != "Lovelace" || derefString(u.EmailAddress) != "ada@example.com" {
						return fmt.Errorf("user sent = %+v, want the resolved one", u)
					}
					return nil
				},
			}},
		})
	})

	t.Run("user left at its null variable default sends no user", func(t *testing.T) {
		factories, store := newReservationTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config:          variableConfig,
				ConfigVariables: offerVars("psc_capacity_perf_ondemand"),
				Check: func(_ *terraform.State) error {
					if u := store.putBody(0).Properties.User; u != nil {
						return fmt.Errorf("user sent = %+v, want none", u)
					}
					return nil
				},
			}},
		})
	})
}

// reservationDirect drives ReservationResource's CRUD methods directly against the fake
// store, for the paths the Terraform CLI cannot reach deterministically: it refreshes
// before every apply, so a fault meant for the call inside Update would be spent by the
// refresh, and a create that fails after acceptance would leave the harness holding an
// exhausted poller entry.
type reservationDirect struct {
	r      *ReservationResource
	schema fwresource.SchemaResponse
	store  *fakeReservationStore
}

func newReservationDirect(t *testing.T) *reservationDirect {
	t.Helper()
	ctx := context.Background()
	store := newFakeReservationStore(testSubscriptionID)
	srv := store.newReservationsServer()
	opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: psbfake.NewReservationsServerTransport(&srv)}}

	r := &ReservationResource{}
	var configureResp fwresource.ConfigureResponse
	r.Configure(ctx, fwresource.ConfigureRequest{ProviderData: &Clients{
		cred:           &azcorefake.TokenCredential{},
		opts:           opts,
		subscriptionID: testSubscriptionID,
	}}, &configureResp)
	if configureResp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", configureResp.Diagnostics)
	}
	d := &reservationDirect{r: r, store: store}
	r.Schema(ctx, fwresource.SchemaRequest{}, &d.schema)
	return d
}

// model builds a reservation as Terraform holds it after a create. unknownComputed
// leaves the service-assigned values unknown, as in a create plan.
func (d *reservationDirect) model(t *testing.T, planID string, tags map[string]string, unknownComputed bool) ReservationResourceModel {
	t.Helper()
	ctx := context.Background()

	offerPlanName := types.StringValue("Plan " + planID)
	computed := types.StringValue
	id := armids.IDValue{StringValue: types.StringValue(reservationAPIID())}
	if unknownComputed {
		offerPlanName = types.StringUnknown()
		computed = func(string) types.String { return types.StringUnknown() }
		id = armids.IDValue{StringValue: types.StringUnknown()}
	}
	offer, diags := types.ObjectValueFrom(ctx, reservationOfferObjectType.AttrTypes, ReservationOfferModel{
		OfferID:  types.StringValue("offer_a"),
		PlanID:   types.StringValue(planID),
		TermUnit: types.StringValue("P1M"),
		PlanName: offerPlanName,
	})
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	tagMap := types.MapNull(types.StringType)
	if tags != nil {
		tagMap, diags = types.MapValueFrom(ctx, types.StringType, tags)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
	}
	timeoutsType, ok := d.schema.Schema.Blocks["timeouts"].Type().(timeouts.Type)
	if !ok {
		t.Fatalf("timeouts block type = %T", d.schema.Schema.Blocks["timeouts"].Type())
	}
	return ReservationResourceModel{
		ID:                            id,
		Name:                          types.StringValue("res1"),
		ResourceGroupName:             types.StringValue(testResourceGroup),
		Location:                      types.StringValue("eastus2"),
		Offer:                         offer,
		User:                          types.ObjectNull(reservationUserObjectType.AttrTypes),
		Tags:                          tagMap,
		InternalID:                    computed("internal-res1"),
		MarketplaceSubscriptionID:     computed("purchase-res1"),
		MarketplaceSubscriptionStatus: computed("Subscribed"),
		Timeouts:                      timeouts.Value{Object: types.ObjectNull(timeoutsType.AttrTypes)},
	}
}

func (d *reservationDirect) plan(t *testing.T, m ReservationResourceModel) tfsdk.Plan {
	t.Helper()
	plan := tfsdk.Plan{Schema: d.schema.Schema}
	if diags := plan.Set(context.Background(), &m); diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	return plan
}

func (d *reservationDirect) state(t *testing.T, m ReservationResourceModel) tfsdk.State {
	t.Helper()
	state := tfsdk.State{Schema: d.schema.Schema}
	if diags := state.Set(context.Background(), &m); diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	return state
}

// update runs Update from prior to planned and returns the response's diagnostics.
func (d *reservationDirect) update(t *testing.T, prior, planned ReservationResourceModel) diag.Diagnostics {
	t.Helper()
	plan := d.plan(t, planned)
	resp := &fwresource.UpdateResponse{State: tfsdk.State{Schema: d.schema.Schema, Raw: plan.Raw}}
	d.r.Update(context.Background(), fwresource.UpdateRequest{Plan: plan, State: d.state(t, prior)}, resp)
	return resp.Diagnostics
}

func TestReservationUpdateErrorPaths(t *testing.T) {
	const plan1, plan2 = "psc_capacity_perf_ondemand", "psc_capacity_perf_1year"

	tests := []struct {
		name  string
		setup func(*fakeReservationStore)
		// tagsOnly changes only the tags, so Update takes the PATCH path; otherwise the
		// plan changes and Update re-PUTs.
		tagsOnly     bool
		wantInDetail string
		wantPuts     int
	}{
		{
			name:         "a 404 at the tags PATCH says the reservation no longer exists",
			setup:        func(*fakeReservationStore) {}, // never seeded: the PATCH finds nothing
			tagsOnly:     true,
			wantInDetail: "deleted outside Terraform",
		},
		{
			name: "a failed GET before the re-PUT stops before any PUT",
			setup: func(s *fakeReservationStore) {
				s.seed(testResourceGroup, "res1", fakeSeededReservation(plan1))
				s.failNextGet(testResourceGroup, "res1", fakeFailure{status: 500, code: "InternalError"})
			},
			wantInDetail: "InternalError",
		},
		{
			name: "a re-PUT that fails while polling carries the plan-change hint",
			setup: func(s *fakeReservationStore) {
				s.seed(testResourceGroup, "res1", fakeSeededReservation(plan1))
				s.failNextPutLRO(testResourceGroup, "res1", fakeFailure{status: 400, code: "BadRequest"})
			},
			wantInDetail: "SaaS resource is visible",
			wantPuts:     1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newReservationDirect(t)
			tt.setup(d.store)

			var diags diag.Diagnostics
			if tt.tagsOnly {
				diags = d.update(t,
					d.model(t, plan1, map[string]string{"environment": "test"}, false),
					d.model(t, plan1, map[string]string{"environment": "prod"}, false))
			} else {
				diags = d.update(t, d.model(t, plan1, nil, false), d.model(t, plan2, nil, false))
			}

			if !diags.HasError() {
				t.Fatal("expected an error diagnostic")
			}
			if got, want := diags[len(diags)-1].Summary(), reservationOperation(actionUpdate).fail(detailError{}).Summary(); got != want {
				t.Fatalf("summary = %q, want %q", got, want)
			}
			requireContains(t, diags[len(diags)-1].Detail(), tt.wantInDetail)
			if got := d.store.putCount(); got != tt.wantPuts {
				t.Fatalf("puts = %d, want %d", got, tt.wantPuts)
			}
		})
	}
}

// TestReservationCreateFailureAfterAcceptance covers a create the service accepts and
// then fails while polling: the apply errors, but state already holds the reservation
// with its composed id, so Terraform marks it tainted and a destroy can reach it.
func TestReservationCreateFailureAfterAcceptance(t *testing.T) {
	ctx := context.Background()
	d := newReservationDirect(t)
	d.store.failNextPutLRO(testResourceGroup, "res1", fakeFailure{status: 400, code: "BadRequest"})

	plan := d.plan(t, d.model(t, "psc_capacity_perf_ondemand", nil, true))
	state := tfsdk.State{
		Schema: d.schema.Schema,
		Raw:    tftypes.NewValue(d.schema.Schema.Type().TerraformType(ctx), nil),
	}
	createResp := &fwresource.CreateResponse{State: state}
	d.r.Create(ctx, fwresource.CreateRequest{
		Plan:   plan,
		Config: tfsdk.Config{Schema: d.schema.Schema, Raw: plan.Raw},
	}, createResp)

	if !createResp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic")
	}
	if got, want := createResp.Diagnostics[0].Summary(), reservationOperation(actionCreate).fail(detailError{}).Summary(); got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}

	var saved ReservationResourceModel
	if diags := createResp.State.Get(ctx, &saved); diags.HasError() {
		t.Fatalf("state after the failed create is unreadable: %v", diags)
	}
	if saved.ID.ValueString() != reservationAPIID() {
		t.Fatalf("id in state = %q, want %q", saved.ID.ValueString(), reservationAPIID())
	}
	if !saved.InternalID.IsNull() || !saved.MarketplaceSubscriptionID.IsNull() {
		t.Fatalf("computed values in the tainted state = %v / %v, want null", saved.InternalID, saved.MarketplaceSubscriptionID)
	}

	deleteResp := &fwresource.DeleteResponse{State: createResp.State}
	d.r.Delete(ctx, fwresource.DeleteRequest{State: createResp.State}, deleteResp)
	if deleteResp.Diagnostics.HasError() {
		t.Fatalf("destroy of the tainted reservation failed: %v", deleteResp.Diagnostics)
	}
	if d.store.has(testResourceGroup, "res1") {
		t.Fatal("the reservation still exists after destroy")
	}
}

// TestFakeReservationStoreRefusals pins the fake to what the service refuses, so a
// provider that sent such a request would fail the integration tests instead of passing
// against a store more forgiving than the service.
func TestFakeReservationStoreRefusals(t *testing.T) {
	user := &armpurestorageblock.UserDetails{FirstName: to.Ptr("Ada")}
	put := func(offerID, planID, termUnit string) armpurestorageblock.Reservation {
		return armpurestorageblock.Reservation{
			Location: to.Ptr("eastus2"),
			Properties: &armpurestorageblock.ReservationPropertiesBaseResourceProperties{
				Marketplace: &armpurestorageblock.MarketplaceDetails{OfferDetails: &armpurestorageblock.OfferDetails{
					PublisherID: to.Ptr(reservationPublisherID),
					OfferID:     to.Ptr(offerID),
					PlanID:      to.Ptr(planID),
					TermUnit:    to.Ptr(termUnit),
				}},
			},
		}
	}

	tests := []struct {
		name string
		call func(*armpurestorageblock.ReservationsClient) error
		want int // 0 = accepted
	}{
		{"PATCH with tags only", func(c *armpurestorageblock.ReservationsClient) error {
			_, err := c.BeginUpdate(context.Background(), testResourceGroup, "res1", armpurestorageblock.ReservationUpdate{Tags: map[string]*string{"a": to.Ptr("b")}}, nil)
			return err
		}, 0},
		{"PATCH with a user", func(c *armpurestorageblock.ReservationsClient) error {
			_, err := c.BeginUpdate(context.Background(), testResourceGroup, "res1", armpurestorageblock.ReservationUpdate{
				Properties: &armpurestorageblock.ReservationUpdateProperties{User: user},
			}, nil)
			return err
		}, 400},
		{"re-PUT with a changed plan_id", func(c *armpurestorageblock.ReservationsClient) error {
			_, err := c.BeginCreate(context.Background(), testResourceGroup, "res1", put("offer_a", "other_plan", "P1M"), nil)
			return err
		}, 0},
		{"re-PUT with a changed offer_id", func(c *armpurestorageblock.ReservationsClient) error {
			_, err := c.BeginCreate(context.Background(), testResourceGroup, "res1", put("offer_b", "plan_a", "P1M"), nil)
			return err
		}, 400},
		{"re-PUT with a changed term_unit", func(c *armpurestorageblock.ReservationsClient) error {
			_, err := c.BeginCreate(context.Background(), testResourceGroup, "res1", put("offer_a", "plan_a", "P1Y"), nil)
			return err
		}, 400},
		{"PUT without properties", func(c *armpurestorageblock.ReservationsClient) error {
			_, err := c.BeginCreate(context.Background(), testResourceGroup, "res2", armpurestorageblock.Reservation{Location: to.Ptr("eastus2")}, nil)
			return err
		}, 400},
		{"PUT without marketplace details", func(c *armpurestorageblock.ReservationsClient) error {
			_, err := c.BeginCreate(context.Background(), testResourceGroup, "res2", armpurestorageblock.Reservation{
				Location:   to.Ptr("eastus2"),
				Properties: &armpurestorageblock.ReservationPropertiesBaseResourceProperties{},
			}, nil)
			return err
		}, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeReservationStore(testSubscriptionID)
			store.seed(testResourceGroup, "res1", fakeSeededReservation("plan_a"))
			srv := store.newReservationsServer()
			client, err := armpurestorageblock.NewReservationsClient(testSubscriptionID, &azcorefake.TokenCredential{},
				&arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: psbfake.NewReservationsServerTransport(&srv)}})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			err = tt.call(client)
			var respErr *azcore.ResponseError
			switch {
			case tt.want == 0 && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.want != 0 && !errors.As(err, &respErr):
				t.Fatalf("error = %v, want a %d response", err, tt.want)
			case tt.want != 0 && respErr.StatusCode != tt.want:
				t.Fatalf("status = %d, want %d", respErr.StatusCode, tt.want)
			}
		})
	}
}

// TestReservationResourceConfigCasingChange: a case-only respelling of
// resource_group_name in config must plan nothing, in particular not a replace.
func TestReservationResourceConfigCasingChange(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	orig := reservationConfig("psc_capacity_perf_ondemand", reservationUserBlock)
	respelled := strings.Replace(orig,
		fmt.Sprintf("resource_group_name = %q", testResourceGroup),
		fmt.Sprintf("resource_group_name = %q", strings.ToUpper(testResourceGroup)), 1)
	if respelled == orig {
		t.Fatal("reservationConfig no longer renders resource_group_name as this test expects")
	}

	factories, _ := newReservationTestProviderFactories()
	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{Config: orig},
			{Config: respelled, PlanOnly: true},
		},
	})
}
