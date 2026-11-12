// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

// Extension identity: publisher and the two type names, one per operating system.
const (
	extensionPublisher          = "PureStorage.Extensions"
	extensionTypeLinux          = "MountIscsiLinux"
	extensionTypeWindows        = "MountIscsiWindows"
	extensionTypeHandlerVersion = "1.0"
)

// isMountExtension reports whether a publisher and extension type name this resource's
// extension: its publisher and one of its two types. It is the one definition of "ours",
// used by Read to detect a replaced extension and by a move to refuse a foreign one.
func isMountExtension(publisher string, extensionType string) bool {
	return publisher == extensionPublisher && (extensionType == extensionTypeLinux || extensionType == extensionTypeWindows)
}

// Handler messages, matched as substrings of the instance view status message. Success
// and a Windows setup that is rebooting the guest report the same status code, so only
// the message tells them apart; reboot-required is matched in the message too.
const (
	handlerMessageRebooting      = "Rebooting as requested..."
	handlerMessageRebootRequired = "To finish iSCSI configuration, reboot is required."
)

// VirtualMachineConnectionResourceModel is the Terraform-side representation of
// everpure_azure_virtual_machine_connection.
type VirtualMachineConnectionResourceModel struct {
	ID                armids.IDValue `tfsdk:"id"`
	VirtualMachineID  armids.IDValue `tfsdk:"virtual_machine_id"`
	VolumeGroupIDs    types.Set      `tfsdk:"volume_group_ids"`
	IscsiSessionCount types.Int64    `tfsdk:"iscsi_session_count"`
	InitiatorName     types.String   `tfsdk:"initiator_name"`
	RebootAllowed     types.Bool     `tfsdk:"reboot_allowed"`
	ExtensionName     types.String   `tfsdk:"extension_name"`
	ExtensionType     types.String   `tfsdk:"extension_type"`
	ResolvedTargets   types.List     `tfsdk:"resolved_targets"`
	ReapplyOnBoot     types.List     `tfsdk:"reapply_on_boot"`
	WindowsSettings   types.List     `tfsdk:"windows_settings"`
	Timeouts          timeouts.Value `tfsdk:"timeouts"`
}

// ReapplyOnBootModel is the "reapply_on_boot" nested block.
type ReapplyOnBootModel struct {
	Enabled                types.Bool `tfsdk:"enabled"`
	RebootOnReapplyAllowed types.Bool `tfsdk:"reboot_on_reapply_allowed"`
}

// WindowsSettingsModel is the "windows_settings" nested block.
type WindowsSettingsModel struct {
	DiskTimeoutInSeconds  types.Int64 `tfsdk:"disk_timeout_in_seconds"`
	LinkDownTimeInSeconds types.Int64 `tfsdk:"link_down_time_in_seconds"`
}

// ResolvedTargetModel is one element of the computed "resolved_targets" list.
type ResolvedTargetModel struct {
	IP   types.String `tfsdk:"ip"`
	IQN  types.String `tfsdk:"iqn"`
	Port types.Int64  `tfsdk:"port"`
}

var resolvedTargetObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"ip":   types.StringType,
	"iqn":  types.StringType,
	"port": types.Int64Type,
}}

var reapplyOnBootObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"enabled":                   types.BoolType,
	"reboot_on_reapply_allowed": types.BoolType,
}}

var windowsSettingsObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"disk_timeout_in_seconds":   types.Int64Type,
	"link_down_time_in_seconds": types.Int64Type,
}}

// Extension public settings, with json tags matching the extension's settings schema.

type autoUpdateConfigurationSetting struct {
	Enabled                 bool `json:"enabled"`
	AllowRebootOnAutoUpdate bool `json:"allowRebootOnAutoUpdate"`
}

type advancedWindowsSettingsSetting struct {
	DiskTimeout  int64 `json:"diskTimeout"`
	LinkDownTime int64 `json:"linkDownTime"`
}

// iscsiExtensionSettings is the full settings object Create and Update send. Every
// field is always present, with unset attributes sent as the extension's own defaults.
type iscsiExtensionSettings struct {
	Targets                 []iscsiTarget                  `json:"targets"`
	ReplaceTargets          bool                           `json:"replaceTargets"`
	InitiatorName           string                         `json:"initiatorName,omitempty"`
	SessionCount            int64                          `json:"sessionCount"`
	Reboot                  bool                           `json:"reboot"`
	AutoUpdateConfiguration autoUpdateConfigurationSetting `json:"autoUpdateConfiguration"`
	AdvancedWindowsSettings advancedWindowsSettingsSetting `json:"advancedWindowsSettings"`
}

