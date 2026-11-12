// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

var (
	_ resource.Resource                = &VirtualMachineConnectionResource{}
	_ resource.ResourceWithConfigure   = &VirtualMachineConnectionResource{}
	_ resource.ResourceWithImportState = &VirtualMachineConnectionResource{}
	_ resource.ResourceWithIdentity    = &VirtualMachineConnectionResource{}
	_ resource.ResourceWithModifyPlan  = &VirtualMachineConnectionResource{}
	_ resource.ResourceWithMoveState   = &VirtualMachineConnectionResource{}
)

// Per-operation timeout defaults, overridable via the `timeouts` attribute.
const (
	defaultVirtualMachineConnectionCreateTimeout = 30 * time.Minute
	defaultVirtualMachineConnectionReadTimeout   = 5 * time.Minute
	defaultVirtualMachineConnectionUpdateTimeout = 30 * time.Minute
	defaultVirtualMachineConnectionDeleteTimeout = 15 * time.Minute
)

// instanceViewPollInterval is how long Create and Update wait between polls while the
// extension reports it is still configuring or rebooting. A var, not a const, so tests can shrink it.
var instanceViewPollInterval = 30 * time.Second

type VirtualMachineConnectionResource struct {
	// Provides Configure and the virtual machine/extension clients via promotion.
	clientsHolder
}

func NewVirtualMachineConnectionResource() resource.Resource {
	return &VirtualMachineConnectionResource{}
}

func (r *VirtualMachineConnectionResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_virtual_machine_connection"
}

func (r *VirtualMachineConnectionResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = armids.VirtualMachineExtensionIdentitySchema()
}

