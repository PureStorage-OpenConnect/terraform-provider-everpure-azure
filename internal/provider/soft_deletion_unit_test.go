// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestSoftDeletionFromAPI(t *testing.T) {
	tests := []struct {
		name           string
		sd             *armpurestorageblock.DestroyedStateProperties
		wantNull       bool
		wantPrevious   string
		wantDestroyed  string
		wantEradicated string
	}{
		{
			name:     "nil pointer maps to a null object",
			sd:       nil,
			wantNull: true,
		},
		{
			name:     "Destroyed false maps to a null object",
			sd:       &armpurestorageblock.DestroyedStateProperties{Destroyed: to.Ptr(false)},
			wantNull: true,
		},
		{
			name:     "Destroyed nil maps to a null object",
			sd:       &armpurestorageblock.DestroyedStateProperties{},
			wantNull: true,
		},
		{
			name: "every field set maps through",
			sd: &armpurestorageblock.DestroyedStateProperties{
				Destroyed:            to.Ptr(true),
				PreviousName:         to.Ptr("vol1"),
				DestroyedAt:          to.Ptr(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)),
				EradicationTimestamp: to.Ptr(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)),
			},
			wantPrevious:   "vol1",
			wantDestroyed:  "2026-09-30T12:00:00Z",
			wantEradicated: "2026-10-30T12:00:00Z",
		},
		{
			name: "individual nil fields on an otherwise-destroyed object map to null strings",
			sd: &armpurestorageblock.DestroyedStateProperties{
				Destroyed: to.Ptr(true),
			},
			wantPrevious:   "",
			wantDestroyed:  "",
			wantEradicated: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, diags := softDeletionFromAPI(tt.sd)
			if diags.HasError() {
				t.Fatalf("unexpected diagnostics: %v", diags)
			}

			if tt.wantNull {
				if !got.IsNull() {
					t.Fatalf("expected a null object, got %v", got)
				}
				return
			}
			if got.IsNull() {
				t.Fatalf("expected a non-null object, got null")
			}

			attrs := got.Attributes()
			checkString := func(attrName, want string) {
				v, ok := attrs[attrName]
				if !ok {
					t.Fatalf("missing attribute %q", attrName)
				}
				sv, ok := v.(types.String)
				if !ok {
					t.Fatalf("attribute %q is a %T, want types.String", attrName, v)
				}
				if want == "" {
					if !sv.IsNull() {
						t.Errorf("%s: expected null, got %q", attrName, sv.ValueString())
					}
					return
				}
				if sv.IsNull() {
					t.Errorf("%s: expected %q, got null", attrName, want)
					return
				}
				if sv.ValueString() != want {
					t.Errorf("%s: expected %q, got %q", attrName, want, sv.ValueString())
				}
			}

			checkString("previous_name", tt.wantPrevious)
			checkString("destroyed_at", tt.wantDestroyed)
			checkString("eradication_timestamp", tt.wantEradicated)
		})
	}
}

func TestSoftDeletionDetailsFromAPI(t *testing.T) {
	tests := []struct {
		name         string
		sd           *armpurestorageblock.DestroyedStateProperties
		wantPrevious types.String
	}{
		{
			name:         "nil pointer maps to an object of null fields",
			sd:           nil,
			wantPrevious: types.StringNull(),
		},
		{
			name:         "Destroyed nil still maps through",
			sd:           &armpurestorageblock.DestroyedStateProperties{PreviousName: to.Ptr("vg1")},
			wantPrevious: types.StringValue("vg1"),
		},
		{
			name:         "Destroyed false still maps through",
			sd:           &armpurestorageblock.DestroyedStateProperties{Destroyed: to.Ptr(false), PreviousName: to.Ptr("vg1")},
			wantPrevious: types.StringValue("vg1"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, diags := softDeletionDetailsFromAPI(tt.sd)
			if diags.HasError() {
				t.Fatalf("unexpected diagnostics: %v", diags)
			}
			if got.IsNull() {
				t.Fatalf("expected a non-null object, got null")
			}
			if previous := got.Attributes()["previous_name"]; !previous.Equal(tt.wantPrevious) {
				t.Errorf("previous_name: expected %v, got %v", tt.wantPrevious, previous)
			}
		})
	}
}
