// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// Classifying Azure errors: which failure is this? The text each one gets lives in
// errors.go; the callers here and in the resources pick the text.

const (
	// Storage pool delete failures, told apart by classifyStoragePoolDelete.
	// errCodeCannotDeleteResource is the synchronous 409 for live child resources;
	// errCodeDeleteStoragePoolContainsData fails the delete operation when the pool still
	// holds anything, including soft-deleted children Terraform cannot see.
	errCodeCannotDeleteResource          = "CannotDeleteResource"
	errCodeDeleteStoragePoolContainsData = "delete_storage_pool_contains_data"

	// The synchronous 400 a reservation delete gets while storage pools still use it.
	errCodeResourceDeletionValidateFailed = "ResourceDeletionValidateFailed"

	// Codes in the body of a 404 from the volume group status action, which tell a
	// missing parent pool from a missing volume group.
	errCodeStoragePoolNotFound    = "storage_pool_not_found"
	errCodeStoragePoolDeleted     = "storage_pool_deleted"
	errCodeStoragePoolUnreachable = "storage_pool_unreachable"

	// The 409 a platform console call gets when the pool has the console disabled. Its
	// error code is just "409", so only the message tells it apart from other conflicts.
	platformConsoleDisabledMessage = "Platform Console is not enabled"

	// The 409 a platform console call gets when the username is taken. The service also
	// answers 409 for a pool without the console, and the body's error code reaching the
	// client is just "409" in both cases, so only the message identifies a taken username.
	// Any other 409 (from ARM itself, such as a ReadOnly lock on the pool) matches neither.
	platformConsoleUserExistsMessage = "already exists"

	// The 400 Azure returns for a second extension of the same publisher and type on one
	// virtual machine. Its error code is the generic BadRequest, so only the message
	// recognises it.
	duplicateExtensionMessageSubstring = "Multiple VMExtensions per handler not supported"
)

func isNotFound(err error) bool {
	var respErr *azcore.ResponseError
	return errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound
}

func isConflict(err error) bool {
	var respErr *azcore.ResponseError
	return errors.As(err, &respErr) && respErr.StatusCode == http.StatusConflict
}

func isPlatformConsoleDisabled(err error) bool {
	return isConflict(err) && strings.Contains(err.Error(), platformConsoleDisabledMessage)
}

func isPlatformConsoleUserExists(err error) bool {
	return isConflict(err) && strings.Contains(err.Error(), platformConsoleUserExistsMessage)
}

// isDuplicateExtension reports whether err is Azure's duplicate-extension 400.
func isDuplicateExtension(err error) bool {
	var respErr *azcore.ResponseError
	if !errors.As(err, &respErr) || respErr.StatusCode != http.StatusBadRequest {
		return false
	}
	return strings.Contains(respErr.Error(), duplicateExtensionMessageSubstring)
}

// apiErrorCode is the error code in the response body behind err, or "".
func apiErrorCode(err error) string {
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		return respErr.ErrorCode
	}
	return ""
}

// isStoragePoolMissing reports whether a 404 from the volume group status action is about
// the parent pool (missing, deleted or unreachable) rather than the volume group. Any
// other or absent code, including ARM's own ResourceNotFound that never reaches the
// service, means the volume group.
func isStoragePoolMissing(err error) bool {
	switch apiErrorCode(err) {
	case errCodeStoragePoolNotFound, errCodeStoragePoolDeleted, errCodeStoragePoolUnreachable:
		return true
	default:
		return false
	}
}

// storagePoolDeleteFailure is the reason a storage pool delete failed, when it is one
// that has its own explanation.
type storagePoolDeleteFailure int

const (
	storagePoolDeleteOther storagePoolDeleteFailure = iota
	storagePoolDeleteHasChildren
	storagePoolDeleteContainsData
)

// classifyStoragePoolDelete tells the delete failures that need an explanation from the
// rest. Soft-deleted children are invisible to Terraform, so without the explanation
// nothing tells the user why the destroy keeps failing.
func classifyStoragePoolDelete(err error) storagePoolDeleteFailure {
	switch apiErrorCode(err) {
	case errCodeCannotDeleteResource:
		return storagePoolDeleteHasChildren
	case errCodeDeleteStoragePoolContainsData:
		return storagePoolDeleteContainsData
	default:
		return storagePoolDeleteOther
	}
}

// isReservationHasStoragePools reports whether err is the delete guard's refusal to delete
// a reservation that storage pools still use.
func isReservationHasStoragePools(err error) bool {
	return apiErrorCode(err) == errCodeResourceDeletionValidateFailed
}

// Request tracing.

// apiFailure is what a cause needs of an Azure error: what the service said went wrong,
// and the IDs support needs to find the request in server logs.
type apiFailure struct {
	// response is err's own text: for an Azure response error, a multi-line dump of the
	// request URL, status, error code and body. When the body carries the service's own
	// error message, that message comes first in a paragraph of its own, since in the dump
	// it is one JSON field among many.
	response string
	// requestIDs lists this run's correlation ID (known even when no response came
	// back), and, from the response, x-ms-request-id and x-ms-failure-cause ("gateway"
	// means the request never reached the service). azcore.ResponseError.Error() prints
	// no headers, so these are always added separately.
	requestIDs string
}

func (c *Clients) apiFailure(err error) apiFailure {
	var respErr *azcore.ResponseError
	errors.As(err, &respErr)
	response := err.Error()
	if message := serviceErrorMessage(respErr); message != "" {
		response = "Operation reported error:\n" + message + "\n\n" + response
	}
	return apiFailure{response: response, requestIDs: c.requestIDs(respErr)}
}

// serviceErrorMessage returns error.message from respErr's body, or "" when there is no
// such message. A failed call and a long-running operation that ends in Failed both carry
// it there: the SDK builds the error of a failed poll from the operation-status response,
// whose body is {"status": "Failed", "error": {"message": ...}}, often with no code.
func serviceErrorMessage(respErr *azcore.ResponseError) string {
	if respErr == nil || respErr.RawResponse == nil {
		return ""
	}
	// The SDK has already read the body while building respErr, and Payload returns that
	// cached copy.
	body, err := runtime.Payload(respErr.RawResponse)
	if err != nil {
		return ""
	}
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Error.Message)
}

func (c *Clients) requestIDs(respErr *azcore.ResponseError) string {
	parts := []string{"x-ms-correlation-request-id: " + c.correlationID}
	if respErr != nil && respErr.RawResponse != nil {
		if reqID := respErr.RawResponse.Header.Get("X-Ms-Request-Id"); reqID != "" {
			parts = append(parts, "x-ms-request-id: "+reqID)
		}
		if cause := respErr.RawResponse.Header.Get("X-Ms-Failure-Cause"); cause != "" {
			parts = append(parts, "x-ms-failure-cause: "+cause)
		}
	}
	return strings.Join(parts, ", ")
}

// apiCallError composes a frame and a generic Azure-failure cause for the common case:
// the failing call is the operation itself.
func (c *Clients) apiCallError(op operation, err error) diag.Diagnostic {
	return op.fail(newApiCallFailedError(op, c.apiFailure(err)))
}
