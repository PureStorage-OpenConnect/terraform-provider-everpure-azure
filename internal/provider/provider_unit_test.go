// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func lookupEnvFrom(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

// fixedCredentialFactory is the credentialFactory tests inject through
// newProvider when a test cares about driving the fake ARM transport, not
// about authentication itself: it ignores the resolved authConfig entirely
// and always returns cred.
func fixedCredentialFactory(cred azcore.TokenCredential) credentialFactory {
	return func(context.Context, authConfig, *arm.ClientOptions) (azcore.TokenCredential, error) {
		return cred, nil
	}
}

// TestResolveSubscriptionID exercises resolveSetting (auth.go) with
// subscription_id's own two env keys - the precedence spec for
// subscription_id, kept as its own test because it's what Configure actually
// calls, even though subscription_id is not itself part of authConfig.
func TestResolveSubscriptionID(t *testing.T) {
	tests := []struct {
		name       string
		config     types.String
		env        map[string]string
		wantStatus settingStatus
		wantValue  string
	}{
		{
			name:   "prefers the config value over any environment variable",
			config: types.StringValue("config-sub"),
			env: map[string]string{
				"ARM_SUBSCRIPTION_ID":   "arm-sub",
				"AZURE_SUBSCRIPTION_ID": "azure-sub",
			},
			wantStatus: settingResolved,
			wantValue:  "config-sub",
		},
		{
			name:   "falls back to ARM_SUBSCRIPTION_ID when config is null",
			config: types.StringNull(),
			env: map[string]string{
				"ARM_SUBSCRIPTION_ID":   "arm-sub",
				"AZURE_SUBSCRIPTION_ID": "azure-sub",
			},
			wantStatus: settingResolved,
			wantValue:  "arm-sub",
		},
		{
			name:       "falls back to AZURE_SUBSCRIPTION_ID when config is null and ARM_SUBSCRIPTION_ID is unset",
			config:     types.StringNull(),
			env:        map[string]string{"AZURE_SUBSCRIPTION_ID": "azure-sub"},
			wantStatus: settingResolved,
			wantValue:  "azure-sub",
		},
		{
			name:       "treats an empty config string as unset",
			config:     types.StringValue(""),
			env:        map[string]string{"ARM_SUBSCRIPTION_ID": "arm-sub"},
			wantStatus: settingResolved,
			wantValue:  "arm-sub",
		},
		{
			name:       "reports unset when config is null and no environment variable is set",
			config:     types.StringNull(),
			wantStatus: settingUnset,
		},
		{
			name:       "reports unknown without error when config is an unknown value",
			config:     types.StringUnknown(),
			wantStatus: settingUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, status, _ := resolveSetting(tt.config, lookupEnvFrom(tt.env), "ARM_SUBSCRIPTION_ID", "AZURE_SUBSCRIPTION_ID")
			if status != tt.wantStatus {
				t.Fatalf("status = %v, want %v", status, tt.wantStatus)
			}
			if status == settingResolved && got != tt.wantValue {
				t.Fatalf("value = %q, want %q", got, tt.wantValue)
			}
		})
	}
}

// TestHasUnknownAuthAttributeAuxiliaryTenantIDsElement covers the element-level
// check hasUnknownAuthAttribute needs on top of its list-level one: the list
// itself can be known (not Null, not Unknown) while one of its elements still
// is, e.g. `auxiliary_tenant_ids = [aws_iam... , some_unresolved.value]`.
func TestHasUnknownAuthAttributeAuxiliaryTenantIDsElement(t *testing.T) {
	t.Run("an unknown element makes the whole model unknown", func(t *testing.T) {
		list, diags := types.ListValue(types.StringType, []attr.Value{
			types.StringValue("tenant-a"),
			types.StringUnknown(),
		})
		if diags.HasError() {
			t.Fatalf("unexpected error building list: %v", diags)
		}
		m := providerModel{AuxiliaryTenantIDs: list}
		if !m.hasUnknownAuthAttribute() {
			t.Error("hasUnknownAuthAttribute() = false, want true with an unknown list element")
		}
	})

	t.Run("every element known reports no unknown attribute", func(t *testing.T) {
		m := providerModel{AuxiliaryTenantIDs: stringList(t, "tenant-a", "tenant-b")}
		if m.hasUnknownAuthAttribute() {
			t.Error("hasUnknownAuthAttribute() = true, want false with every element known")
		}
	})

	t.Run("a null (not unknown) auxiliary_tenant_ids reports no unknown attribute", func(t *testing.T) {
		m := providerModel{}
		if m.hasUnknownAuthAttribute() {
			t.Error("hasUnknownAuthAttribute() = true, want false for the zero-value model")
		}
	})
}
