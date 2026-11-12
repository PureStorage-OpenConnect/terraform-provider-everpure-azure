// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

// baseVirtualMachineConnectionModel returns a minimal, schema-conformant model with
// every block absent and every optional attribute null/default.
func baseVirtualMachineConnectionModel() VirtualMachineConnectionResourceModel {
	return VirtualMachineConnectionResourceModel{
		IscsiSessionCount: types.Int64Value(2),
		InitiatorName:     types.StringNull(),
		RebootAllowed:     types.BoolValue(false),
		ReapplyOnBoot:     types.ListNull(reapplyOnBootObjectType),
		WindowsSettings:   types.ListNull(windowsSettingsObjectType),
	}
}

func TestBuildIscsiExtensionSettings(t *testing.T) {
	ctx := context.Background()
	targets := []iscsiTarget{
		{IQN: "iqn.2010-01.com.purestorage.psc:aaa-vg1", IP: "10.0.1.4", Port: 3260},
		{IQN: "iqn.2010-01.com.purestorage.psc:aaa-vg1", IP: "10.0.1.5", Port: 3260},
	}

	t.Run("minimal: targets, replaceTargets and sessionCount reflect config; other keys carry the extension's own defaults", func(t *testing.T) {
		data := baseVirtualMachineConnectionModel()
		settings, diags := buildIscsiExtensionSettings(ctx, data, targets)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if len(settings.Targets) != 2 || !settings.ReplaceTargets || settings.SessionCount != 2 {
			t.Fatalf("unexpected core fields: %+v", settings)
		}
		if settings.InitiatorName != "" {
			t.Fatalf("expected no initiator name, got %q", settings.InitiatorName)
		}
		if settings.Reboot {
			t.Fatal("expected reboot = false")
		}
		if settings.AutoUpdateConfiguration != (autoUpdateConfigurationSetting{}) {
			t.Fatalf("expected zero-value autoUpdateConfiguration, got %+v", settings.AutoUpdateConfiguration)
		}
		if settings.AdvancedWindowsSettings != (advancedWindowsSettingsSetting{}) {
			t.Fatalf("expected zero-value advancedWindowsSettings, got %+v", settings.AdvancedWindowsSettings)
		}
	})

	t.Run("full: every block's values are reflected", func(t *testing.T) {
		data := baseVirtualMachineConnectionModel()
		data.InitiatorName = types.StringValue("iqn.2026-09.com.contoso:app01")
		data.RebootAllowed = types.BoolValue(true)
		data.IscsiSessionCount = types.Int64Value(4)
		reapply, diags := types.ListValueFrom(ctx, reapplyOnBootObjectType, []ReapplyOnBootModel{{
			Enabled:                types.BoolValue(true),
			RebootOnReapplyAllowed: types.BoolValue(true),
		}})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		data.ReapplyOnBoot = reapply
		windows, diags := types.ListValueFrom(ctx, windowsSettingsObjectType, []WindowsSettingsModel{{
			DiskTimeoutInSeconds:  types.Int64Value(240),
			LinkDownTimeInSeconds: types.Int64Value(180),
		}})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		data.WindowsSettings = windows

		settings, diags := buildIscsiExtensionSettings(ctx, data, targets)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if settings.InitiatorName != "iqn.2026-09.com.contoso:app01" {
			t.Fatalf("unexpected initiator name: %q", settings.InitiatorName)
		}
		if !settings.Reboot || settings.SessionCount != 4 {
			t.Fatalf("unexpected reboot/sessionCount: %+v", settings)
		}
		if !settings.AutoUpdateConfiguration.Enabled || !settings.AutoUpdateConfiguration.AllowRebootOnAutoUpdate {
			t.Fatalf("unexpected autoUpdateConfiguration: %+v", settings.AutoUpdateConfiguration)
		}
		if settings.AdvancedWindowsSettings.DiskTimeout != 240 || settings.AdvancedWindowsSettings.LinkDownTime != 180 {
			t.Fatalf("unexpected advancedWindowsSettings: %+v", settings.AdvancedWindowsSettings)
		}
	})

	t.Run("initiator_name unset omits the key entirely from the wire body", func(t *testing.T) {
		data := baseVirtualMachineConnectionModel()
		settings, diags := buildIscsiExtensionSettings(ctx, data, targets)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		encoded, err := json.Marshal(settings)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		if strings.Contains(string(encoded), "initiatorName") {
			t.Fatalf("expected initiatorName omitted entirely, got %s", encoded)
		}
	})

	t.Run("JSON field names are pinned exactly", func(t *testing.T) {
		data := baseVirtualMachineConnectionModel()
		data.InitiatorName = types.StringValue("iqn.example:x")
		reapply, _ := types.ListValueFrom(ctx, reapplyOnBootObjectType, []ReapplyOnBootModel{{
			Enabled:                types.BoolValue(true),
			RebootOnReapplyAllowed: types.BoolValue(true),
		}})
		data.ReapplyOnBoot = reapply
		windows, _ := types.ListValueFrom(ctx, windowsSettingsObjectType, []WindowsSettingsModel{{
			DiskTimeoutInSeconds:  types.Int64Value(240),
			LinkDownTimeInSeconds: types.Int64Value(180),
		}})
		data.WindowsSettings = windows

		settings, diags := buildIscsiExtensionSettings(ctx, data, targets[:1])
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		encoded, err := json.Marshal(settings)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}

		var decoded map[string]any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		for _, key := range []string{"targets", "replaceTargets", "initiatorName", "sessionCount", "reboot", "autoUpdateConfiguration", "advancedWindowsSettings"} {
			if _, ok := decoded[key]; !ok {
				t.Fatalf("expected key %q in encoded settings %s", key, encoded)
			}
		}
		auc, ok := decoded["autoUpdateConfiguration"].(map[string]any)
		if !ok {
			t.Fatalf("autoUpdateConfiguration is not an object: %s", encoded)
		}
		for _, key := range []string{"enabled", "allowRebootOnAutoUpdate"} {
			if _, ok := auc[key]; !ok {
				t.Fatalf("expected key %q in autoUpdateConfiguration %v", key, auc)
			}
		}
		aws, ok := decoded["advancedWindowsSettings"].(map[string]any)
		if !ok {
			t.Fatalf("advancedWindowsSettings is not an object: %s", encoded)
		}
		for _, key := range []string{"diskTimeout", "linkDownTime"} {
			if _, ok := aws[key]; !ok {
				t.Fatalf("expected key %q in advancedWindowsSettings %v", key, aws)
			}
		}
		target0, ok := decoded["targets"].([]any)[0].(map[string]any)
		if !ok {
			t.Fatalf("targets[0] is not an object: %s", encoded)
		}
		for _, key := range []string{"ip", "port", "iqn"} {
			if _, ok := target0[key]; !ok {
				t.Fatalf("expected key %q in targets[0] %v", key, target0)
			}
		}
	})
}

