// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"

	azcorefake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	psbfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock/fake"
)

// fakeReservationStore is the in-memory backing store behind the integration tests'
// armpurestorageblock/fake.ReservationsServer. Like the real service it keeps `user` as
// first sent: a re-PUT of an existing reservation replaces its offer details, tags and
// location but never its user.
type fakeReservationStore struct {
	mu             sync.Mutex
	subscriptionID string
	reservations   map[string]*armpurestorageblock.Reservation

	// putBodies records every PUT body (create and re-PUT alike) in order, as received.
	putBodies []armpurestorageblock.Reservation

	// patchBodies records every PATCH body in order.
	patchBodies []armpurestorageblock.ReservationUpdate

	// omitOffer makes GET and list answer without offer details for the named
	// reservations, as the service may for a reservation it has no offer to report for.
	omitOffer map[string]bool

	// locationOverride makes GET report the given location spelling for a reservation
	// (the service answers a display name for some of them).
	locationOverride map[string]string

	// deleteFailure makes the next DELETE of a reservation fail synchronously with the
	// given status and code. It is consumed by that delete, so the test harness's own
	// teardown destroy can then succeed.
	deleteFailure map[string]fakeFailure

	// putFailure makes the next PUT of a reservation fail synchronously, once.
	putFailure map[string]fakeFailure

	// putLROFailure makes the next PUT of a reservation be accepted and then fail while
	// polling, once. A failed create leaves the reservation behind in the Failed state;
	// a failed re-PUT leaves the stored reservation as it was.
	putLROFailure map[string]fakeFailure

	// getFailure makes the next GET of a reservation fail, once.
	getFailure map[string]fakeFailure

	// pageSize, when > 0, splits a list answer into pages of that many entries, so the
	// callers' pager loops are exercised over more than one page.
	pageSize int
}

type fakeFailure struct {
	status int
	code   string
}

func newFakeReservationStore(subscriptionID string) *fakeReservationStore {
	return &fakeReservationStore{
		subscriptionID:   subscriptionID,
		reservations:     map[string]*armpurestorageblock.Reservation{},
		locationOverride: map[string]string{},
		deleteFailure:    map[string]fakeFailure{},
		putFailure:       map[string]fakeFailure{},
		putLROFailure:    map[string]fakeFailure{},
		getFailure:       map[string]fakeFailure{},
		omitOffer:        map[string]bool{},
	}
}

func reservationKey(resourceGroupName, name string) string {
	return strings.ToLower(resourceGroupName + "/" + name)
}

func (s *fakeReservationStore) fakeReservationID(resourceGroupName, name string) string {
	return fmt.Sprintf(
		"/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/reservations/%s",
		s.subscriptionID, resourceGroupName, name,
	)
}

// seed adds a reservation as if it had been created outside Terraform.
func (s *fakeReservationStore) seed(resourceGroupName, name string, res armpurestorageblock.Reservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res.ID = to.Ptr(s.fakeReservationID(resourceGroupName, name))
	res.Name = to.Ptr(name)
	s.reservations[reservationKey(resourceGroupName, name)] = &res
}

func (s *fakeReservationStore) remove(resourceGroupName, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.reservations, reservationKey(resourceGroupName, name))
}

func (s *fakeReservationStore) mutate(resourceGroupName, name string, mutate func(*armpurestorageblock.Reservation)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if res, ok := s.reservations[reservationKey(resourceGroupName, name)]; ok {
		mutate(res)
	}
}

func (s *fakeReservationStore) setOmitOfferDetails(resourceGroupName, name string, omit bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.omitOffer[reservationKey(resourceGroupName, name)] = omit
}

func (s *fakeReservationStore) setLocationOverride(resourceGroupName, name, location string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.locationOverride[reservationKey(resourceGroupName, name)] = location
}

func (s *fakeReservationStore) failNextDelete(resourceGroupName, name string, failure fakeFailure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteFailure[reservationKey(resourceGroupName, name)] = failure
}

func (s *fakeReservationStore) failNextPut(resourceGroupName, name string, failure fakeFailure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putFailure[reservationKey(resourceGroupName, name)] = failure
}

