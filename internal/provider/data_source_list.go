// SPDX-License-Identifier: Apache-2.0

package provider

import "strings"

// idLess orders the plural data sources' and list resources' results by ID, ignoring
// case, since the list APIs don't guarantee an order and ARM doesn't always echo an ID's
// casing consistently. Snapshots are the exception: their list API sorts them itself.
func idLess(id, otherID string) bool {
	return strings.ToLower(id) < strings.ToLower(otherID)
}

// ensureListElementProperties gives a list response element an empty properties object
// when the API returned none, so every plural data source can map it through the same
// nil-safe mapping as a well-formed element: its id, name and other top-level fields
// come through, and every property-derived attribute is null. Failing the read instead
// (as the singular data sources do, where a response with no properties has nothing
// else to offer) would let one malformed element hide every well-formed element in the
// list. Dropping it would shorten the list with no visible cause.
//
// An element with no ID is the one case each list does drop: it can't be sorted
// (see idLess), referenced, or told apart from another such element.
func ensureListElementProperties[P any](properties **P) {
	if *properties == nil {
		*properties = new(P)
	}
}