func (r *VirtualMachineConnectionResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Connects an Azure virtual machine to one or more Everpure Volume Groups over iSCSI. " +
			"The connection is made by the PureStorage.Extensions iSCSI mount VM extension, which this resource " +
			"installs on the virtual machine and configures with the Volume Groups' iSCSI endpoints.\n\n" +
			"Use one connection resource per virtual machine: Azure rejects a second instance of the extension " +
			"on the same virtual machine. If the virtual machine was already connected through the Azure Portal " +
			"or the API, [import](#import) its existing extension instead of creating a new one.\n\n" +
			"~> **This resource takes over the virtual machine's iSCSI connections, and is incompatible with " +
			"connections managed any other way.** Every apply connects the virtual machine to exactly " +
			"`volume_group_ids` and overrides iSCSI connections made outside this resource.\n\n" +
			"~> **Connecting a virtual machine to a Volume Group that has no volumes fails.** Referencing the " +
			"Volume Group's `id` in `volume_group_ids` only orders the connection after the Volume Group, not " +
			"after its volumes, so Terraform can create the connection while the Volume Group is still empty. " +
			"Add every volume the virtual machine should see to the connection's `depends_on`, as in the " +
			"example below: a volume created after the connection is not visible on the virtual machine until " +
			"its iSCSI sessions are rescanned by hand.\n\n" +
			"To configure iSCSI on the virtual machine yourself instead (your own initiator setup, cloud-init, " +
			"configuration management), don't use this resource: read the Volume Group's portal IPs, ports and " +
			"IQN from the [`everpure_azure_volume_group_connection_parameters` data source](https://registry." +
			"terraform.io/providers/PureStorage-OpenConnect/everpure-azure/latest/docs/data-sources/" +
			"azure_volume_group_connection_parameters).\n\n" +
			"~> **Destroying this resource logs every iSCSI session out before removing the extension**, but " +
			"leaves any OS-level configuration behind (installed packages, `multipath.conf`, udev rules).\n\n" +
			"Required Azure role assignments: `Microsoft.Compute/virtualMachines/read`, " +
			"`Microsoft.Compute/virtualMachines/extensions/read`, " +
			"`Microsoft.Compute/virtualMachines/extensions/write`, " +
			"`Microsoft.Compute/virtualMachines/extensions/delete` and " +
			"`PureStorage.Block/storagePools/volumeGroups/listConnectionParameters/action`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				CustomType:    armids.IDType{},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "Full ARM ID of the VM extension " +
					"(`Microsoft.Compute/virtualMachines/{vm}/extensions/{name}`).",
			},
			"virtual_machine_id": schema.StringAttribute{
				Required:   true,
				CustomType: armids.IDType{},
				PlanModifiers: []planmodifier.String{
					armids.PreserveEquivalentIDSpelling(),
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					armids.TypeVirtualMachine.Validator(),
				},
				MarkdownDescription: "Full ARM ID of the virtual machine (`Microsoft.Compute/virtualMachines`). " +
					"Can be in a different subscription than the one configured on the provider; if that " +
					"subscription is in another Microsoft Entra tenant, list the tenant in the provider's " +
					"`auxiliary_tenant_ids`. Changing this " +
					"value replaces the resource: the extension is removed from the old virtual machine and " +
					"created on the new one.",
			},
			"volume_group_ids": schema.SetAttribute{
				Required:    true,
				ElementType: armids.IDType{},
				PlanModifiers: []planmodifier.Set{
					armids.PreserveEquivalentIDSpellingInSet(),
				},
				Validators: []validator.Set{
					setvalidator.SizeAtLeast(1),
					setvalidator.ValueStringsAre(armids.TypeVolumeGroup.Validator()),
				},
				MarkdownDescription: "Full ARM IDs of the Volume Groups to connect this virtual machine to. Each " +
					"Volume Group's iSCSI endpoints (portal IPs and target IQNs) are resolved automatically and " +
					"sent to the extension; order does not matter. Changing this set logs out sessions for any " +
					"Volume Group removed from it and connects sessions for any Volume Group added to it, " +
					"without disturbing the rest.",
			},
			"iscsi_session_count": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(2),
				Validators: []validator.Int64{
					int64validator.Between(1, 16),
				},
				MarkdownDescription: "Number of iSCSI sessions per portal to establish for each connected " +
					"target. Defaults to 2 (the extension's own default). Applies only to Volume Groups " +
					"connected after this value changes; targets already connected keep their current " +
					"session count. Must be between 1 and 16. Can be changed in place.",
			},
			"initiator_name": schema.StringAttribute{
				Optional: true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
				MarkdownDescription: "iSCSI initiator name (IQN) to configure on the virtual machine. Left " +
					"unset, the virtual machine keeps its OS-generated initiator name, which this provider " +
					"cannot read back (it never appears in Azure Resource Manager), so this attribute reads " +
					"back as unset even when the guest has one. Setting this attribute, or changing it, changes " +
					"the virtual machine's iSCSI identity and logs out its existing sessions before " +
					"reconnecting under the new name.",
			},
			"reboot_allowed": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Whether the virtual machine may be rebooted automatically while this " +
					"extension configures it. Defaults to `false`. Can be changed in place. " +
					"Effective on Windows only:\n\n" +
					"* On Windows, first-time iSCSI setup (MPIO) cannot finish without a reboot. With this " +
					"attribute left `false` (the default), that first setup reports success with a warning " +
					"telling you to reboot the virtual machine, or set this attribute to `true`, before " +
					"configuration finishes; the resource is not marked failed. The warning repeats on every " +
					"plan or refresh until the virtual machine is rebooted and the extension reports " +
					"configuration complete.\n" +
					"* On Linux this attribute has no effect.",
			},
			"extension_name": schema.StringAttribute{
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
					stringplanmodifier.RequiresReplace(),
				},
				MarkdownDescription: "Name of the VM extension instance. Defaults to `PureMountIscsi` " +
					"on create — the same name the Azure Portal's \"Mount on Virtual Machine\" action uses, so " +
					"a later Portal-driven mount of the same virtual machine updates this instance instead of " +
					"failing. Set this to a distinct name to make a Portal mount fail instead of silently " +
					"taking over this resource's extension instance. On import, this attribute is set to the " +
					"imported extension's existing name. Changing this attribute after creation replaces the " +
					"resource: the extension is deleted (disconnecting every session) and recreated under the " +
					"new name.",
			},
			"extension_type": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "The extension type installed: `MountIscsiLinux` or `MountIscsiWindows`, " +
					"derived from the virtual machine's operating system. Changing this value (the virtual " +
					"machine's operating system changing) replaces the resource.",
			},
			"resolved_targets": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The iSCSI targets (IP, port and IQN) resolved from `volume_group_ids` and " +
					"configured on the virtual machine. Populated automatically; recomputed whenever " +
					"`volume_group_ids` or a referenced Volume Group's connection endpoints change.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"ip": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Portal IP address of the target.",
						},
						"iqn": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "IQN of the target, unique to the Volume Group it was resolved from.",
						},
						"port": schema.Int64Attribute{
							Computed:            true,
							MarkdownDescription: "Portal port of the target.",
						},
					},
				},
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeoutsBlock(ctx, "the virtual machine connection", timeoutsDefaults{
				Create: defaultVirtualMachineConnectionCreateTimeout,
				Read:   defaultVirtualMachineConnectionReadTimeout,
				Update: defaultVirtualMachineConnectionUpdateTimeout,
				Delete: defaultVirtualMachineConnectionDeleteTimeout,
			}, ""),
			"reapply_on_boot": schema.ListNestedBlock{
				MarkdownDescription: "Configures whether the extension reapplies its iSCSI configuration on " +
					"every virtual machine boot. Omitting this block is equivalent to `{ enabled = false }`.",
				Validators: []validator.List{
					listvalidator.SizeAtMost(1),
				},
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"enabled": schema.BoolAttribute{
							Required: true,
							MarkdownDescription: "Whether the extension reapplies its configuration on every " +
								"boot.",
						},
						"reboot_on_reapply_allowed": schema.BoolAttribute{
							Optional: true,
							Computed: true,
							Default:  booldefault.StaticBool(false),
							MarkdownDescription: "Whether the virtual machine may be rebooted automatically if " +
								"reapplying the configuration on boot requires it (Windows only). Defaults to " +
								"`false`.",
						},
					},
				},
			},
			"windows_settings": schema.ListNestedBlock{
				MarkdownDescription: "Advanced settings honoured on Windows virtual machines only; ignored on " +
					"Linux.",
				Validators: []validator.List{
					listvalidator.SizeAtMost(1),
				},
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"disk_timeout_in_seconds": schema.Int64Attribute{
							Optional: true,
							Validators: []validator.Int64{
								int64validator.AtLeast(1),
							},
							MarkdownDescription: "MPIO disk I/O timeout in seconds. Leaving this unset sends 0, " +
								"which keeps the extension's own default (240 seconds).",
						},
						"link_down_time_in_seconds": schema.Int64Attribute{
							Optional: true,
							Validators: []validator.Int64{
								int64validator.AtLeast(1),
							},
							MarkdownDescription: "MPIO link-down time in seconds. Leaving this unset sends 0, " +
								"which keeps the extension's own default (180 seconds).",
						},
					},
				},
			},
		},
	}
}

