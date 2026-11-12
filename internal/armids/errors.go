// SPDX-License-Identifier: Apache-2.0

package armids

import "fmt"

// The errors below carry the facts of a rejected ID and leave the wording shown to a
// user to the caller; Error is only a terse fallback for a caller that holds nothing but
// an error.

// InvalidIDError is returned when Input does not match the ID grammar of Noun.
// WantFormat is an example of the expected shape.
type InvalidIDError struct {
	Input      string
	Noun       string
	WantFormat string
}

func (e *InvalidIDError) Error() string {
	return fmt.Sprintf("%q is not a valid %s ID: expected %s", e.Input, e.Noun, e.WantFormat)
}

// NoParentError is returned when Input is a Noun ID of a type that sits directly under
// the resource group, so it has no parent resource to cut.
type NoParentError struct {
	Input string
	Noun  string
}

func (e *NoParentError) Error() string {
	return fmt.Sprintf("%q is a %s ID, which has no parent resource", e.Input, e.Noun)
}

// NotNestedError is returned when Input is a Noun ID that is not nested under a WantNoun.
type NotNestedError struct {
	Input    string
	Noun     string
	WantNoun string
}

func (e *NotNestedError) Error() string {
	return fmt.Sprintf("%q is a %s ID, not nested under a %s", e.Input, e.Noun, e.WantNoun)
}
