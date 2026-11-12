// SPDX-License-Identifier: Apache-2.0

package provider

import "strings"

// isSubscriptionMismatch reports whether idSubscriptionID names a different subscription
// than the one this provider instance is configured for. The SDK's clients are pinned to
// the provider's subscription, so continuing would otherwise surface as a misleading
// "not found" once the call reaches ARM.
func isSubscriptionMismatch(idSubscriptionID, expectedSubscriptionID string) bool {
	return !strings.EqualFold(idSubscriptionID, expectedSubscriptionID)
}

// subscribedID is an ARM ID that knows the subscription it is in.
type subscribedID interface {
	FullID() string
	Subscription() string
}

// parseIDInSubscription parses an ARM ID of kind k and checks that it is in the provider's
// subscription. It returns a nil cause when both hold, and otherwise the cause for the
// caller to report under its own frame: where the ID came from decides the frame.
func parseIDInSubscription[T subscribedID](parse func(string) (T, error), raw string, k kind, expectedSubscriptionID string) (T, *detailError) {
	id, err := parse(raw)
	if err != nil {
		c := idErrorCause(err)
		return id, &c
	}
	return id, subscriptionMismatchCause(id, k, expectedSubscriptionID)
}

// subscriptionMismatchCause returns the cause for id, of kind k, being in another
// subscription than the provider's, or nil when it is in the provider's own. Read, Update
// and Delete check the ID in state with it before any call: the clients would otherwise
// look in the provider's subscription, where the object is not, and read the 404 as the
// object being gone.
func subscriptionMismatchCause(id subscribedID, k kind, expectedSubscriptionID string) *detailError {
	if isSubscriptionMismatch(id.Subscription(), expectedSubscriptionID) {
		c := newSubscriptionMismatchError(k, id.FullID(), id.Subscription(), expectedSubscriptionID)
		return &c
	}
	return nil
}

// subscriptionOrResourceGroupScope is the ID of what a list of objects that live directly in a
// resource group covers: that resource group, or, when resourceGroupName is empty, the whole
// subscription. Objects listed under a parent object, such as volume groups, are scoped by
// the parent's ID instead.
func subscriptionOrResourceGroupScope(subscriptionID, resourceGroupName string) string {
	if resourceGroupName == "" {
		return "/subscriptions/" + subscriptionID
	}
	return "/subscriptions/" + subscriptionID + "/resourceGroups/" + resourceGroupName
}