// destroyClearingIscsiSettingsBody is the settings body destroy sends before deleting
// the extension: an empty targets list with replaceTargets: true logs out every session.
type destroyClearingIscsiSettingsBody struct {
	Targets        []iscsiTarget `json:"targets"`
	ReplaceTargets bool          `json:"replaceTargets"`
}

func destroyClearingIscsiSettings() destroyClearingIscsiSettingsBody {
	return destroyClearingIscsiSettingsBody{Targets: []iscsiTarget{}, ReplaceTargets: true}
}

// buildIscsiExtensionSettings builds the settings object Create and Update send, from
// the planned configuration and the targets resolveVMExtensionTargets already resolved.
func buildIscsiExtensionSettings(
	ctx context.Context, data VirtualMachineConnectionResourceModel, targets []iscsiTarget,
) (iscsiExtensionSettings, diag.Diagnostics) {
	var diags diag.Diagnostics

	reapply, d := blockElements[ReapplyOnBootModel](ctx, data.ReapplyOnBoot)
	diags.Append(d...)
	var autoUpdate autoUpdateConfigurationSetting
	if len(reapply) == 1 {
		autoUpdate = autoUpdateConfigurationSetting{
			Enabled:                 reapply[0].Enabled.ValueBool(),
			AllowRebootOnAutoUpdate: reapply[0].RebootOnReapplyAllowed.ValueBool(),
		}
	}

	windows, d := blockElements[WindowsSettingsModel](ctx, data.WindowsSettings)
	diags.Append(d...)
	var advanced advancedWindowsSettingsSetting
	if len(windows) == 1 {
		advanced = advancedWindowsSettingsSetting{
			DiskTimeout:  windows[0].DiskTimeoutInSeconds.ValueInt64(),
			LinkDownTime: windows[0].LinkDownTimeInSeconds.ValueInt64(),
		}
	}

	settings := iscsiExtensionSettings{
		Targets:                 targets,
		ReplaceTargets:          true,
		SessionCount:            data.IscsiSessionCount.ValueInt64(),
		Reboot:                  data.RebootAllowed.ValueBool(),
		AutoUpdateConfiguration: autoUpdate,
		AdvancedWindowsSettings: advanced,
	}
	if !data.InitiatorName.IsNull() {
		settings.InitiatorName = data.InitiatorName.ValueString()
	}

	return settings, diags
}

// iscsiSettingsFromProperties decodes the extension's returned settings, ignoring
// unknown keys. Targets are sorted into the same (iqn, ip, port) order
// resolveVMExtensionTargets produces: the extension isn't guaranteed to echo them back
// in the order they were sent, and leaving that order as-is would make resolved_targets
// flap on every subsequent plan and make settingsAlreadyApplied's comparison spuriously
// mismatch.
func iscsiSettingsFromProperties(props *armcompute.VirtualMachineExtensionProperties) (iscsiExtensionSettings, error) {
	var settings iscsiExtensionSettings
	if props == nil || props.Settings == nil {
		return settings, nil
	}
	encoded, err := json.Marshal(props.Settings)
	if err != nil {
		return settings, err
	}
	if err := json.Unmarshal(encoded, &settings); err != nil {
		return settings, err
	}
	sortISCSITargets(settings.Targets)
	return settings, nil
}

// settingsAlreadyApplied reports whether Update can skip its PUT: the extension's live
// settings equal what would be sent and its last provisioning succeeded.
func settingsAlreadyApplied(current, desired iscsiExtensionSettings, currentProvisioningState string) bool {
	return currentProvisioningState == "Succeeded" && reflect.DeepEqual(current, desired)
}

// forceUpdateTagFor returns the forceUpdateTag Update sends with its PUT: a fresh value
// when the extension's last provisioning failed, nil otherwise. A PUT identical to the
// stored model of a Failed extension gets no new sequence number, so the guest never
// runs the extension again and the failed step is never retried; a changed forceUpdateTag
// makes it a new request. After a success an identical PUT is already skipped, and a PUT
// with changed settings is a new request by itself, so the tag would only add re-runs.
func forceUpdateTagFor(currentProvisioningState string) *string {
	if currentProvisioningState != "Failed" {
		return nil
	}
	return to.Ptr(uuid.NewString())
}

func resolvedTargetsToList(ctx context.Context, targets []iscsiTarget) (types.List, diag.Diagnostics) {
	models := make([]ResolvedTargetModel, 0, len(targets))
	for _, t := range targets {
		models = append(models, ResolvedTargetModel{
			IP:   types.StringValue(t.IP),
			IQN:  types.StringValue(t.IQN),
			Port: types.Int64Value(int64(t.Port)),
		})
	}
	return types.ListValueFrom(ctx, resolvedTargetObjectType, models)
}