// ModifyPlan resolves resolved_targets and derives extension_type (plus the
// Windows-only-attribute warning) once virtual_machine_id and volume_group_ids are
// known. Both are recomputed at apply too, so an unknown value here can't pin a stale
// plan.
func (r *VirtualMachineConnectionResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy: nothing to compute.
	}

	var plan VirtualMachineConnectionResourceModel
	if resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...); resp.Diagnostics.HasError() {
		return
	}

	op := operation{action: actionPlan, kind: kindVirtualMachineConnection, in: plan.VirtualMachineID.ValueString(), unnamed: true}

	// Plan-time calls are bounded by the read timeout because they only read existing
	// objects (the virtual machine and the volume groups' connection parameters), like
	// Read does. An unknown or unset timeout yields the default with no diagnostics. A
	// value that does not parse also yields the default here; Create, Read, Update and
	// Delete report the parse error when they run.
	readTimeout, _ := plan.Timeouts.Read(ctx, defaultVirtualMachineConnectionReadTimeout)
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	if plan.VolumeGroupIDs.IsUnknown() || setHasUnknownElement(plan.VolumeGroupIDs) || r.clients == nil {
		// r.clients == nil means Configure deferred (a provider configuration value,
		// subscription_id or an authentication attribute, isn't known yet in this
		// plan): resolveVMExtensionTargets would only fail with "provider not
		// configured" below, hard-failing a plan that a later Configure call could still
		// complete. Leaving it unknown here matches the extension_type branch's handling
		// of the same situation and defers to apply, which recomputes both anyway.
		plan.ResolvedTargets = types.ListUnknown(resolvedTargetObjectType)
	} else {
		_, resolvedTargets, diags := r.resolveTargets(ctx, op, plan.VolumeGroupIDs)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		plan.ResolvedTargets = resolvedTargets
	}

	if !plan.VirtualMachineID.IsUnknown() && !plan.VirtualMachineID.IsNull() && r.clients != nil {
		vm, err := armids.ParseVirtualMachineID(plan.VirtualMachineID.ValueString())
		if err != nil {
			resp.Diagnostics.Append(diag.WithPath(path.Root("virtual_machine_id"), invalidAttributeValueDiagnostic(idErrorCause(err))))
			return
		}

		vmResult, ok := r.getVirtualMachine(ctx, op, vm, &resp.Diagnostics)
		if !ok {
			return
		}

		extensionType, diags := extensionTypeForVM(vmResult)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		plan.ExtensionType = types.StringValue(extensionType)
		resp.Diagnostics.Append(warnWindowsOnlyAttributesOnLinux(extensionType, plan)...)

		// Update PUTs the extension type carried over from prior state rather than
		// re-deriving it (see Update's own comment), so a virtual machine whose
		// operating system changed since the last plan can't be reconciled by an
		// in-place update: only a new extension can speak the new type.
		if !req.State.Raw.IsNull() {
			var priorExtensionType types.String
			resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("extension_type"), &priorExtensionType)...)
			if resp.Diagnostics.HasError() {
				return
			}
			if !priorExtensionType.IsNull() && !priorExtensionType.IsUnknown() && priorExtensionType.ValueString() != extensionType {
				resp.RequiresReplace = append(resp.RequiresReplace, path.Root("extension_type"))
			}
		}
	}

	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

// getVirtualMachine maps a 404 to an attribute error on virtual_machine_id. Shared by
// ModifyPlan and Create, so a missing virtual machine fails the plan, not just the apply.
func (r *VirtualMachineConnectionResource) getVirtualMachine(
	ctx context.Context, op operation, vm armids.VirtualMachineID, diags *diag.Diagnostics,
) (armcompute.VirtualMachine, bool) {
	vmClient, ok := r.virtualMachinesClient(vm.SubscriptionID, diags)
	if !ok {
		return armcompute.VirtualMachine{}, false
	}

	vmResult, err := vmClient.Get(ctx, vm.ResourceGroupName, vm.VirtualMachineName, nil)
	if err != nil {
		if isNotFound(err) {
			diags.Append(diag.WithPath(path.Root("virtual_machine_id"), invalidAttributeValueDiagnostic(newNotFoundError(kindVirtualMachine, vm.FullID(), nil, r.clients.apiFailure(err)))))
			return armcompute.VirtualMachine{}, false
		}
		diags.Append(op.fail(newApiStepFailedError(op, step{action: actionRetrieve, kind: kindVirtualMachine, id: vm.FullID()}, r.clients.apiFailure(err))))
		return armcompute.VirtualMachine{}, false
	}
	return vmResult.VirtualMachine, true
}

