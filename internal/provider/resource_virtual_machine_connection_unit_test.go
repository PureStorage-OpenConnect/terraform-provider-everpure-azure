// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

// testVMID builds a full ARM ID for a virtual machine under subscriptionID, for tests
// that need a virtual_machine_id fixture (including one in a different subscription
// than the provider's own, which this resource must accept).
func testVMID(subscriptionID, vmName string) string {
	return armids.VirtualMachineID{
		ResourceGroupID: armids.ResourceGroupID{
			SubscriptionID:    subscriptionID,
			ResourceGroupName: testResourceGroup,
		},
		VirtualMachineName: vmName,
	}.FullID()
}

// armidsTestVMID is a same-subscription convenience wrapper around testVMID.
func armidsTestVMID(subscriptionID string) string {
	return testVMID(subscriptionID, "vm1")
}

func TestVirtualMachineIDValidatorAcceptsAnySubscription(t *testing.T) {
	v := armids.TypeVirtualMachine.Validator()

	t.Run("same subscription as the provider", func(t *testing.T) {
		req := validator.StringRequest{Path: path.Root("virtual_machine_id"), ConfigValue: types.StringValue(testVMID(testSubscriptionID, "vm1"))}
		var resp validator.StringResponse
		v.ValidateString(context.Background(), req, &resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
	})

	t.Run("a different subscription than the provider's own is still schema-valid", func(t *testing.T) {
		otherSubscription := "22222222-2222-2222-2222-222222222222"
		req := validator.StringRequest{Path: path.Root("virtual_machine_id"), ConfigValue: types.StringValue(testVMID(otherSubscription, "vm1"))}
		var resp validator.StringResponse
		v.ValidateString(context.Background(), req, &resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("expected a cross-subscription virtual machine ID to be schema-valid, got %v", resp.Diagnostics)
		}

		// It must parse to that other subscription, so the client can be scoped to it.
		parsed, err := armids.ParseVirtualMachineID(testVMID(otherSubscription, "vm1"))
		if err != nil {
			t.Fatalf("ParseVirtualMachineID: %v", err)
		}
		if parsed.SubscriptionID != otherSubscription {
			t.Fatalf("parsed subscription = %q, want %q", parsed.SubscriptionID, otherSubscription)
		}
	})

	t.Run("rejects a storage pool ID", func(t *testing.T) {
		req := validator.StringRequest{Path: path.Root("virtual_machine_id"), ConfigValue: types.StringValue(testVGID("pool1", "vg1"))}
		var resp validator.StringResponse
		v.ValidateString(context.Background(), req, &resp)
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected a volume group ID to be rejected as a virtual_machine_id")
		}
	})
}

func TestVirtualMachineExtensionImportID(t *testing.T) {
	validID := testVMID(testSubscriptionID, "vm1") + "/extensions/MountIscsiLinux-deploy"

	t.Run("a well-formed extension ID parses and validates", func(t *testing.T) {
		req := validator.StringRequest{Path: path.Root("id"), ConfigValue: types.StringValue(validID)}
		var resp validator.StringResponse
		armids.TypeVirtualMachineExtension.Validator().ValidateString(context.Background(), req, &resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}

		parsed, err := armids.ParseVirtualMachineExtensionID(validID)
		if err != nil {
			t.Fatalf("ParseVirtualMachineExtensionID: %v", err)
		}
		if parsed.VirtualMachineName != "vm1" || parsed.ExtensionName != "MountIscsiLinux-deploy" {
			t.Fatalf("unexpected parse result: %+v", parsed)
		}
		if got := parsed.VirtualMachine().FullID(); got != testVMID(testSubscriptionID, "vm1") {
			t.Fatalf("VirtualMachine().FullID() = %q, want %q", got, testVMID(testSubscriptionID, "vm1"))
		}
	})

	t.Run("a virtual machine ID (missing the extensions segment) is rejected", func(t *testing.T) {
		if _, err := armids.ParseVirtualMachineExtensionID(testVMID(testSubscriptionID, "vm1")); err == nil {
			t.Fatal("expected an error parsing a bare virtual machine ID as an extension ID")
		}
	})

	t.Run("a volume group ID is rejected", func(t *testing.T) {
		if _, err := armids.ParseVirtualMachineExtensionID(testVGID("pool1", "vg1")); err == nil {
			t.Fatal("expected an error parsing a volume group ID as an extension ID")
		}
	})
}

func TestIscsiSessionCountValidator(t *testing.T) {
	v := int64validator.Between(1, 16)

	tests := []struct {
		value   int64
		wantErr bool
	}{
		{0, true},
		{1, false},
		{16, false},
		{17, true},
	}
	for _, tt := range tests {
		req := validator.Int64Request{Path: path.Root("iscsi_session_count"), ConfigValue: types.Int64Value(tt.value)}
		var resp validator.Int64Response
		v.ValidateInt64(context.Background(), req, &resp)
		if resp.Diagnostics.HasError() != tt.wantErr {
			t.Fatalf("iscsi_session_count = %d: HasError() = %v, want %v", tt.value, resp.Diagnostics.HasError(), tt.wantErr)
		}
	}
}

func TestWindowsSettingsTimeoutValidators(t *testing.T) {
	v := int64validator.AtLeast(1)

	tests := []struct {
		value   int64
		wantErr bool
	}{
		{0, true},
		{1, false},
	}
	for _, attrName := range []string{"disk_timeout_in_seconds", "link_down_time_in_seconds"} {
		for _, tt := range tests {
			req := validator.Int64Request{Path: path.Root(attrName), ConfigValue: types.Int64Value(tt.value)}
			var resp validator.Int64Response
			v.ValidateInt64(context.Background(), req, &resp)
			if resp.Diagnostics.HasError() != tt.wantErr {
				t.Fatalf("%s = %d: HasError() = %v, want %v", attrName, tt.value, resp.Diagnostics.HasError(), tt.wantErr)
			}
		}
	}
}

func TestInitiatorNameValidator(t *testing.T) {
	v := stringvalidator.LengthAtLeast(1)

	t.Run("empty string is rejected", func(t *testing.T) {
		req := validator.StringRequest{Path: path.Root("initiator_name"), ConfigValue: types.StringValue("")}
		var resp validator.StringResponse
		v.ValidateString(context.Background(), req, &resp)
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an empty initiator_name to be rejected")
		}
	})

	t.Run("null (unset) is not validated — it means \"keep the guest's IQN\"", func(t *testing.T) {
		req := validator.StringRequest{Path: path.Root("initiator_name"), ConfigValue: types.StringNull()}
		var resp validator.StringResponse
		v.ValidateString(context.Background(), req, &resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("unexpected diagnostics for a null value: %v", resp.Diagnostics)
		}
	})

	t.Run("any non-empty string is accepted, with no format check", func(t *testing.T) {
		for _, value := range []string{"iqn.2026-09.com.contoso:app01", "not-an-iqn-at-all", "x"} {
			req := validator.StringRequest{Path: path.Root("initiator_name"), ConfigValue: types.StringValue(value)}
			var resp validator.StringResponse
			v.ValidateString(context.Background(), req, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("value %q: unexpected diagnostics: %v", value, resp.Diagnostics)
			}
		}
	})
}