// applyExtensionCoreSettings maps the unambiguous fields of a read-back settings
// object onto data.
func applyExtensionCoreSettings(ctx context.Context, data *VirtualMachineConnectionResourceModel, settings iscsiExtensionSettings) diag.Diagnostics {
	var diags diag.Diagnostics

	resolvedTargets, d := resolvedTargetsToList(ctx, settings.Targets)
	diags.Append(d...)
	data.ResolvedTargets = resolvedTargets

	data.IscsiSessionCount = types.Int64Value(settings.SessionCount)
	if settings.InitiatorName == "" {
		data.InitiatorName = types.StringNull()
	} else {
		data.InitiatorName = types.StringValue(settings.InitiatorName)
	}
	data.RebootAllowed = types.BoolValue(settings.Reboot)

	return diags
}

// reapplyOnBootFromAPI maps autoUpdateConfiguration back onto the reapply_on_boot
// block. An omitted block and an all-false block send the same wire settings, so an
// all-false response only collapses to null when prior was already empty (the block
// was never configured); once a block was configured, an all-false live response is
// itself a real value (a possible sign of drift) and must still be recorded, not
// replaced with prior's stale value.
func reapplyOnBootFromAPI(ctx context.Context, auc autoUpdateConfigurationSetting, prior types.List) (types.List, diag.Diagnostics) {
	if !auc.Enabled && !auc.AllowRebootOnAutoUpdate && len(prior.Elements()) == 0 {
		return types.ListNull(reapplyOnBootObjectType), nil
	}
	return types.ListValueFrom(ctx, reapplyOnBootObjectType, []ReapplyOnBootModel{{
		Enabled:                types.BoolValue(auc.Enabled),
		RebootOnReapplyAllowed: types.BoolValue(auc.AllowRebootOnAutoUpdate),
	}})
}

// windowsSettingsFromAPI maps advancedWindowsSettings back onto the windows_settings
// block. A 0 means the field was unset; both-zero only collapses to null when prior
// was already empty, for the same reason reapplyOnBootFromAPI keeps a live value once
// the block was configured: see its comment.
func windowsSettingsFromAPI(ctx context.Context, aws advancedWindowsSettingsSetting, prior types.List) (types.List, diag.Diagnostics) {
	if aws.DiskTimeout == 0 && aws.LinkDownTime == 0 && len(prior.Elements()) == 0 {
		return types.ListNull(windowsSettingsObjectType), nil
	}
	diskTimeout := types.Int64Null()
	if aws.DiskTimeout != 0 {
		diskTimeout = types.Int64Value(aws.DiskTimeout)
	}
	linkDownTime := types.Int64Null()
	if aws.LinkDownTime != 0 {
		linkDownTime = types.Int64Value(aws.LinkDownTime)
	}
	return types.ListValueFrom(ctx, windowsSettingsObjectType, []WindowsSettingsModel{{
		DiskTimeoutInSeconds:  diskTimeout,
		LinkDownTimeInSeconds: linkDownTime,
	}})
}

// applyExtensionAmbiguousBlocks maps reapply_on_boot and windows_settings from a
// read-back settings object, using data's current value as prior. Read only: Create and
// Update must save the plan's values unchanged.
func applyExtensionAmbiguousBlocks(ctx context.Context, data *VirtualMachineConnectionResourceModel, settings iscsiExtensionSettings) diag.Diagnostics {
	var diags diag.Diagnostics

	reapply, d := reapplyOnBootFromAPI(ctx, settings.AutoUpdateConfiguration, data.ReapplyOnBoot)
	diags.Append(d...)
	data.ReapplyOnBoot = reapply

	windows, d := windowsSettingsFromAPI(ctx, settings.AdvancedWindowsSettings, data.WindowsSettings)
	diags.Append(d...)
	data.WindowsSettings = windows

	return diags
}

// applyExtensionIdentity maps the extension envelope's own id/name/type onto data.
func applyExtensionIdentity(data *VirtualMachineConnectionResourceModel, ext armcompute.VirtualMachineExtension) {
	data.ID = armids.IDValue{StringValue: types.StringPointerValue(ext.ID)}
	if ext.Name != nil {
		data.ExtensionName = preserveCaseInsensitiveSpelling(data.ExtensionName, *ext.Name)
	}
	if ext.Properties != nil && ext.Properties.Type != nil {
		data.ExtensionType = types.StringValue(*ext.Properties.Type)
	}
}