// resolveTargets resolves vgIDs into the iSCSI targets to send and their resolved_targets value.
func (r *VirtualMachineConnectionResource) resolveTargets(ctx context.Context, op operation, vgIDs types.Set) ([]iscsiTarget, types.List, diag.Diagnostics) {
	var diags diag.Diagnostics

	ids, d := setElementsAsStrings(ctx, vgIDs)
	diags.Append(d...)
	if diags.HasError() {
		return nil, types.ListNull(resolvedTargetObjectType), diags
	}

	targets, d := resolveVMExtensionTargets(ctx, &r.clientsHolder, op, ids)
	diags.Append(d...)
	if diags.HasError() {
		return nil, types.ListNull(resolvedTargetObjectType), diags
	}

	resolvedTargets, d := resolvedTargetsToList(ctx, targets)
	diags.Append(d...)
	return targets, resolvedTargets, diags
}

func (r *VirtualMachineConnectionResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data VirtualMachineConnectionResourceModel
	if resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	createTimeout, diags := data.Timeouts.Create(ctx, defaultVirtualMachineConnectionCreateTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()

	op := operation{action: actionCreate, kind: kindVirtualMachineConnection, in: data.VirtualMachineID.ValueString(), unnamed: true}
	vm, err := armids.ParseVirtualMachineID(data.VirtualMachineID.ValueString())
	if err != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("virtual_machine_id"), op.fail(idErrorCause(err))))
		return
	}

	extClient, ok := r.virtualMachineExtensionsClient(vm.SubscriptionID, &resp.Diagnostics)
	if !ok {
		return
	}

	vmResult, ok := r.getVirtualMachine(ctx, op, vm, &resp.Diagnostics)
	if !ok {
		return
	}
	if vmResult.Location == nil {
		resp.Diagnostics.Append(unexpectedResponseDiagnostic(newEmptyLocationError(kindVirtualMachine)))
		return
	}

	extensionType, diags := extensionTypeForVM(vmResult)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.ExtensionType = types.StringValue(extensionType)

	resp.Diagnostics.Append(warnWindowsOnlyAttributesOnLinux(extensionType, data)...)

	extensionName := data.ExtensionName.ValueString()
	if data.ExtensionName.IsNull() || data.ExtensionName.IsUnknown() {
		extensionName = defaultExtensionName()
	}
	data.ExtensionName = types.StringValue(extensionName)
	op.id = vm.Extension(extensionName).FullID()

	// Check every existing extension of this publisher+type under any name: a PUT
	// would silently take over one not managed by this resource instead of failing.
	existing, err := extClient.List(ctx, vm.ResourceGroupName, vm.VirtualMachineName, nil)
	if err != nil {
		resp.Diagnostics.Append(op.fail(newApiStepFailedError(op, step{action: actionList, kind: kindVirtualMachineExtensions, id: vm.FullID()}, r.clients.apiFailure(err))))
		return
	}
	if dup := findDuplicateExtension(existing.Value, extensionType); dup != nil {
		resp.Diagnostics.Append(op.fail(duplicateExtensionCause(data.VirtualMachineID.ValueString(), dup, extensionType)))
		return
	}

	targets, resolvedTargets, diags := r.resolveTargets(ctx, op, data.VolumeGroupIDs)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.ResolvedTargets = resolvedTargets

	settings, diags := buildIscsiExtensionSettings(ctx, data, targets)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	extension := toVirtualMachineExtension(*vmResult.Location, extensionType, settings)

	poller, err := extClient.BeginCreateOrUpdate(ctx, vm.ResourceGroupName, vm.VirtualMachineName, extensionName, extension, nil)
	if err != nil {
		if isDuplicateExtension(err) {
			failure := r.clients.apiFailure(err)
			resp.Diagnostics.Append(op.fail(newExtensionOfTypeAlreadyExistsError(data.VirtualMachineID.ValueString(), extensionType, &failure)))
			return
		}
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	data.ID = armids.IDValue{StringValue: types.StringValue(vm.Extension(extensionName).FullID())}

	// Save minimal state as soon as the create is accepted, so a failed poll leaves
	// a tainted resource Terraform knows about instead of an orphan.
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, data.ID.ValueString(), armids.ParseVirtualMachineExtensionID)...)
	resp.Diagnostics.Append(setPartialState(ctx, &resp.State, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ext, diags := r.waitAndSettle(ctx, op, extClient, poller, vm.ResourceGroupName, vm.VirtualMachineName, extensionName)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(applyExtensionSettings(ctx, &data, ext)...)
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, data.ID.ValueString(), armids.ParseVirtualMachineExtensionID)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *VirtualMachineConnectionResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data VirtualMachineConnectionResourceModel
	if resp.Diagnostics.Append(req.State.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	readTimeout, diags := data.Timeouts.Read(ctx, defaultVirtualMachineConnectionReadTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	op := operation{action: actionRetrieve, kind: kindVirtualMachineConnection, id: data.ID.ValueString(), unnamed: true}
	ext, err := armids.ParseVirtualMachineExtensionID(data.ID.ValueString())
	if err != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("id"), op.fail(newInvalidStoredIDError(storedInState, idErrorCause(err)))))
		return
	}

	extClient, ok := r.virtualMachineExtensionsClient(ext.SubscriptionID, &resp.Diagnostics)
	if !ok {
		return
	}

	result, err := extClient.Get(ctx, ext.ResourceGroupName, ext.VirtualMachineName, ext.ExtensionName,
		&armcompute.VirtualMachineExtensionsClientGetOptions{Expand: expandInstanceView()})
	if err != nil {
		if isNotFound(err) {
			// Covers both the extension itself and its virtual machine being gone: an
			// extension is a child resource, so a deleted VM 404s the same GET.
			resp.Diagnostics.Append(setIdentityFromIDIfNull(ctx, resp.Identity, data.ID.ValueString(), armids.ParseVirtualMachineExtensionID)...)
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	// The identity comes from the id already in state, read before
	// applyExtensionIdentity overwrites it with the API's spelling.
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, data.ID.ValueString(), armids.ParseVirtualMachineExtensionID)...)

	// A state written by a move has not yet been checked for being this resource's
	// extension, which the move could not ask the API. The check is made once, and the
	// marker is cleared with it.
	moved, d := req.Private.GetKey(ctx, movedPrivateKey)
	resp.Diagnostics.Append(d...)
	if !isOurExtension(result.Properties) {
		if len(moved) > 0 {
			moveOp := operation{action: actionMove, kind: kindVirtualMachineConnection, id: data.ID.ValueString(), unnamed: true}
			resp.Diagnostics.Append(moveOp.fail(newMovedExtensionNotMountExtensionError(data.ID.ValueString())))
			return
		}
		resp.Diagnostics.Append(op.fail(newExtensionReplacedOutOfBandError(data.ID.ValueString())))
		return
	}
	if len(moved) > 0 && resp.Private != nil {
		resp.Diagnostics.Append(resp.Private.SetKey(ctx, movedPrivateKey, nil)...)
	}

	// virtual_machine_id and volume_group_ids are preserved from state: neither can be
	// derived from the extension's own settings.
	applyExtensionIdentity(&data, result.VirtualMachineExtension)
	settings, err := iscsiSettingsFromProperties(result.Properties)
	if err != nil {
		resp.Diagnostics.Append(unexpectedResponseDiagnostic(newUnparseableExtensionSettingsError(err)))
		return
	}
	resp.Diagnostics.Append(applyExtensionCoreSettings(ctx, &data, settings)...)
	resp.Diagnostics.Append(applyExtensionAmbiguousBlocks(ctx, &data, settings)...)

	switch outcome, code, message := classifyInstanceView(instanceViewOf(result.Properties)); outcome {
	case instanceViewRebootRequired:
		resp.Diagnostics.Append(rebootRequiredWarning())
	case instanceViewFailed:
		if message == "" {
			resp.Diagnostics.Append(instanceViewFailedWithoutMessageWarning())
		} else {
			resp.Diagnostics.Append(instanceViewFailedWarning(message))
		}
	case instanceViewUnrecognized:
		resp.Diagnostics.Append(instanceViewUnrecognizedWarning(code, message))
	case instanceViewSuccess, instanceViewInProgress, instanceViewMissing:
		// Nothing to report: a refresh during configuration or without a status leaves
		// the state as read.
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *VirtualMachineConnectionResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data VirtualMachineConnectionResourceModel
	if resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}
	var priorState VirtualMachineConnectionResourceModel
	if resp.Diagnostics.Append(req.State.Get(ctx, &priorState)...); resp.Diagnostics.HasError() {
		return
	}

	// An in-place update never changes the id, so the identity is carried over
	// from state unchanged.
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, priorState.ID.ValueString(), armids.ParseVirtualMachineExtensionID)...)

	updateTimeout, diags := data.Timeouts.Update(ctx, defaultVirtualMachineConnectionUpdateTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()

	op := operation{action: actionUpdate, kind: kindVirtualMachineConnection, id: data.ID.ValueString(), unnamed: true}
	ext, err := armids.ParseVirtualMachineExtensionID(data.ID.ValueString())
	if err != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("id"), op.fail(newInvalidStoredIDError(storedInPlan, idErrorCause(err)))))
		return
	}

	extClient, ok := r.virtualMachineExtensionsClient(ext.SubscriptionID, &resp.Diagnostics)
	if !ok {
		return
	}

	// The full PUT needs the extension's location, which isn't in state; a GET supplies
	// it and confirms the extension still exists. extension_type is read from prior
	// state rather than re-derived, since it can't change without replacing
	// virtual_machine_id.
	current, err := extClient.Get(ctx, ext.ResourceGroupName, ext.VirtualMachineName, ext.ExtensionName, nil)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(op.fail(newDeletedOutsideTerraformError(kindVirtualMachineConnection, op.id)))
			return
		}
		resp.Diagnostics.Append(op.fail(newApiStepFailedError(op, step{action: actionRetrieve, kind: kindVirtualMachineExtension, id: op.id}, r.clients.apiFailure(err))))
		return
	}
	if current.Location == nil {
		resp.Diagnostics.Append(unexpectedResponseDiagnostic(newEmptyLocationError(kindVirtualMachineExtension)))
		return
	}

	extensionType := priorState.ExtensionType.ValueString()
	resp.Diagnostics.Append(warnWindowsOnlyAttributesOnLinux(extensionType, data)...)

	targets, resolvedTargets, diags := r.resolveTargets(ctx, op, data.VolumeGroupIDs)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.ResolvedTargets = resolvedTargets

	settings, diags := buildIscsiExtensionSettings(ctx, data, targets)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Skip the PUT only if the extension's live settings already match what this apply
	// would send and provisioning succeeded. An identical PUT to a succeeded extension
	// still runs it again in full, so the skip saves a re-run, not just a call.
	currentSettings, err := iscsiSettingsFromProperties(current.Properties)
	if err != nil {
		resp.Diagnostics.Append(unexpectedResponseDiagnostic(newUnparseableExtensionSettingsError(err)))
		return
	}
	currentProvisioningState := ""
	if current.Properties != nil && current.Properties.ProvisioningState != nil {
		currentProvisioningState = *current.Properties.ProvisioningState
	}
	if settingsAlreadyApplied(currentSettings, settings, currentProvisioningState) {
		resp.Diagnostics.Append(applyExtensionSettings(ctx, &data, current.VirtualMachineExtension)...)
		resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
		return
	}

	extension := toVirtualMachineExtension(*current.Location, extensionType, settings)
	extension.Properties.ForceUpdateTag = forceUpdateTagFor(currentProvisioningState)

	poller, err := extClient.BeginCreateOrUpdate(ctx, ext.ResourceGroupName, ext.VirtualMachineName, ext.ExtensionName, extension, nil)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(op.fail(newDeletedOutsideTerraformError(kindVirtualMachineConnection, op.id)))
			return
		}
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	finalExt, diags := r.waitAndSettle(ctx, op, extClient, poller, ext.ResourceGroupName, ext.VirtualMachineName, ext.ExtensionName)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(applyExtensionSettings(ctx, &data, finalExt)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *VirtualMachineConnectionResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data VirtualMachineConnectionResourceModel
	if resp.Diagnostics.Append(req.State.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	deleteTimeout, diags := data.Timeouts.Delete(ctx, defaultVirtualMachineConnectionDeleteTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()

	op := operation{action: actionDelete, kind: kindVirtualMachineConnection, id: data.ID.ValueString(), unnamed: true}
	ext, err := armids.ParseVirtualMachineExtensionID(data.ID.ValueString())
	if err != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("id"), op.fail(newInvalidStoredIDError(storedInState, idErrorCause(err)))))
		return
	}

	extClient, ok := r.virtualMachineExtensionsClient(ext.SubscriptionID, &resp.Diagnostics)
	if !ok {
		return
	}

	// Log every session out before removing the extension: sessions left up log back in
	// automatically after the next reboot. A 404 on either step means the desired end
	// state already holds. The clearing PUT needs the extension's location, which isn't
	// in state, so this GETs it first.
	current, err := extClient.Get(ctx, ext.ResourceGroupName, ext.VirtualMachineName, ext.ExtensionName, nil)
	if err != nil {
		if !isNotFound(err) {
			resp.Diagnostics.Append(op.fail(newApiStepFailedError(op, step{action: actionRetrieve, kind: kindVirtualMachineExtension, id: op.id}, r.clients.apiFailure(err))))
		}
		return
	}
	if current.Location == nil {
		resp.Diagnostics.Append(unexpectedResponseDiagnostic(newEmptyLocationError(kindVirtualMachineExtension)))
		return
	}

	clearing := toVirtualMachineExtension(*current.Location, data.ExtensionType.ValueString(), destroyClearingIscsiSettings())
	clearPoller, err := extClient.BeginCreateOrUpdate(ctx, ext.ResourceGroupName, ext.VirtualMachineName, ext.ExtensionName, clearing, nil)
	if err != nil {
		if !isNotFound(err) {
			resp.Diagnostics.Append(op.fail(newIscsiClearingFailedError(r.clients.apiFailure(err))))
		}
		return
	}
	if _, clearErr := clearPoller.PollUntilDone(ctx, nil); clearErr != nil {
		if isNotFound(clearErr) {
			return
		}

		// A failed clearing PUT isn't necessarily fatal: reboot-required is a known
		// outcome, so this checks the instance view before giving up.
		result, getErr := extClient.Get(ctx, ext.ResourceGroupName, ext.VirtualMachineName, ext.ExtensionName,
			&armcompute.VirtualMachineExtensionsClientGetOptions{Expand: expandInstanceView()})
		outcome := instanceViewMissing
		if getErr == nil {
			outcome, _, _ = classifyInstanceView(instanceViewOf(result.Properties))
		}
		if !clearingPutFailureIsHarmless(getErr, outcome) {
			resp.Diagnostics.Append(op.fail(newIscsiClearingFailedError(r.clients.apiFailure(clearErr))))
			return
		}
	}

	delPoller, err := extClient.BeginDelete(ctx, ext.ResourceGroupName, ext.VirtualMachineName, ext.ExtensionName, nil)
	if err != nil {
		if !isNotFound(err) {
			resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		}
		return
	}
	if _, err := delPoller.PollUntilDone(ctx, nil); err != nil {
		if !isNotFound(err) {
			resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		}
	}
}

