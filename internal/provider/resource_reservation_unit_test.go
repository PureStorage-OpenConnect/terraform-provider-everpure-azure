// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestReservationsClientNilClients(t *testing.T) {
	r := &ReservationResource{}
	var diags diag.Diagnostics

	client, ok := r.reservationsClient(&diags)

	if ok || client != nil {
		t.Fatalf("client = %v, ok = %v, want nil and false with nil clients", client, ok)
	}
	if !diags.HasError() {
		t.Fatal("expected a diagnostic, got none")
	}
}

func TestReservationImportStateNilClients(t *testing.T) {
	r := &ReservationResource{}
	req := resource.ImportStateRequest{
		ID: "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/reservations/res1",
	}
	resp := &resource.ImportStateResponse{}

	r.ImportState(context.Background(), req, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected a diagnostic, got none")
	}
}

func TestReservationNamePattern(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"letters, digits, hyphens and underscores", "res-1_ok", true},
		{"single character", "a", true},
		{"127 characters, the maximum", strings.Repeat("a", 127), true},
		{"128 characters, one too many", strings.Repeat("a", 128), false},
		{"empty", "", false},
		{"a space", "res 1", false},
		{"a dot", "res.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reservationNamePattern.MatchString(tt.value); got != tt.want {
				t.Fatalf("MatchString(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

// TestReservationTermUnitValidators runs the validators the schema itself carries, so
// the grammar and the bare-"P" exclusion are tested where they are attached.
func TestReservationTermUnitValidators(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"P1M", true},
		{"P1Y", true},
		{"P3Y", true},
		{"P12M", true},
		{"P1Y6M", true},
		{"P2W", true},
		{"P30D", true},
		{"P1Y2M3W4D", true},
		{"1M", false},
		{"P", false},
		{"PT1H", false},
		{"monthly", false},
		{"", false},
		{"p1m", false},
		{"P1M ", false},
		{"P1D1M", false},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			var diags diag.Diagnostics
			for _, v := range reservationTermUnitValidators() {
				resp := &validator.StringResponse{}
				v.ValidateString(context.Background(), validator.StringRequest{ConfigValue: types.StringValue(tt.value)}, resp)
				diags.Append(resp.Diagnostics...)
			}
			if got := !diags.HasError(); got != tt.want {
				t.Fatalf("accepted(%q) = %v, want %v (diagnostics: %v)", tt.value, got, tt.want, diags)
			}
		})
	}
}

func TestIsReservationHasStoragePools(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"the delete guard", fakeResponseError(http.StatusBadRequest, errCodeResourceDeletionValidateFailed, "pools exist", nil), true},
		{"forbidden by status alone", fakeResponseError(http.StatusForbidden, "", "no", nil), false},
		{"an authorization failure", fakeResponseError(http.StatusForbidden, "AuthorizationFailed", "no", nil), false},
		{"another API error", fakeResponseError(http.StatusInternalServerError, "InternalError", "boom", nil), false},
		{"a transport error", errors.New("connection reset"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isReservationHasStoragePools(tt.err); got != tt.want {
				t.Fatalf("isReservationHasStoragePools() = %v, want %v", got, tt.want)
			}
		})
	}
}