func TestDestroyClearingIscsiSettings(t *testing.T) {
	settings := destroyClearingIscsiSettings()
	encoded, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("expected exactly targets and replaceTargets, got %v", decoded)
	}
	targets, ok := decoded["targets"].([]any)
	if !ok || len(targets) != 0 {
		t.Fatalf("expected targets: [], got %v", decoded["targets"])
	}
	if replaceTargets, ok := decoded["replaceTargets"].(bool); !ok || !replaceTargets {
		t.Fatalf("expected replaceTargets: true, got %v", decoded["replaceTargets"])
	}
}

func TestIscsiSettingsFromRaw(t *testing.T) {
	t.Run("nil settings return the zero value", func(t *testing.T) {
		settings, err := iscsiSettingsFromProperties(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(settings.Targets) != 0 || settings.ReplaceTargets || settings.SessionCount != 0 ||
			settings.InitiatorName != "" || settings.Reboot ||
			settings.AutoUpdateConfiguration != (autoUpdateConfigurationSetting{}) ||
			settings.AdvancedWindowsSettings != (advancedWindowsSettingsSetting{}) {
			t.Fatalf("expected zero value, got %+v", settings)
		}
	})

	t.Run("unknown extra keys, including legacy targetIps, are ignored", func(t *testing.T) {
		raw := map[string]any{
			"targets":        []any{map[string]any{"ip": "10.0.1.4", "port": float64(3260), "iqn": "iqn.x"}},
			"replaceTargets": true,
			"sessionCount":   float64(2),
			"targetIps":      []any{"10.0.1.4"},
			"debugSettings":  map[string]any{"logLevel": "debug"},
		}
		settings, err := iscsiSettingsFromProperties(&armcompute.VirtualMachineExtensionProperties{Settings: raw})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(settings.Targets) != 1 || settings.Targets[0].IP != "10.0.1.4" || settings.Targets[0].IQN != "iqn.x" {
			t.Fatalf("unexpected targets: %+v", settings.Targets)
		}
		if !settings.ReplaceTargets || settings.SessionCount != 2 {
			t.Fatalf("unexpected core fields: %+v", settings)
		}
	})
}

func TestSettingsAlreadyApplied(t *testing.T) {
	desired := iscsiExtensionSettings{
		Targets:        []iscsiTarget{{IP: "10.0.1.4", Port: 3260, IQN: "iqn.x"}},
		ReplaceTargets: true,
		SessionCount:   2,
	}

	t.Run("identical settings with Succeeded provisioning are skipped", func(t *testing.T) {
		current := desired
		if !settingsAlreadyApplied(current, desired, "Succeeded") {
			t.Fatal("expected settingsAlreadyApplied to return true")
		}
	})

	t.Run("a Portal-shaped extension (no replaceTargets) is not skipped even with matching targets", func(t *testing.T) {
		// A Portal-mounted extension's settings never carry replaceTargets: true.
		current := desired
		current.ReplaceTargets = false
		if settingsAlreadyApplied(current, desired, "Succeeded") {
			t.Fatal("expected settingsAlreadyApplied to return false for a Portal-shaped (no replaceTargets) extension")
		}
	})

	t.Run("a Failed provisioning state is never skipped, even with identical settings", func(t *testing.T) {
		current := desired
		if settingsAlreadyApplied(current, desired, "Failed") {
			t.Fatal("expected settingsAlreadyApplied to return false after a failed provisioning attempt")
		}
	})

	t.Run("differing settings with Succeeded provisioning are not skipped", func(t *testing.T) {
		current := desired
		current.SessionCount = 3
		if settingsAlreadyApplied(current, desired, "Succeeded") {
			t.Fatal("expected settingsAlreadyApplied to return false for differing settings")
		}
	})
}

func TestApplyExtensionCoreSettings(t *testing.T) {
	ctx := context.Background()

	t.Run("initiator name empty maps to null, not an empty string", func(t *testing.T) {
		data := baseVirtualMachineConnectionModel()
		diags := applyExtensionCoreSettings(ctx, &data, iscsiExtensionSettings{SessionCount: 2})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if !data.InitiatorName.IsNull() {
			t.Fatalf("expected null initiator_name, got %v", data.InitiatorName)
		}
	})

	t.Run("a populated initiator name round-trips", func(t *testing.T) {
		data := baseVirtualMachineConnectionModel()
		diags := applyExtensionCoreSettings(ctx, &data, iscsiExtensionSettings{
			SessionCount:  2,
			InitiatorName: "iqn.example:x",
		})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if data.InitiatorName.ValueString() != "iqn.example:x" {
			t.Fatalf("unexpected initiator_name: %v", data.InitiatorName)
		}
	})

	t.Run("empty targets maps to an empty (not null) resolved_targets list", func(t *testing.T) {
		data := baseVirtualMachineConnectionModel()
		diags := applyExtensionCoreSettings(ctx, &data, iscsiExtensionSettings{SessionCount: 2})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if data.ResolvedTargets.IsNull() {
			t.Fatal("expected a known (possibly empty) list, not null")
		}
		if len(data.ResolvedTargets.Elements()) != 0 {
			t.Fatalf("expected zero elements, got %d", len(data.ResolvedTargets.Elements()))
		}
	})
}

func TestReapplyOnBootFromAPI(t *testing.T) {
	ctx := context.Background()
	nullPrior := types.ListNull(reapplyOnBootObjectType)

	t.Run("all-false settings preserve a null prior as absent", func(t *testing.T) {
		got, diags := reapplyOnBootFromAPI(ctx, autoUpdateConfigurationSetting{}, nullPrior)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if !got.IsNull() {
			t.Fatalf("expected null, got %v", got)
		}
	})

	t.Run("all-false settings preserve an explicitly-false prior block", func(t *testing.T) {
		prior, diags := types.ListValueFrom(ctx, reapplyOnBootObjectType, []ReapplyOnBootModel{{
			Enabled:                types.BoolValue(false),
			RebootOnReapplyAllowed: types.BoolValue(false),
		}})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		got, diags := reapplyOnBootFromAPI(ctx, autoUpdateConfigurationSetting{}, prior)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if len(got.Elements()) != 1 {
			t.Fatalf("expected the prior's explicit block preserved, got %v", got)
		}
	})

	t.Run("an enabled response is unambiguous regardless of prior", func(t *testing.T) {
		got, diags := reapplyOnBootFromAPI(ctx, autoUpdateConfigurationSetting{Enabled: true}, nullPrior)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if len(got.Elements()) != 1 {
			t.Fatalf("expected a present block, got %v", got)
		}
	})

	t.Run("all-false settings surface as drift against an enabled prior block", func(t *testing.T) {
		prior, diags := types.ListValueFrom(ctx, reapplyOnBootObjectType, []ReapplyOnBootModel{{
			Enabled:                types.BoolValue(true),
			RebootOnReapplyAllowed: types.BoolValue(false),
		}})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		got, diags := reapplyOnBootFromAPI(ctx, autoUpdateConfigurationSetting{}, prior)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		elems, diags := blockElements[ReapplyOnBootModel](ctx, got)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if len(elems) != 1 || elems[0].Enabled.ValueBool() {
			t.Fatalf("expected the live all-false value recorded (not the enabled prior echoed back), got %+v", elems)
		}
	})
}

func TestWindowsSettingsFromAPI(t *testing.T) {
	ctx := context.Background()
	nullPrior := types.ListNull(windowsSettingsObjectType)

	t.Run("both zero preserve a null prior as absent", func(t *testing.T) {
		got, diags := windowsSettingsFromAPI(ctx, advancedWindowsSettingsSetting{}, nullPrior)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if !got.IsNull() {
			t.Fatalf("expected null, got %v", got)
		}
	})

	t.Run("one non-zero field maps the other back to null within a present block", func(t *testing.T) {
		got, diags := windowsSettingsFromAPI(ctx, advancedWindowsSettingsSetting{DiskTimeout: 240}, nullPrior)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		elems, diags := blockElements[WindowsSettingsModel](ctx, got)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if len(elems) != 1 || elems[0].DiskTimeoutInSeconds.ValueInt64() != 240 || !elems[0].LinkDownTimeInSeconds.IsNull() {
			t.Fatalf("unexpected mapping: %+v", elems)
		}
	})

	t.Run("both-zero settings surface as drift against a configured prior block", func(t *testing.T) {
		prior, diags := types.ListValueFrom(ctx, windowsSettingsObjectType, []WindowsSettingsModel{{
			DiskTimeoutInSeconds:  types.Int64Value(240),
			LinkDownTimeInSeconds: types.Int64Null(),
		}})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		got, diags := windowsSettingsFromAPI(ctx, advancedWindowsSettingsSetting{}, prior)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		elems, diags := blockElements[WindowsSettingsModel](ctx, got)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if len(elems) != 1 || !elems[0].DiskTimeoutInSeconds.IsNull() {
			t.Fatalf("expected the live both-zero value recorded (not the configured prior echoed back), got %+v", elems)
		}
	})
}

func TestExtensionTypeForVM(t *testing.T) {
	vmWith := func(os armcompute.OperatingSystemTypes) armcompute.VirtualMachine {
		return armcompute.VirtualMachine{
			ID: to.Ptr("vm1"),
			Properties: &armcompute.VirtualMachineProperties{
				StorageProfile: &armcompute.StorageProfile{OSDisk: &armcompute.OSDisk{OSType: to.Ptr(os)}},
			},
		}
	}

	t.Run("Linux maps to MountIscsiLinux", func(t *testing.T) {
		extensionType, diags := extensionTypeForVM(vmWith(armcompute.OperatingSystemTypesLinux))
		if diags.HasError() || extensionType != extensionTypeLinux {
			t.Fatalf("got (%q, %v)", extensionType, diags)
		}
	})

	t.Run("Windows maps to MountIscsiWindows", func(t *testing.T) {
		extensionType, diags := extensionTypeForVM(vmWith(armcompute.OperatingSystemTypesWindows))
		if diags.HasError() || extensionType != extensionTypeWindows {
			t.Fatalf("got (%q, %v)", extensionType, diags)
		}
	})

	t.Run("a missing OSDisk/OSType is an error", func(t *testing.T) {
		_, diags := extensionTypeForVM(armcompute.VirtualMachine{ID: to.Ptr("vm1")})
		if !diags.HasError() {
			t.Fatal("expected an error")
		}
	})

	t.Run("an unsupported OS type is an error", func(t *testing.T) {
		_, diags := extensionTypeForVM(vmWith(armcompute.OperatingSystemTypes("FreeBSD")))
		if !diags.HasError() {
			t.Fatal("expected an error")
		}
	})
}

func TestClassifyInstanceView(t *testing.T) {
	status := func(code, level, message string) *armcompute.VirtualMachineExtensionInstanceView {
		return &armcompute.VirtualMachineExtensionInstanceView{
			Statuses: []*armcompute.InstanceViewStatus{{
				Code:    to.Ptr(code),
				Level:   to.Ptr(armcompute.StatusLevelTypes(level)),
				Message: to.Ptr(message),
			}},
		}
	}

	windowsFailMessage := "Enable failed: iSCSI configuration failed for steps: iscsi-mount.\nDetails: iscsi-mount: " +
		"setting up iSCSI sessions: could not connect to iSCSI target 10.250.142.132:3260, " +
		"iqn.2010-01.com.purestorage.psc:8d4a9d0e421a1464e218-apriadko-empty-vg \u2013 make sure it exists and has volumes."

	tests := []struct {
		name        string
		view        *armcompute.VirtualMachineExtensionInstanceView
		want        instanceViewOutcome
		wantCode    string
		wantMessage string
	}{
		{name: "missing: nil view", want: instanceViewMissing},
		{name: "missing: no statuses", view: &armcompute.VirtualMachineExtensionInstanceView{}, want: instanceViewMissing},
		{name: "missing: nil status", view: &armcompute.VirtualMachineExtensionInstanceView{Statuses: []*armcompute.InstanceViewStatus{nil}}, want: instanceViewMissing},
		{
			name: "succeeded with the handler's own message is success",
			view: status("ProvisioningState/succeeded", "Info", "Enable succeeded: iSCSI configuration completed successfully"),
			want: instanceViewSuccess, wantCode: "ProvisioningState/succeeded", wantMessage: "Enable succeeded: iSCSI configuration completed successfully",
		},
		{
			name: "succeeded with any other message is success",
			view: status("ProvisioningState/succeeded", "Info", "Enable succeeded: something new"),
			want: instanceViewSuccess, wantCode: "ProvisioningState/succeeded", wantMessage: "Enable succeeded: something new",
		},
		{
			name: "succeeded while rebooting is in progress",
			view: status("ProvisioningState/succeeded", "Info", "Rebooting as requested..."),
			want: instanceViewInProgress, wantCode: "ProvisioningState/succeeded", wantMessage: "Rebooting as requested...",
		},
		{
			name: "succeeded with reboot required is reboot required",
			view: status("ProvisioningState/succeeded", "Info", "Enable succeeded: To finish iSCSI configuration, reboot is required."),
			want: instanceViewRebootRequired, wantCode: "ProvisioningState/succeeded", wantMessage: "Enable succeeded: To finish iSCSI configuration, reboot is required.",
		},
		{
			name: "failed with reboot required is reboot required, not a failure",
			view: status("ProvisioningState/failed/0", "Error", "Enable failed: To finish iSCSI configuration, reboot is required."),
			want: instanceViewRebootRequired, wantCode: "ProvisioningState/failed/0", wantMessage: "Enable failed: To finish iSCSI configuration, reboot is required.",
		},
		{
			name: "transitioning after the reboot is in progress",
			view: status("ProvisioningState/transitioning", "Info", "Enable in progress: Setting up iSCSI configuration"),
			want: instanceViewInProgress, wantCode: "ProvisioningState/transitioning", wantMessage: "Enable in progress: Setting up iSCSI configuration",
		},
		{
			name: "transitioning with any message is in progress",
			view: status("ProvisioningState/transitioning", "Info", "Enable succeeded: iSCSI configuration completed successfully"),
			want: instanceViewInProgress, wantCode: "ProvisioningState/transitioning", wantMessage: "Enable succeeded: iSCSI configuration completed successfully",
		},
		{
			name: "failed with the live Windows message is failed",
			view: status("ProvisioningState/failed/0", "Error", windowsFailMessage),
			want: instanceViewFailed, wantCode: "ProvisioningState/failed/0", wantMessage: windowsFailMessage,
		},
		{
			name: "failed with a message that looks like success is still failed",
			view: status("ProvisioningState/failed/0", "Error", "Enable succeeded: iSCSI configuration completed successfully"),
			want: instanceViewFailed, wantCode: "ProvisioningState/failed/0", wantMessage: "Enable succeeded: iSCSI configuration completed successfully",
		},
		{
			name: "the code is matched case-insensitively",
			view: status("provisioningstate/FAILED/0", "Error", "boom"),
			want: instanceViewFailed, wantCode: "provisioningstate/FAILED/0", wantMessage: "boom",
		},
		{
			name: "warning is unrecognized",
			view: status("ProvisioningState/warning", "Warning", "Enable succeeded: iSCSI configuration completed successfully"),
			want: instanceViewUnrecognized, wantCode: "ProvisioningState/warning", wantMessage: "Enable succeeded: iSCSI configuration completed successfully",
		},
		{
			name: "an unknown code is unrecognized",
			view: status("ProvisioningState/updating", "Info", "Enable in progress: Setting up iSCSI configuration"),
			want: instanceViewUnrecognized, wantCode: "ProvisioningState/updating", wantMessage: "Enable in progress: Setting up iSCSI configuration",
		},
		{
			name: "an empty code is unrecognized",
			view: status("", "Info", "Enable succeeded: iSCSI configuration completed successfully"),
			want: instanceViewUnrecognized, wantMessage: "Enable succeeded: iSCSI configuration completed successfully",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, code, message := classifyInstanceView(tt.view)
			if got != tt.want {
				t.Fatalf("classifyInstanceView() = %v, want %v", got, tt.want)
			}
			if code != tt.wantCode || message != tt.wantMessage {
				t.Fatalf("classifyInstanceView() code, message = %q, %q; want %q, %q", code, message, tt.wantCode, tt.wantMessage)
			}
		})
	}
}