func (r *VirtualMachineConnectionResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if r.clients == nil {
		resp.Diagnostics.Append(operationConfigureProvider.fail(newProviderNotConfiguredError()))
		return
	}

	importID, diags := resolveImportID(ctx, req, kindVirtualMachineConnection, armids.VirtualMachineExtensionID.FullID, armids.ParseVirtualMachineExtensionID)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	importOp := operation{action: actionImport, kind: kindVirtualMachineConnection, id: importID, unnamed: true}

	parsed, err := armids.ParseVirtualMachineExtensionID(importID)
	if err != nil {
		resp.Diagnostics.Append(importOp.fail(idErrorCause(err)))
		return
	}
	// The parsed subscription isn't checked against the provider's own: the virtual
	// machine may legitimately live in a different subscription.

	resp.Diagnostics.Append(resp.Identity.Set(ctx, parsed)...)
	resp.Diagnostics.Append(seedVirtualMachineConnectionState(ctx, &resp.State, importID, parsed)...)
}

// seedVirtualMachineConnectionState writes the attributes known from the extension's ID
// alone, for ImportState and for a move.
func seedVirtualMachineConnectionState(ctx context.Context, state *tfsdk.State, id string, parsed armids.VirtualMachineExtensionID) diag.Diagnostics {
	var diags diag.Diagnostics
	diags.Append(state.SetAttribute(ctx, path.Root("id"), id)...)
	diags.Append(state.SetAttribute(ctx, path.Root("virtual_machine_id"), parsed.VirtualMachine().FullID())...)
	// volume_group_ids is left unset: it can't be reconstructed from the extension's
	// settings, so the first plan after an import or a move shows it being set. Every
	// other attribute is filled in by the Read that runs right after.
	diags.Append(state.SetAttribute(ctx, path.Root("extension_name"), parsed.ExtensionName)...)
	return diags
}

