// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"errors"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

// idErrorCause is the cause for an ID that armids rejected: it picks the text from the kind
// of rejection. Every error armids returns for an ID is one of its three typed errors, so
// anything else is the provider's own bug.
func idErrorCause(err error) detailError {
	var invalid *armids.InvalidIDError
	var noParent *armids.NoParentError
	var notNested *armids.NotNestedError
	switch {
	case errors.As(err, &invalid):
		return newIdNotValidError(invalid.Input, invalid.Noun, invalid.WantFormat)
	case errors.As(err, &noParent):
		return newIdHasNoParentError(noParent.Input, noParent.Noun)
	case errors.As(err, &notNested):
		return newIdNotNestedError(notNested.Input, notNested.Noun, notNested.WantNoun)
	default:
		return newProviderBugError(err.Error())
	}
}