// preserveCaseInsensitiveSpelling returns prior when it differs from fromAPI only in
// case, otherwise fromAPI.
func preserveCaseInsensitiveSpelling(prior types.String, fromAPI string) types.String {
	if !prior.IsNull() && !prior.IsUnknown() && strings.EqualFold(prior.ValueString(), fromAPI) {
		return prior
	}
	return types.StringValue(fromAPI)
}

// applyExtensionSettings maps an extension response's id, name, type and unambiguous
// settings fields onto data.
func applyExtensionSettings(ctx context.Context, data *VirtualMachineConnectionResourceModel, ext armcompute.VirtualMachineExtension) diag.Diagnostics {
	var diags diag.Diagnostics
	applyExtensionIdentity(data, ext)

	settings, err := iscsiSettingsFromProperties(ext.Properties)
	if err != nil {
		diags.Append(unexpectedResponseDiagnostic(newUnparseableExtensionSettingsError(err)))
		return diags
	}

	diags.Append(applyExtensionCoreSettings(ctx, data, settings)...)
	return diags
}

// toVirtualMachineExtension builds the full PUT body for every write. A PATCH is not
// used because it may merge settings instead of replacing them.
func toVirtualMachineExtension(location, extensionType string, settings any) armcompute.VirtualMachineExtension {
	return armcompute.VirtualMachineExtension{
		Location: to.Ptr(location),
		Properties: &armcompute.VirtualMachineExtensionProperties{
			Publisher:               to.Ptr(extensionPublisher),
			Type:                    to.Ptr(extensionType),
			TypeHandlerVersion:      to.Ptr(extensionTypeHandlerVersion),
			AutoUpgradeMinorVersion: to.Ptr(true),
			Settings:                settings,
		},
	}
}

// extensionTypeForVM derives the extension type from the virtual machine's OS disk type.
func extensionTypeForVM(vm armcompute.VirtualMachine) (extensionType string, diags diag.Diagnostics) {
	vmID := ""
	if vm.ID != nil {
		vmID = *vm.ID
	}

	if vm.Properties == nil || vm.Properties.StorageProfile == nil || vm.Properties.StorageProfile.OSDisk == nil ||
		vm.Properties.StorageProfile.OSDisk.OSType == nil {
		diags.Append(diag.WithPath(path.Root("virtual_machine_id"), invalidAttributeValueDiagnostic(newVirtualMachineOSUnknownError(vmID))))
		return "", diags
	}

	os := *vm.Properties.StorageProfile.OSDisk.OSType
	switch os {
	case armcompute.OperatingSystemTypesLinux:
		return extensionTypeLinux, diags
	case armcompute.OperatingSystemTypesWindows:
		return extensionTypeWindows, diags
	default:
		diags.Append(diag.WithPath(path.Root("virtual_machine_id"), invalidAttributeValueDiagnostic(newVirtualMachineOSUnsupportedError(vmID, string(os)))))
		return "", diags
	}
}

// defaultExtensionName is the extension instance name shared by the Azure Portal's
// "Mount on Virtual Machine" action for every OS, so a Portal mount of the same virtual
// machine finds this instance rather than creating a second, conflicting one.
func defaultExtensionName() string {
	return "PureMountIscsi"
}

// warnWindowsOnlyAttributesOnLinux warns about each Windows-only attribute set on a
// Linux virtual machine, where the extension ignores it.
func warnWindowsOnlyAttributesOnLinux(extensionType string, data VirtualMachineConnectionResourceModel) diag.Diagnostics {
	var diags diag.Diagnostics
	if extensionType != extensionTypeLinux {
		return diags
	}

	if !data.RebootAllowed.IsUnknown() && !data.RebootAllowed.IsNull() && data.RebootAllowed.ValueBool() {
		diags.Append(rebootAllowedNoEffectWarning())
	}
	if !data.WindowsSettings.IsUnknown() && len(data.WindowsSettings.Elements()) > 0 {
		diags.Append(windowsSettingsNoEffectWarning())
	}
	return diags
}

// extensionFullID composes the full ARM ID of an extension named extensionName on the
// virtual machine named by vmID.
func extensionFullID(vmID, extensionName string) (string, error) {
	vm, err := armids.ParseVirtualMachineID(vmID)
	if err != nil {
		return "", err
	}
	return vm.Extension(extensionName).FullID(), nil
}