// MoveState takes over an azapi_resource of type Microsoft.Compute/virtualMachines/extensions
// or an azurerm_virtual_machine_extension. Neither says which extension it installs in a way
// the move can rely on without reading the rest of its state, and the move cannot ask the
// API, so it marks the state and Read checks that the extension is this resource's.
func (r *VirtualMachineConnectionResource) MoveState(context.Context) []resource.StateMover {
	return resourceMove[armids.VirtualMachineExtensionID]{
		kind:    kindVirtualMachineConnection,
		sources: []moveSource{azapiResourceSource, azurermVirtualMachineExtensionSource},
		parse:   armids.ParseVirtualMachineExtensionID, seed: seedVirtualMachineConnectionState, markMoved: true,
	}.stateMovers()
}

// waitAndSettle waits for a Create or Update PUT, polls the instance view until setup
// finishes, and returns the extension from the last instance-view GET. A
// reboot-required failure becomes a warning.
func (r *VirtualMachineConnectionResource) waitAndSettle(
	ctx context.Context, op operation, extClient *armcompute.VirtualMachineExtensionsClient,
	poller *runtime.Poller[armcompute.VirtualMachineExtensionsClientCreateOrUpdateResponse],
	resourceGroupName, vmName, extensionName string,
) (armcompute.VirtualMachineExtension, diag.Diagnostics) {
	var diags diag.Diagnostics

	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		// The LRO's own error is a generic wrapper; the instance view distinguishes
		// reboot-required (success + warning) from a genuine failure. Falls back to
		// the LRO's error only if that GET itself fails.
		result, getErr := extClient.Get(ctx, resourceGroupName, vmName, extensionName,
			&armcompute.VirtualMachineExtensionsClientGetOptions{Expand: expandInstanceView()})
		outcome, message := instanceViewMissing, ""
		if getErr == nil {
			outcome, _, message = classifyInstanceView(instanceViewOf(result.Properties))
		}
		switch classifyLROFailure(getErr, outcome, message) {
		case lroFailureRebootRequired:
			diags.Append(rebootRequiredWarning())
			return result.VirtualMachineExtension, diags
		case lroFailureUseInstanceViewMessage:
			diags.Append(op.fail(newIscsiConfigurationFailedError(message)))
			return armcompute.VirtualMachineExtension{}, diags
		default: // lroFailureUseLROError
			diags.Append(r.clients.apiCallError(op, err))
			return armcompute.VirtualMachineExtension{}, diags
		}
	}

	ext, outcome, code, message, d := r.waitForConfigurationOutcome(ctx, op, extClient, resourceGroupName, vmName, extensionName)
	diags.Append(d...)
	if diags.HasError() {
		return armcompute.VirtualMachineExtension{}, diags
	}
	switch outcome {
	case instanceViewFailed:
		diags.Append(op.fail(newIscsiConfigurationFailedError(message)))
		return armcompute.VirtualMachineExtension{}, diags
	case instanceViewMissing:
		diags.Append(op.fail(newIscsiConfigurationStatusMissingError()))
		return armcompute.VirtualMachineExtension{}, diags
	case instanceViewRebootRequired:
		diags.Append(rebootRequiredWarning())
	case instanceViewUnrecognized:
		diags.Append(op.fail(newIscsiConfigurationStatusUnrecognizedError(code, message)))
		return armcompute.VirtualMachineExtension{}, diags
	case instanceViewSuccess, instanceViewInProgress:
		// instanceViewInProgress can't actually reach here: waitForConfigurationOutcome
		// only returns once the handler stops configuring or rebooting. Listed for
		// exhaustiveness.
	}

	return ext, diags
}

