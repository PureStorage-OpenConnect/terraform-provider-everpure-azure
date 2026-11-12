// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
)

func TestExistingObjectCause(t *testing.T) {
	const (
		id           = "/subscriptions/s/resourceGroups/rg/providers/PureStorage.Block/storagePools/Pool1"
		resourceType = "everpure_azure_storage_pool"
	)
	importText := "see the " + resourceType + " documentation"

	tests := []struct {
		name       string
		state      string
		wantCode   string
		wantImport bool
		wantText   string
	}{
		{name: "absent", state: "", wantCode: "already_exists", wantImport: true},
		{name: "succeeded", state: "Succeeded", wantCode: "already_exists", wantImport: true},
		{name: "accepted", state: "Accepted", wantCode: "already_exists_accepted", wantImport: true, wantText: "contact Everpure support"},
		{name: "deleting", state: "Deleting", wantCode: "already_exists_deleting", wantText: "Wait for the deletion to finish, then retry"},
		{name: "failed", state: "Failed", wantCode: "already_exists_failed", wantText: "in the state Failed. Contact Everpure support"},
		{name: "canceled", state: "Canceled", wantCode: "already_exists_failed", wantText: "in the state Canceled. Contact Everpure support"},
		{name: "unknown value", state: "Migrating", wantCode: "already_exists_in_state", wantImport: true, wantText: "in the state Migrating"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := existingObjectCause(kindStoragePool, resourceType, id, tt.state)
			if got.code != tt.wantCode {
				t.Fatalf("code = %q, want %q (%s)", got.code, tt.wantCode, got.message)
			}
			if !strings.Contains(got.message, `"`+id+`"`) {
				t.Errorf("message %q does not name the ID %q", got.message, id)
			}
			if !strings.Contains(got.message, "storage pool") {
				t.Errorf("message %q does not name the kind", got.message)
			}
			if has := strings.Contains(got.message, importText); has != tt.wantImport {
				t.Errorf("message %q: import advice present = %t, want %t", got.message, has, tt.wantImport)
			}
			if !strings.Contains(got.message, tt.wantText) {
				t.Errorf("message %q does not contain %q", got.message, tt.wantText)
			}
		})
	}
}

// TestExistingObjectCauseCoversTheAPIEnums keeps every value either provisioning-state enum
// can hold mapped to a cause on purpose: a value added to the SDK lands in the generic
// cause and fails this test until it is classified.
func TestExistingObjectCauseCoversTheAPIEnums(t *testing.T) {
	known := map[string]string{
		"Succeeded": "already_exists",
		"Accepted":  "already_exists_accepted",
		"Deleting":  "already_exists_deleting",
		"Failed":    "already_exists_failed",
		"Canceled":  "already_exists_failed",
	}
	var states []string
	for _, s := range armpurestorageblock.PossibleProvisioningStateValues() {
		states = append(states, string(s))
	}
	for _, s := range armpurestorageblock.PossibleResourceProvisioningStateValues() {
		states = append(states, string(s))
	}
	for _, state := range states {
		t.Run(state, func(t *testing.T) {
			want, ok := known[state]
			if !ok {
				t.Fatalf("state %q has no cause chosen for it", state)
			}
			if got := existingObjectCause(kindVolume, "everpure_azure_volume", "id", state).code; got != want {
				t.Errorf("code = %q, want %q", got, want)
			}
		})
	}
}

func TestProvisioningStateName(t *testing.T) {
	if got := provisioningStateName[armpurestorageblock.ProvisioningState](nil); got != "" {
		t.Errorf("nil ProvisioningState = %q, want empty", got)
	}
	if got := provisioningStateName(to.Ptr(armpurestorageblock.ProvisioningStateAccepted)); got != "Accepted" {
		t.Errorf("ProvisioningState = %q, want Accepted", got)
	}
	if got := provisioningStateName(to.Ptr(armpurestorageblock.ResourceProvisioningStateCanceled)); got != "Canceled" {
		t.Errorf("ResourceProvisioningState = %q, want Canceled", got)
	}
}

func TestImportAdviceIsSharedWithTheExtensionError(t *testing.T) {
	got := newExtensionAlreadyExistsError("ext-id").message
	if !strings.Contains(got, importAdvice("everpure_azure_virtual_machine_connection")) {
		t.Errorf("extension error %q does not carry the shared import advice", got)
	}
}
