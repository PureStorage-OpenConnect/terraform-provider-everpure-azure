// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
)

// fakeResponseError builds a real *azcore.ResponseError the same way the SDK
// would from an ARM error body, so classification logic is exercised exactly
// as it runs in production.
func fakeResponseError(statusCode int, code, message string, headers map[string]string) error {
	return fakeResponseErrorWithBody(statusCode, `{"error":{"code":"`+code+`","message":"`+message+`"}}`, headers)
}

// fakeResponseErrorWithBody is fakeResponseError for a body of any shape.
func fakeResponseErrorWithBody(statusCode int, body string, headers map[string]string) error {
	header := http.Header{}
	for k, v := range headers {
		header.Set(k, v)
	}
	resp := &http.Response{
		StatusCode: statusCode,
		Status:     http.StatusText(statusCode),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	return runtime.NewResponseError(resp)
}

func requireContains(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Fatalf("%q does not contain %q", s, substr)
	}
}

func TestAPIFailure(t *testing.T) {
	clients := &Clients{correlationID: "corr-1"}

	tests := []struct {
		name    string
		err     error
		wantIDs string
	}{
		{
			name:    "the run's correlation ID, plus x-ms-request-id when a response is present",
			err:     fakeResponseError(500, "Internal", "boom", map[string]string{"x-ms-request-id": "req-1"}),
			wantIDs: "x-ms-correlation-request-id: corr-1, x-ms-request-id: req-1",
		},
		{
			name:    "no x-ms-request-id when the response has none",
			err:     fakeResponseError(500, "Internal", "boom", nil),
			wantIDs: "x-ms-correlation-request-id: corr-1",
		},
		{
			name: "x-ms-failure-cause when the response carries it",
			err: fakeResponseError(400, "InvalidTemplate", "boom", map[string]string{
				"x-ms-request-id":    "req-1",
				"x-ms-failure-cause": "gateway",
			}),
			wantIDs: "x-ms-correlation-request-id: corr-1, x-ms-request-id: req-1, x-ms-failure-cause: gateway",
		},
		{
			name:    "still the correlation ID when no response was received",
			err:     errors.New("network blip"),
			wantIDs: "x-ms-correlation-request-id: corr-1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clients.apiFailure(tt.err)
			if got.requestIDs != tt.wantIDs {
				t.Errorf("requestIDs = %q, want %q", got.requestIDs, tt.wantIDs)
			}
		})
	}
}

func TestAPIFailureResponse(t *testing.T) {
	clients := &Clients{correlationID: "corr-1"}

	// The body of the operation-status response a storage pool create got, live, when its
	// subnet did not exist: no error code, only a message.
	const failedOperationBody = `{
  "id": "/providers/PureStorage.Block/locations/EASTUS/operationStatuses/op-1",
  "name": "op-1",
  "resourceId": "/subscriptions/sub/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1",
  "status": "Failed",
  "startTime": "2026-10-27T12:25:58.3360992Z",
  "endTime": "2026-10-27T12:27:18.538483Z",
  "error": {
    "message": "Subnet with resource Id: /subscriptions/sub/resourceGroups/net/providers/Microsoft.Network/virtualNetworks/vnet/subnets/missing not found in the VNet /subscriptions/sub/resourceGroups/net/providers/Microsoft.Network/virtualNetworks/vnet"
  }
}`

	tests := []struct {
		name string
		err  error
		// message is the service's message expected ahead of the error's own text; empty
		// means the error's own text alone.
		message string
	}{
		{
			name:    "a failed operation's message, which has no error code",
			err:     fakeResponseErrorWithBody(http.StatusOK, failedOperationBody, nil),
			message: "Subnet with resource Id: /subscriptions/sub/resourceGroups/net/providers/Microsoft.Network/virtualNetworks/vnet/subnets/missing not found in the VNet /subscriptions/sub/resourceGroups/net/providers/Microsoft.Network/virtualNetworks/vnet",
		},
		{
			name:    "a failed call's message",
			err:     fakeResponseError(http.StatusBadRequest, "ResourceCreationValidateFailed", "The reservation was not found.", nil),
			message: "The reservation was not found.",
		},
		{
			name:    "the message trimmed of surrounding whitespace",
			err:     fakeResponseErrorWithBody(http.StatusInternalServerError, `{"error":{"message":"  boom \n"}}`, nil),
			message: "boom",
		},
		{
			name: "the error's own text when the body's error has no message",
			err:  fakeResponseErrorWithBody(http.StatusInternalServerError, `{"error":{"code":"InternalError"}}`, nil),
		},
		{
			name: "the error's own text when the message is blank",
			err:  fakeResponseErrorWithBody(http.StatusInternalServerError, `{"error":{"code":"InternalError","message":"  "}}`, nil),
		},
		{
			name: "the error's own text when the body is not JSON",
			err:  fakeResponseErrorWithBody(http.StatusBadGateway, "<html>Bad Gateway</html>", nil),
		},
		{
			name: "the error's own text when the body is empty",
			err:  fakeResponseErrorWithBody(http.StatusServiceUnavailable, "", nil),
		},
		{
			name: "the error's own text when no response was received",
			err:  errors.New("network blip"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.err.Error()
			if tt.message != "" {
				want = "Operation reported error:\n" + tt.message + "\n\n" + want
			}
			if got := clients.apiFailure(tt.err).response; got != want {
				t.Errorf("response = %q, want %q", got, want)
			}
		})
	}
}

