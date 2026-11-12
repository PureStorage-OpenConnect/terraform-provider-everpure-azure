// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestApplyPlatformConsoleActivationCredentials(t *testing.T) {
	utc := time.Date(2026, 10, 2, 14, 30, 0, 0, time.UTC)

	tests := []struct {
		name string
		in   armpurestorageblock.PlatformConsoleActivationCode
		want PlatformConsoleActivationCredentialsEphemeralResourceModel
	}{
		{
			name: "all fields set, expiry already in UTC",
			in: armpurestorageblock.PlatformConsoleActivationCode{
				ActivationCode: to.Ptr("code"), Username: to.Ptr("user"), ExpiresAt: to.Ptr(utc),
			},
			want: PlatformConsoleActivationCredentialsEphemeralResourceModel{
				Password:  types.StringValue("code"),
				Username:  types.StringValue("user"),
				ExpiresAt: types.StringValue("2026-10-02T14:30:00Z"),
			},
		},
		{
			name: "expiry in another offset is rendered in UTC",
			in: armpurestorageblock.PlatformConsoleActivationCode{
				ActivationCode: to.Ptr("code"), Username: to.Ptr("user"),
				ExpiresAt: to.Ptr(utc.In(time.FixedZone("UTC-5", -5*60*60))),
			},
			want: PlatformConsoleActivationCredentialsEphemeralResourceModel{
				Password:  types.StringValue("code"),
				Username:  types.StringValue("user"),
				ExpiresAt: types.StringValue("2026-10-02T14:30:00Z"),
			},
		},
		{
			name: "sub-second expiry is truncated to whole seconds",
			in: armpurestorageblock.PlatformConsoleActivationCode{
				ActivationCode: to.Ptr("code"), Username: to.Ptr("user"),
				ExpiresAt: to.Ptr(utc.Add(250 * time.Millisecond)),
			},
			want: PlatformConsoleActivationCredentialsEphemeralResourceModel{
				Password:  types.StringValue("code"),
				Username:  types.StringValue("user"),
				ExpiresAt: types.StringValue("2026-10-02T14:30:00Z"),
			},
		},
		{
			name: "nil fields map to null",
			in:   armpurestorageblock.PlatformConsoleActivationCode{},
			want: PlatformConsoleActivationCredentialsEphemeralResourceModel{
				Password:  types.StringNull(),
				Username:  types.StringNull(),
				ExpiresAt: types.StringNull(),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Start from non-null values to prove the mapping overwrites rather than
			// keeps whatever the model already held.
			got := PlatformConsoleActivationCredentialsEphemeralResourceModel{
				Password:  types.StringValue("stale"),
				Username:  types.StringValue("stale"),
				ExpiresAt: types.StringValue("stale"),
			}
			applyPlatformConsoleActivationCredentials(&got, tt.in)

			if !got.Password.Equal(tt.want.Password) {
				t.Errorf("password = %s, want %s", got.Password, tt.want.Password)
			}
			if !got.Username.Equal(tt.want.Username) {
				t.Errorf("username = %s, want %s", got.Username, tt.want.Username)
			}
			if !got.ExpiresAt.Equal(tt.want.ExpiresAt) {
				t.Errorf("expires_at = %s, want %s", got.ExpiresAt, tt.want.ExpiresAt)
			}
		})
	}
}
