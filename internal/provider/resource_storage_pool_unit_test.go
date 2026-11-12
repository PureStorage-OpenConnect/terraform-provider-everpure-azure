// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// TestStoragePoolsClientNilClients confirms that a resource whose Configure
// never ran (r.clients still nil) reports a diagnostic instead of panicking
// on r.clients.StoragePools(). This is the state every CRUD method is in
// after the provider defers Configure because a config value was still
// unknown.
func TestStoragePoolsClientNilClients(t *testing.T) {
	r := &StoragePoolResource{}
	var diags diag.Diagnostics

	client, ok := r.storagePoolsClient(&diags)

	if ok {
		t.Fatal("ok = true, want false with nil clients")
	}
	if client != nil {
		t.Fatalf("client = %v, want nil", client)
	}
	if !diags.HasError() {
		t.Fatal("expected a diagnostic, got none")
	}
}

// TestImportStateNilClients confirms ImportState reports a diagnostic
// instead of panicking on r.clients.subscriptionID when Configure never ran
// (a provider config value was still unknown on the last Configure call).
func TestImportStateNilClients(t *testing.T) {
	r := &StoragePoolResource{}
	req := resource.ImportStateRequest{
		ID: "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1",
	}
	resp := &resource.ImportStateResponse{}

	r.ImportState(context.Background(), req, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected a diagnostic, got none")
	}
}

func TestStoragePoolNamePattern(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"letters, digits, hyphens and underscores", "pool-1_ok", true},
		{"single character", "a", true},
		{"127 characters, the maximum", strings.Repeat("a", 127), true},
		{"128 characters, one too many", strings.Repeat("a", 128), false},
		{"empty", "", false},
		{"a space", "pool 1", false},
		{"a dot", "pool.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := storagePoolNamePattern.MatchString(tt.value); got != tt.want {
				t.Fatalf("MatchString(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestZonePattern(t *testing.T) {
	tests := []struct {
		name string
		zone string
		want bool
	}{
		{"single digit", "1", true},
		{"multiple digits", "12", true},
		{"empty, for a region with no availability zones", "", true},
		{"zero", "0", false},
		{"leading zero", "01", false},
		{"negative", "-1", false},
		{"non-numeric", "one", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := zonePattern.MatchString(tt.zone); got != tt.want {
				t.Fatalf("MatchString(%q) = %v, want %v", tt.zone, got, tt.want)
			}
		})
	}
}