func TestIsNotFound(t *testing.T) {
	t.Run("recognizes a 404 ResponseError", func(t *testing.T) {
		if !isNotFound(fakeResponseError(404, "NotFound", "gone", nil)) {
			t.Fatal("expected isNotFound to return true")
		}
	})

	t.Run("rejects a non-404 ResponseError", func(t *testing.T) {
		if isNotFound(fakeResponseError(500, "Internal", "boom", nil)) {
			t.Fatal("expected isNotFound to return false")
		}
	})

	t.Run("rejects a plain error", func(t *testing.T) {
		if isNotFound(errors.New("boom")) {
			t.Fatal("expected isNotFound to return false")
		}
	})
}

func TestIsStoragePoolMissing(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"volume group not found", &azcore.ResponseError{StatusCode: 404, ErrorCode: "volume_group_not_found"}, false},
		{"pool not found", &azcore.ResponseError{StatusCode: 404, ErrorCode: "storage_pool_not_found"}, true},
		{"pool deleted", &azcore.ResponseError{StatusCode: 404, ErrorCode: "storage_pool_deleted"}, true},
		{"pool unreachable", &azcore.ResponseError{StatusCode: 404, ErrorCode: "storage_pool_unreachable"}, true},
		{"unknown code", &azcore.ResponseError{StatusCode: 404, ErrorCode: "something_else"}, false},
		{"ARM-level ResourceNotFound", &azcore.ResponseError{StatusCode: 404, ErrorCode: "ResourceNotFound"}, false},
		{"no code", &azcore.ResponseError{StatusCode: 404}, false},
		{"not a response error", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isStoragePoolMissing(tt.err); got != tt.want {
				t.Errorf("isStoragePoolMissing() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClassifyStoragePoolDelete(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want storagePoolDeleteFailure
	}{
		{
			name: "a synchronous CannotDeleteResource 409 means live children",
			err:  fakeResponseError(409, "CannotDeleteResource", "Cannot delete resource while nested resources exist.", nil),
			want: storagePoolDeleteHasChildren,
		},
		{
			name: "an LRO failure carrying delete_storage_pool_contains_data means data on the pool",
			err:  fakeResponseError(412, "delete_storage_pool_contains_data", "All volume groups must be removed.", nil),
			want: storagePoolDeleteContainsData,
		},
		{
			name: "an unrecognized error code",
			err:  fakeResponseError(500, "InternalError", "boom", nil),
			want: storagePoolDeleteOther,
		},
		{
			name: "an error that is not a response",
			err:  errors.New("network blip"),
			want: storagePoolDeleteOther,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyStoragePoolDelete(tt.err); got != tt.want {
				t.Errorf("classifyStoragePoolDelete() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsPlatformConsoleDisabled(t *testing.T) {
	const prefix = "Partner resource configure platform console auth for storage pool id e8ddb729-305e-4ea3-9be5-9b70d85bd442 failed with error "
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "the live 409 for a pool without the console",
			err:  fakeResponseError(409, "409", prefix+fakeConsoleDisabledMessage, nil),
			want: true,
		},
		{
			name: "the service's other 409, an existing user",
			err:  fakeResponseError(409, "409", prefix+`user \"tfadmin\" already exists`, nil),
		},
		{
			name: "an ARM 409 of its own",
			err:  fakeResponseError(409, "ScopeLocked", "The scope cannot perform write operation because it is locked.", nil),
		},
		{
			name: "the same text on another status",
			err:  fakeResponseError(400, "400", prefix+fakeConsoleDisabledMessage, nil),
		},
		{
			name: "a plain error",
			err:  errors.New(fakeConsoleDisabledMessage),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPlatformConsoleDisabled(tt.err); got != tt.want {
				t.Errorf("isPlatformConsoleDisabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsDuplicateExtension(t *testing.T) {
	t.Run("recognizes the live duplicate-handler 400 body", func(t *testing.T) {
		err := fakeResponseError(http.StatusBadRequest, "BadRequest",
			"Multiple VMExtensions per handler not supported for OS type 'Linux'. VMExtension 'new-name' with "+
				"handler 'PureStorage.Extensions.MountIscsiLinux' already added or specified in input.", nil)
		if !isDuplicateExtension(err) {
			t.Fatal("expected isDuplicateExtension to return true")
		}
	})

	t.Run("rejects any other 400", func(t *testing.T) {
		err := fakeResponseError(http.StatusBadRequest, "BadRequest", "some other validation error", nil)
		if isDuplicateExtension(err) {
			t.Fatal("expected isDuplicateExtension to return false")
		}
	})

	t.Run("rejects a non-400 error", func(t *testing.T) {
		err := fakeResponseError(http.StatusInternalServerError, "InternalError",
			"Multiple VMExtensions per handler not supported for OS type 'Linux'.", nil)
		if isDuplicateExtension(err) {
			t.Fatal("expected isDuplicateExtension to return false for a non-400 status")
		}
	})
}

func TestIsPlatformConsoleUserExists(t *testing.T) {
	const prefix = "Partner resource configure platform console auth for storage pool id e8ddb729-305e-4ea3-9be5-9b70d85bd442 failed with error "
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "the service's 409 for an existing user",
			err:  fakeResponseError(409, "409", prefix+`user \"tfadmin\" already exists`, nil),
			want: true,
		},
		{
			name: "the service's other 409, a pool without the console",
			err:  fakeResponseError(409, "409", prefix+fakeConsoleDisabledMessage, nil),
		},
		{
			name: "an ARM 409 of its own",
			err:  fakeResponseError(409, "ScopeLocked", "The scope cannot perform write operation because it is locked.", nil),
		},
		{
			name: "the same text on another status",
			err:  fakeResponseError(400, "400", prefix+`user \"tfadmin\" already exists`, nil),
		},
		{
			name: "a plain error",
			err:  errors.New("already exists"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPlatformConsoleUserExists(tt.err); got != tt.want {
				t.Errorf("isPlatformConsoleUserExists() = %v, want %v", got, tt.want)
			}
		})
	}
}