// waitForConfigurationOutcome polls the extension's instance view until it stops
// reporting that the handler is still configuring or rebooting, or ctx's deadline is
// reached, whichever comes first, and returns the extension from the last GET alongside
// its outcome, status code and message.
func (r *VirtualMachineConnectionResource) waitForConfigurationOutcome(
	ctx context.Context, op operation, client *armcompute.VirtualMachineExtensionsClient, resourceGroupName, vmName, extensionName string,
) (armcompute.VirtualMachineExtension, instanceViewOutcome, string, string, diag.Diagnostics) {
	var diags diag.Diagnostics

	for {
		result, err := client.Get(ctx, resourceGroupName, vmName, extensionName,
			&armcompute.VirtualMachineExtensionsClientGetOptions{Expand: expandInstanceView()})
		if err != nil {
			diags.Append(op.fail(newInstanceViewReadFailedError(r.clients.apiFailure(err))))
			return armcompute.VirtualMachineExtension{}, instanceViewMissing, "", "", diags
		}

		outcome, code, message := classifyInstanceView(instanceViewOf(result.Properties))
		tflog.Debug(ctx, "Polled the extension's instance view", map[string]any{
			"status_code": code,
			"message":     message,
			"outcome":     outcome.String(),
		})
		if outcome != instanceViewInProgress {
			return result.VirtualMachineExtension, outcome, code, message, diags
		}

		select {
		case <-ctx.Done():
			diags.Append(op.fail(newIscsiConfigurationTimedOutError(message)))
			return armcompute.VirtualMachineExtension{}, instanceViewInProgress, code, message, diags
		case <-time.After(instanceViewPollInterval):
		}
	}
}