func TestClassifyLROFailure(t *testing.T) {
	tests := []struct {
		name    string
		getErr  error
		outcome instanceViewOutcome
		message string
		want    lroFailureAction
	}{
		{"a failed instance-view GET falls back to the LRO error", errors.New("boom"), instanceViewFailed, "some message", lroFailureUseLROError},
		{"reboot required is its own, non-error outcome", nil, instanceViewRebootRequired, "reboot text", lroFailureRebootRequired},
		{"a failed status with a message supplies it", nil, instanceViewFailed, "iSCSI configuration failed for steps: iscsi-setup.", lroFailureUseInstanceViewMessage},
		{"a failed status with no message falls back to the LRO error", nil, instanceViewFailed, "", lroFailureUseLROError},
		{"a stale success status falls back to the LRO error, not its own text", nil, instanceViewSuccess, "iSCSI configuration completed successfully", lroFailureUseLROError},
		{"a stale in-progress status falls back to the LRO error", nil, instanceViewInProgress, "Rebooting as requested...", lroFailureUseLROError},
		{"an unrecognized status falls back to the LRO error", nil, instanceViewUnrecognized, "some message", lroFailureUseLROError},
		{"a missing instance view falls back to the LRO error", nil, instanceViewMissing, "", lroFailureUseLROError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyLROFailure(tt.getErr, tt.outcome, tt.message)
			if got != tt.want {
				t.Fatalf("classifyLROFailure() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClearingPutFailureIsHarmless(t *testing.T) {
	tests := []struct {
		name    string
		getErr  error
		outcome instanceViewOutcome
		want    bool
	}{
		{"reboot required after the GET succeeds is harmless", nil, instanceViewRebootRequired, true},
		{"the GET itself failing is not harmless", errors.New("boom"), instanceViewRebootRequired, false},
		{"a genuinely failed instance view is not harmless", nil, instanceViewFailed, false},
		{"a stale success instance view is not harmless", nil, instanceViewSuccess, false},
		{"a stale in-progress instance view is not harmless", nil, instanceViewInProgress, false},
		{"an unrecognized instance view is not harmless", nil, instanceViewUnrecognized, false},
		{"a missing instance view is not harmless", nil, instanceViewMissing, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clearingPutFailureIsHarmless(tt.getErr, tt.outcome)
			if got != tt.want {
				t.Fatalf("clearingPutFailureIsHarmless() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPreserveCaseInsensitiveSpelling(t *testing.T) {
	t.Run("a null prior takes ARM's spelling", func(t *testing.T) {
		got := preserveCaseInsensitiveSpelling(types.StringNull(), "MountIscsiLinux-deploy")
		if got.ValueString() != "MountIscsiLinux-deploy" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("an unknown prior takes ARM's spelling", func(t *testing.T) {
		got := preserveCaseInsensitiveSpelling(types.StringUnknown(), "MountIscsiLinux-deploy")
		if got.ValueString() != "MountIscsiLinux-deploy" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("a prior differing from ARM only by case is kept as-is", func(t *testing.T) {
		prior := types.StringValue("mountiscsilinux-deploy")
		got := preserveCaseInsensitiveSpelling(prior, "MountIscsiLinux-Deploy")
		if !got.Equal(prior) {
			t.Fatalf("got %v, want the prior spelling %v kept", got, prior)
		}
	})

	t.Run("a genuinely different prior is replaced with ARM's spelling", func(t *testing.T) {
		got := preserveCaseInsensitiveSpelling(types.StringValue("custom-name"), "MountIscsiLinux-deploy")
		if got.ValueString() != "MountIscsiLinux-deploy" {
			t.Fatalf("got %v", got)
		}
	})
}

func TestApplyExtensionIdentityPreservesCasing(t *testing.T) {
	data := baseVirtualMachineConnectionModel()
	priorID := armids.IDValue{StringValue: types.StringValue(
		"/subscriptions/sub/resourcegroups/rg/providers/microsoft.compute/virtualmachines/vm1/extensions/ext1")}
	data.ID = priorID
	data.ExtensionName = types.StringValue("ext1")

	ext := armcompute.VirtualMachineExtension{
		ID:   to.Ptr("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1/extensions/ext1"),
		Name: to.Ptr("Ext1"),
	}
	applyExtensionIdentity(&data, ext)

	// The framework keeps prior's id spelling because the two compare semantically equal.
	if equal, diags := priorID.StringSemanticEquals(context.Background(), data.ID); diags.HasError() || !equal {
		t.Fatalf("expected ARM's id to be semantically equal to prior's spelling, got %v", data.ID)
	}
	if data.ExtensionName.ValueString() != "ext1" {
		t.Fatalf("expected data's own extension_name casing kept, got %v", data.ExtensionName)
	}

	// A genuinely different name is taken from ARM.
	ext.Name = to.Ptr("a-different-name")
	applyExtensionIdentity(&data, ext)
	if data.ExtensionName.ValueString() != "a-different-name" {
		t.Fatalf("expected the genuinely different name to be taken from ARM, got %v", data.ExtensionName)
	}
}

func TestDuplicateExtensionCause(t *testing.T) {
	vmID := armidsTestVMID(testSubscriptionID)

	t.Run("names the existing extension's ID when its name is known", func(t *testing.T) {
		existing := &armcompute.VirtualMachineExtension{Name: to.Ptr("MountIscsiLinux-deploy")}
		existingID, err := extensionFullID(vmID, "MountIscsiLinux-deploy")
		if err != nil {
			t.Fatalf("composing the extension ID: %v", err)
		}
		if got, want := duplicateExtensionCause(vmID, existing, extensionTypeLinux), newExtensionAlreadyExistsError(existingID); got != want {
			t.Fatalf("duplicateExtensionCause() = %q, want %q", got, want)
		}
	})

	t.Run("falls back to the guidance for an unknown name when none is known (the race path)", func(t *testing.T) {
		want := newExtensionOfTypeAlreadyExistsError(vmID, extensionTypeLinux, nil)
		for name, existing := range map[string]*armcompute.VirtualMachineExtension{
			"no extension": nil,
			"no name":      {},
		} {
			if got := duplicateExtensionCause(vmID, existing, extensionTypeLinux); got != want {
				t.Errorf("%s: duplicateExtensionCause() = %q, want %q", name, got, want)
			}
		}
	})

	t.Run("falls back to the guidance for an unknown name when the VM ID does not parse", func(t *testing.T) {
		existing := &armcompute.VirtualMachineExtension{Name: to.Ptr("MountIscsiLinux-deploy")}
		if got, want := duplicateExtensionCause("not-an-id", existing, extensionTypeLinux), newExtensionOfTypeAlreadyExistsError("not-an-id", extensionTypeLinux, nil); got != want {
			t.Fatalf("duplicateExtensionCause() = %q, want %q", got, want)
		}
	})
}

func TestDefaultExtensionName(t *testing.T) {
	if got := defaultExtensionName(); got != "PureMountIscsi" {
		t.Fatalf("defaultExtensionName() = %q", got)
	}
}

func TestWarnWindowsOnlyAttributesOnLinux(t *testing.T) {
	ctx := context.Background()

	windowsSettingsSet, diags := types.ListValueFrom(ctx, windowsSettingsObjectType, []WindowsSettingsModel{{
		DiskTimeoutInSeconds:  types.Int64Value(240),
		LinkDownTimeInSeconds: types.Int64Value(180),
	}})
	if diags.HasError() {
		t.Fatalf("building the windows_settings fixture: %v", diags)
	}

	tests := []struct {
		name          string
		extensionType string
		data          VirtualMachineConnectionResourceModel
		wantWarnings  int
	}{
		{
			name:          "Linux with windows_settings configured warns",
			extensionType: extensionTypeLinux,
			data: func() VirtualMachineConnectionResourceModel {
				m := baseVirtualMachineConnectionModel()
				m.WindowsSettings = windowsSettingsSet
				return m
			}(),
			wantWarnings: 1,
		},
		{
			name:          "Linux with reboot_allowed = true warns",
			extensionType: extensionTypeLinux,
			data: func() VirtualMachineConnectionResourceModel {
				m := baseVirtualMachineConnectionModel()
				m.RebootAllowed = types.BoolValue(true)
				return m
			}(),
			wantWarnings: 1,
		},
		{
			name:          "Windows never warns, even with both set",
			extensionType: extensionTypeWindows,
			data: func() VirtualMachineConnectionResourceModel {
				m := baseVirtualMachineConnectionModel()
				m.RebootAllowed = types.BoolValue(true)
				m.WindowsSettings = windowsSettingsSet
				return m
			}(),
			wantWarnings: 0,
		},
		{
			name:          "Linux with every default (reboot_allowed false, no windows_settings) warns about neither",
			extensionType: extensionTypeLinux,
			data:          baseVirtualMachineConnectionModel(),
			wantWarnings:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diags := warnWindowsOnlyAttributesOnLinux(tt.extensionType, tt.data)
			if len(diags) != tt.wantWarnings {
				t.Fatalf("got %d diagnostics, want %d: %v", len(diags), tt.wantWarnings, diags)
			}
			for _, d := range diags {
				if d.Severity() != diag.SeverityWarning {
					t.Fatalf("expected a warning severity, got %v: %s", d.Severity(), d.Summary())
				}
			}
		})
	}
}

// TestRebootRequiredWarning asserts the diagnostic Create/Update/Read raise for the
// reboot-required outcome is a warning, not an error, and names the handler's own
// message.
func TestRebootRequiredWarning(t *testing.T) {
	d := rebootRequiredWarning()
	if d.Severity() != diag.SeverityWarning {
		t.Fatalf("expected a warning severity, got %v", d.Severity())
	}
	if !strings.Contains(d.Detail(), handlerMessageRebootRequired) {
		t.Fatalf("expected the detail to include the handler's own reboot-required message, got %q", d.Detail())
	}
}

func TestForceUpdateTagFor(t *testing.T) {
	t.Run("a Failed extension gets a fresh tag each time", func(t *testing.T) {
		first, second := forceUpdateTagFor("Failed"), forceUpdateTagFor("Failed")
		if first == nil || second == nil || *first == "" || *first == *second {
			t.Fatalf("got %v and %v, want two different non-empty tags", first, second)
		}
	})

	for _, state := range []string{"Succeeded", "Updating", "Creating", ""} {
		t.Run("no tag for "+state, func(t *testing.T) {
			if tag := forceUpdateTagFor(state); tag != nil {
				t.Fatalf("got %q, want no tag", *tag)
			}
		})
	}
}