// duplicateExtensionCause picks the cause for the duplicate the List-based check found
// ahead of any PUT. existing, when it has a name, gives the ID to import.
func duplicateExtensionCause(vmID string, existing *armcompute.VirtualMachineExtension, extensionType string) detailError {
	if existing != nil && existing.Name != nil {
		if existingID, err := extensionFullID(vmID, *existing.Name); err == nil {
			return newExtensionAlreadyExistsError(existingID)
		}
	}
	return newExtensionOfTypeAlreadyExistsError(vmID, extensionType, nil)
}

// instanceViewOutcome classifies an extension's instance view into the handful of
// shapes this resource treats differently.
type instanceViewOutcome int

const (
	// instanceViewMissing means the extension carried no instance view at all (a nil
	// InstanceView, or one with no statuses).
	instanceViewMissing instanceViewOutcome = iota
	instanceViewSuccess
	// instanceViewInProgress means the handler has not reached a final result: it is
	// rebooting the guest, or running (again) after the reboot.
	instanceViewInProgress
	instanceViewRebootRequired
	instanceViewFailed
	// instanceViewUnrecognized means the status code is none the handler is known to
	// report, so nothing can be said about whether iSCSI is configured.
	instanceViewUnrecognized
)

func (o instanceViewOutcome) String() string {
	switch o {
	case instanceViewMissing:
		return "missing"
	case instanceViewSuccess:
		return "success"
	case instanceViewInProgress:
		return "in_progress"
	case instanceViewRebootRequired:
		return "reboot_required"
	case instanceViewFailed:
		return "failed"
	case instanceViewUnrecognized:
		return "unrecognized"
	}
	return "unknown"
}

// classifyInstanceView reads an extension's instance view (from a GET with
// $expand=instanceView) and returns its outcome plus the status's code and raw message.
// The handler reports a single status whose code is ProvisioningState/<type>, where the
// type is succeeded, failed or transitioning; the code decides the outcome, and the
// message only splits what the code cannot: a succeeded status can mean "done" or
// "rebooting now", and reboot-required arrives as a failed status. Any other code
// (including warning) is unrecognized rather than assumed to be success, because the
// handler keeps reporting transitioning after a reboot and a failure can follow minutes
// later.
func classifyInstanceView(view *armcompute.VirtualMachineExtensionInstanceView) (outcome instanceViewOutcome, code, message string) {
	if view == nil || len(view.Statuses) == 0 || view.Statuses[0] == nil {
		return instanceViewMissing, "", ""
	}

	status := view.Statuses[0]
	if status.Message != nil {
		message = *status.Message
	}
	if status.Code != nil {
		code = *status.Code
	}
	lowerCode := strings.ToLower(code)

	switch {
	case strings.Contains(lowerCode, "failed"):
		// Reboot-required is how the handler reports "set up as far as it can without a
		// reboot": a failed status, but not a failure to act on.
		if strings.Contains(message, handlerMessageRebootRequired) {
			return instanceViewRebootRequired, code, message
		}
		return instanceViewFailed, code, message
	case strings.Contains(lowerCode, "transitioning"):
		return instanceViewInProgress, code, message
	case strings.Contains(lowerCode, "succeeded"):
		switch {
		case strings.Contains(message, handlerMessageRebooting):
			return instanceViewInProgress, code, message
		case strings.Contains(message, handlerMessageRebootRequired):
			return instanceViewRebootRequired, code, message
		}
		return instanceViewSuccess, code, message
	default:
		return instanceViewUnrecognized, code, message
	}
}

// lroFailureAction is what waitAndSettle should do once a Failed PUT's instance view
// has been fetched and classified.
type lroFailureAction int

const (
	// lroFailureUseLROError means the fetched instance view doesn't explain this
	// failure, so the LRO's own error is used instead.
	lroFailureUseLROError lroFailureAction = iota
	lroFailureUseInstanceViewMessage
	lroFailureRebootRequired
)

// classifyLROFailure picks that action from the follow-up instance-view GET. Only a
// failed status with a message replaces the LRO's own error.
func classifyLROFailure(getErr error, outcome instanceViewOutcome, message string) lroFailureAction {
	if getErr != nil {
		return lroFailureUseLROError
	}
	switch {
	case outcome == instanceViewRebootRequired:
		return lroFailureRebootRequired
	case outcome == instanceViewFailed && message != "":
		return lroFailureUseInstanceViewMessage
	default:
		return lroFailureUseLROError
	}
}

// clearingPutFailureIsHarmless reports whether Delete may proceed after its clearing
// PUT failed: only when the VM is still waiting for its first reboot, and so has no
// iSCSI sessions to log out.
func clearingPutFailureIsHarmless(getErr error, outcome instanceViewOutcome) bool {
	return getErr == nil && outcome == instanceViewRebootRequired
}