// expandInstanceView is the $expand value for GETs that need the extension's instance
// view; the PUT/PATCH response never carries one.
func expandInstanceView() *string {
	expand := "instanceView"
	return &expand
}

// instanceViewOf returns props' instance view, or nil if props itself is nil.
func instanceViewOf(props *armcompute.VirtualMachineExtensionProperties) *armcompute.VirtualMachineExtensionInstanceView {
	if props == nil {
		return nil
	}
	return props.InstanceView
}

// isOurExtension reports whether props names this resource's publisher and one of its
// two extension types, so Read can detect an out-of-band replacement.
func isOurExtension(props *armcompute.VirtualMachineExtensionProperties) bool {
	if props == nil || props.Publisher == nil || props.Type == nil {
		return false
	}
	return isMountExtension(*props.Publisher, *props.Type)
}

// findDuplicateExtension returns the first extension in existing whose publisher and
// type match extensionType — regardless of its own name — or nil if none does.
func findDuplicateExtension(existing []*armcompute.VirtualMachineExtension, extensionType string) *armcompute.VirtualMachineExtension {
	for _, ext := range existing {
		if ext == nil || ext.Properties == nil || ext.Properties.Publisher == nil || ext.Properties.Type == nil {
			continue
		}
		if *ext.Properties.Publisher == extensionPublisher && *ext.Properties.Type == extensionType {
			return ext
		}
	}
	return nil
}

// setElementsAsStrings decodes a set of armids.IDValue elements into plain strings. A
// null or unknown set comes back as no elements.
func setElementsAsStrings(ctx context.Context, s types.Set) ([]string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if s.IsNull() || s.IsUnknown() {
		return nil, diags
	}
	var ids []string
	diags.Append(s.ElementsAs(ctx, &ids, false)...)
	return ids, diags
}

// setHasUnknownElement reports whether any element of s is itself unknown (e.g. a
// reference to a resource not yet created in the same plan). A set with a mix of known
// and unknown elements is still "known" as a whole under Terraform's type system.
func setHasUnknownElement(s types.Set) bool {
	for _, e := range s.Elements() {
		if e.IsUnknown() {
			return true
		}
	}
	return false
}