func (s *fakeReservationStore) failNextPutLRO(resourceGroupName, name string, failure fakeFailure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putLROFailure[reservationKey(resourceGroupName, name)] = failure
}

func (s *fakeReservationStore) failNextGet(resourceGroupName, name string, failure fakeFailure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getFailure[reservationKey(resourceGroupName, name)] = failure
}

func (s *fakeReservationStore) putCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.putBodies)
}

func (s *fakeReservationStore) patchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.patchBodies)
}

func (s *fakeReservationStore) putBody(i int) armpurestorageblock.Reservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putBodies[i]
}

func (s *fakeReservationStore) patchBody(i int) armpurestorageblock.ReservationUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.patchBodies[i]
}

func (s *fakeReservationStore) has(resourceGroupName, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.reservations[reservationKey(resourceGroupName, name)]
	return ok
}

// newReservationsServer builds the fake.ReservationsServer wired to this store.
func (s *fakeReservationStore) newReservationsServer() psbfake.ReservationsServer {
	return psbfake.ReservationsServer{
		BeginCreate: func(
			_ context.Context, resourceGroupName, name string, resource armpurestorageblock.Reservation,
			_ *armpurestorageblock.ReservationsClientBeginCreateOptions,
		) (azcorefake.PollerResponder[armpurestorageblock.ReservationsClientCreateResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			s.putBodies = append(s.putBodies, resource)

			key := reservationKey(resourceGroupName, name)
			if failure, ok := s.putFailure[key]; ok {
				delete(s.putFailure, key)
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(failure.status, failure.code)
				return azcorefake.PollerResponder[armpurestorageblock.ReservationsClientCreateResponse]{}, errResp
			}
			if resource.Properties == nil || resource.Properties.Marketplace == nil {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(400, "BadRequest")
				return azcorefake.PollerResponder[armpurestorageblock.ReservationsClientCreateResponse]{}, errResp
			}

			var stored *armpurestorageblock.Reservation
			if existing, ok := s.reservations[key]; ok {
				// The service refuses a changed offer or term on an existing reservation:
				// only the plan can move, so a PUT carrying any other change must never
				// reach it.
				if sent, held := resource.Properties.Marketplace.OfferDetails, reservationOfferDetails(*existing); sent != nil && held != nil &&
					(derefString(sent.OfferID) != derefString(held.OfferID) || derefString(sent.TermUnit) != derefString(held.TermUnit)) {
					var errResp azcorefake.ErrorResponder
					errResp.SetResponseError(400, "BadRequest")
					return azcorefake.PollerResponder[armpurestorageblock.ReservationsClientCreateResponse]{}, errResp
				}
				// A re-PUT: the offer details, tags and location are replaced; user is
				// create-only and stays as first sent. A re-PUT that then fails while
				// polling changes nothing.
				stored = existing
				if _, fails := s.putLROFailure[key]; !fails {
					stored.Location = resource.Location
					stored.Tags = resource.Tags
					if stored.Properties != nil && stored.Properties.Marketplace != nil {
						stored.Properties.Marketplace.OfferDetails = fakeStampOffer(resource.Properties.Marketplace.OfferDetails)
					}
				}
			} else {
				res := resource
				res.ID = to.Ptr(s.fakeReservationID(resourceGroupName, name))
				res.Name = to.Ptr(name)
				props := *resource.Properties
				props.ProvisioningState = to.Ptr(armpurestorageblock.ProvisioningStateSucceeded)
				if _, fails := s.putLROFailure[key]; fails {
					props.ProvisioningState = to.Ptr(armpurestorageblock.ProvisioningStateFailed)
				}
				props.ReservationInternalID = to.Ptr("internal-" + name)
				marketplace := *props.Marketplace
				marketplace.SubscriptionID = to.Ptr("purchase-" + name)
				marketplace.SubscriptionStatus = to.Ptr(armpurestorageblock.MarketplaceSubscriptionStatusPendingFulfillmentStart)
				marketplace.OfferDetails = fakeStampOffer(marketplace.OfferDetails)
				props.Marketplace = &marketplace
				res.Properties = &props
				s.reservations[key] = &res
				stored = &res
			}

			var responder azcorefake.PollerResponder[armpurestorageblock.ReservationsClientCreateResponse]
			if failure, ok := s.putLROFailure[key]; ok {
				delete(s.putLROFailure, key)
				// A PUT is accepted before it can fail, so the initiating call succeeds and
				// the error surfaces from polling.
				responder.AddNonTerminalResponse(200, nil)
				responder.SetTerminalError(failure.status, failure.code)
				return responder, azcorefake.ErrorResponder{}
			}
			responder.SetTerminalResponse(200, armpurestorageblock.ReservationsClientCreateResponse{Reservation: *stored}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		Get: func(
			_ context.Context, resourceGroupName, name string, _ *armpurestorageblock.ReservationsClientGetOptions,
		) (azcorefake.Responder[armpurestorageblock.ReservationsClientGetResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			key := reservationKey(resourceGroupName, name)
			if failure, ok := s.getFailure[key]; ok {
				delete(s.getFailure, key)
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(failure.status, failure.code)
				return azcorefake.Responder[armpurestorageblock.ReservationsClientGetResponse]{}, errResp
			}
			stored, ok := s.reservations[key]
			if !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(404, "NotFound")
				return azcorefake.Responder[armpurestorageblock.ReservationsClientGetResponse]{}, errResp
			}

			res := *stored
			props := *stored.Properties
			res.Properties = &props
			if s.omitOffer[key] && props.Marketplace != nil {
				marketplace := *props.Marketplace
				marketplace.OfferDetails = nil
				props.Marketplace = &marketplace
			}
			if loc, ok := s.locationOverride[key]; ok {
				res.Location = to.Ptr(loc)
			}

			var responder azcorefake.Responder[armpurestorageblock.ReservationsClientGetResponse]
			responder.SetResponse(200, armpurestorageblock.ReservationsClientGetResponse{Reservation: res}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		BeginUpdate: func(
			_ context.Context, resourceGroupName, name string, properties armpurestorageblock.ReservationUpdate,
			_ *armpurestorageblock.ReservationsClientBeginUpdateOptions,
		) (azcorefake.PollerResponder[armpurestorageblock.ReservationsClientUpdateResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			s.patchBodies = append(s.patchBodies, properties)

			var responder azcorefake.PollerResponder[armpurestorageblock.ReservationsClientUpdateResponse]
			// The gateway rejects anything but tags on a PATCH, user included.
			if properties.Properties != nil && properties.Properties.User != nil {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(400, "BadRequest")
				return responder, errResp
			}
			stored, ok := s.reservations[reservationKey(resourceGroupName, name)]
			if !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(404, "NotFound")
				return responder, errResp
			}
			if properties.Tags != nil {
				stored.Tags = properties.Tags
			}
			responder.SetTerminalResponse(200, armpurestorageblock.ReservationsClientUpdateResponse{Reservation: *stored}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		BeginDelete: func(
			_ context.Context, resourceGroupName, name string, _ *armpurestorageblock.ReservationsClientBeginDeleteOptions,
		) (azcorefake.PollerResponder[armpurestorageblock.ReservationsClientDeleteResponse], azcorefake.ErrorResponder) {
			s.mu.Lock()
			defer s.mu.Unlock()

			key := reservationKey(resourceGroupName, name)
			var responder azcorefake.PollerResponder[armpurestorageblock.ReservationsClientDeleteResponse]

			if _, ok := s.reservations[key]; !ok {
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(404, "NotFound")
				return responder, errResp
			}

			if failure, ok := s.deleteFailure[key]; ok {
				delete(s.deleteFailure, key)
				var errResp azcorefake.ErrorResponder
				errResp.SetResponseError(failure.status, failure.code)
				return responder, errResp
			}

			delete(s.reservations, key)
			// The generated client's deleteOperation only accepts 202 or 204 as the
			// initial response, so an immediate delete answers 204.
			responder.SetTerminalResponse(204, armpurestorageblock.ReservationsClientDeleteResponse{}, nil)
			return responder, azcorefake.ErrorResponder{}
		},

		NewListByResourceGroupPager: func(
			resourceGroupName string, _ *armpurestorageblock.ReservationsClientListByResourceGroupOptions,
		) azcorefake.PagerResponder[armpurestorageblock.ReservationsClientListByResourceGroupResponse] {
			entries, pageSize := s.listEntries(func(key string) bool {
				return strings.HasPrefix(key, strings.ToLower(resourceGroupName)+"/")
			})

			var pager azcorefake.PagerResponder[armpurestorageblock.ReservationsClientListByResourceGroupResponse]
			for _, page := range paginate(entries, pageSize) {
				pager.AddPage(200, armpurestorageblock.ReservationsClientListByResourceGroupResponse{
					ReservationListResult: armpurestorageblock.ReservationListResult{Value: page},
				}, nil)
			}
			return pager
		},

		NewListBySubscriptionPager: func(
			_ *armpurestorageblock.ReservationsClientListBySubscriptionOptions,
		) azcorefake.PagerResponder[armpurestorageblock.ReservationsClientListBySubscriptionResponse] {
			entries, pageSize := s.listEntries(func(string) bool { return true })

			var pager azcorefake.PagerResponder[armpurestorageblock.ReservationsClientListBySubscriptionResponse]
			for _, page := range paginate(entries, pageSize) {
				pager.AddPage(200, armpurestorageblock.ReservationsClientListBySubscriptionResponse{
					ReservationListResult: armpurestorageblock.ReservationListResult{Value: page},
				}, nil)
			}
			return pager
		},
	}
}

// listEntries returns copies of the stored reservations whose key matches, and the page
// size to split them by. Like GET, a list answers without offer details when the store
// is set to omit them for that reservation, and with a location overridden to another spelling.
func (s *fakeReservationStore) listEntries(match func(key string) bool) ([]*armpurestorageblock.Reservation, int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var entries []*armpurestorageblock.Reservation
	for key, stored := range s.reservations {
		if !match(key) {
			continue
		}
		res := *stored
		if stored.Properties != nil {
			props := *stored.Properties
			res.Properties = &props
			if s.omitOffer[key] && props.Marketplace != nil {
				marketplace := *props.Marketplace
				marketplace.OfferDetails = nil
				props.Marketplace = &marketplace
			}
		}
		if loc, ok := s.locationOverride[key]; ok {
			res.Location = to.Ptr(loc)
		}
		entries = append(entries, &res)
	}
	return entries, s.pageSize
}

func (s *fakeReservationStore) setPageSize(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pageSize = n
}

// fakeStampOffer returns a copy of the offer details as the service stores them: the
// publisher, offer, plan and term unit as sent, plus a plan name of its own.
func fakeStampOffer(sent *armpurestorageblock.OfferDetails) *armpurestorageblock.OfferDetails {
	if sent == nil {
		return nil
	}
	stamped := *sent
	stamped.PlanName = to.Ptr("Plan " + derefString(sent.PlanID))
	return &stamped
}

// fakeSeededReservation is a reservation as created outside Terraform, with the offer
// the example configurations use.
func fakeSeededReservation(planID string) armpurestorageblock.Reservation {
	return armpurestorageblock.Reservation{
		Location: to.Ptr("eastus2"),
		Properties: &armpurestorageblock.ReservationPropertiesBaseResourceProperties{
			ProvisioningState:     to.Ptr(armpurestorageblock.ProvisioningStateSucceeded),
			ReservationInternalID: to.Ptr("internal-seeded"),
			Marketplace: &armpurestorageblock.MarketplaceDetails{
				SubscriptionID:     to.Ptr("purchase-seeded"),
				SubscriptionStatus: to.Ptr(armpurestorageblock.MarketplaceSubscriptionStatusSubscribed),
				OfferDetails: &armpurestorageblock.OfferDetails{
					PublisherID: to.Ptr(reservationPublisherID),
					OfferID:     to.Ptr("offer_a"),
					PlanID:      to.Ptr(planID),
					TermUnit:    to.Ptr("P1M"),
					PlanName:    to.Ptr("Plan " + planID),
				},
			},
			User: &armpurestorageblock.UserDetails{
				FirstName:    to.Ptr("Seeded"),
				LastName:     to.Ptr("User"),
				EmailAddress: to.Ptr("seeded@example.com"),
			},
		},
	}
}
