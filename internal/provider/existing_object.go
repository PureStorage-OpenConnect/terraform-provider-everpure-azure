// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// An ARM PUT is an upsert, so a create of a name that is taken would adopt the object and
// change it, and a create_before_destroy replacement under an unchanged name would adopt
// the old object and then delete it. Create therefore gets the object first and refuses
// to go on when it exists.

// provisioningStateName is the string of a provisioning state of any of the API's models,
// or "" when the response has none. The five models a create looks up all use
// ProvisioningState; the API also has a narrower ResourceProvisioningState, so the helper
// takes either. Both are open enums, so the value is not limited to the constants.
func provisioningStateName[T ~string](state *T) string {
	if state == nil {
		return ""
	}
	return string(*state)
}

// existingObjectCause is the cause for an object of kind k that exists under the name a
// create asked for, chosen by the object's provisioning state. id is the ID as Azure
// returned it: lookups ignore case, so that spelling is the one terraform import needs.
func existingObjectCause(k kind, resourceType string, id string, state string) detailError {
	switch state {
	case "", string(armpurestorageblock.ProvisioningStateSucceeded):
		return newAlreadyExistsError(k, id, resourceType)
	case string(armpurestorageblock.ProvisioningStateAccepted):
		return newAlreadyExistsAcceptedError(k, id, resourceType)
	case string(armpurestorageblock.ProvisioningStateDeleting):
		return newAlreadyExistsDeletingError(k, id)
	case string(armpurestorageblock.ProvisioningStateFailed), string(armpurestorageblock.ProvisioningStateCanceled):
		return newAlreadyExistsFailedError(k, id, state)
	default:
		return newAlreadyExistsInStateError(k, id, state, resourceType)
	}
}

// existingObjectDiagnostic judges the Get that precedes a create. It returns nil when the
// create may go on, which is when the object is not found. Any 404 counts, even one for a
// missing parent: the create then fails with the parent's own error. Any other failure of
// the Get is reported as it is, and an object that was found is refused.
func (c *Clients) existingObjectDiagnostic(op operation, resourceType string, id *string, state string, err error) diag.Diagnostic {
	switch {
	case err == nil:
		returned := derefString(id)
		if returned == "" {
			returned = op.id
		}
		return op.fail(existingObjectCause(op.kind, resourceType, returned, state))
	case isNotFound(err):
		return nil
	default:
		return c.apiCallError(op, err)
	}
}
