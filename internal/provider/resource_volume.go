// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"strconv"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

var (
	_ resource.Resource                   = &VolumeResource{}
	_ resource.ResourceWithConfigure      = &VolumeResource{}
	_ resource.ResourceWithImportState    = &VolumeResource{}
	_ resource.ResourceWithMoveState      = &VolumeResource{}
	_ resource.ResourceWithIdentity       = &VolumeResource{}
	_ resource.ResourceWithModifyPlan     = &VolumeResource{}
	_ resource.ResourceWithValidateConfig = &VolumeResource{}
)

// Per-operation defaults, overridable per resource instance via the `timeouts`
// attribute. Create and update add margin over the backend's own 15-minute cap on a
// single workflow; delete covers up to two sequential workflows (soft delete, then
// eradication when eradicate_on_destroy is set) — the same reasoning as the volume
// group's own defaults.
const (
	defaultVolumeCreateTimeout = 20 * time.Minute
	defaultVolumeReadTimeout   = 5 * time.Minute
	defaultVolumeUpdateTimeout = 20 * time.Minute
	defaultVolumeDeleteTimeout = 35 * time.Minute
)

type VolumeResource struct {
	// clientsHolder supplies Configure and volumesClient via promotion; see its own
	// doc comment.
	clientsHolder
}

func NewVolumeResource() resource.Resource {
	return &VolumeResource{}
}

func (r *VolumeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_volume"
}

// multipleOf512Validator rejects an int64 value that isn't an exact multiple of 512 —
// the backend's own granularity for a volume's provisioned size ("field provisionedSize
// must be multiple of 512").
type multipleOf512Validator struct{}

func (v multipleOf512Validator) Description(_ context.Context) string {
	return "value must be a multiple of 512"
}

func (v multipleOf512Validator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v multipleOf512Validator) ValidateInt64(_ context.Context, req validator.Int64Request, resp *validator.Int64Response) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if req.ConfigValue.ValueInt64()%512 != 0 {
		resp.Diagnostics.Append(attributeValidatorError(req.Path, notMultipleOf512Rule, strconv.FormatInt(req.ConfigValue.ValueInt64(), 10)))
	}
}

func multipleOf512() validator.Int64 {
	return multipleOf512Validator{}
}

func (r *VolumeResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = armids.VolumeIdentitySchema()
}

func (r *VolumeResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	immutable := []planmodifier.String{requiresReplaceProtected(kindVolume)}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Volume on a Volume Group.\n\n" +
			"~> **Destroying the parent volume group soft-deletes this volume with it.** The " +
			"backend destroys every volume in a volume group as part of destroying the group " +
			"itself; all of it stays recoverable until eradication. See " +
			"[Deletion policies](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/guides/deletion-policies).\n\n" +
			"To read a volume managed elsewhere without taking ownership of it, use the " +
			"[`everpure_azure_volume` data source](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/data-sources/azure_volume).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				CustomType:    armids.IDType{},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "The full Azure Resource Manager ID of the volume, e.g. " +
					"`/subscriptions/.../resourceGroups/.../providers/PureStorage.Block/storagePools/pool1/" +
					"volumeGroups/vg1/volumes/vol1`.",
			},
			"name": schema.StringAttribute{
				Required:            true,
				PlanModifiers:       immutable,
				Validators:          []validator.String{storageObjectNameValidator()},
				MarkdownDescription: "Name of the volume. The name " + storageObjectNameRule + ". Changing this forces a new volume to be created.",
			},
			"volume_group_id": schema.StringAttribute{
				Required:   true,
				CustomType: armids.IDType{},
				PlanModifiers: append([]planmodifier.String{armids.PreserveEquivalentIDSpelling()},
					immutable...),
				Validators: []validator.String{
					armids.TypeVolumeGroup.Validator(),
				},
				MarkdownDescription: "Full Azure Resource Manager ID of the volume group this volume lives on. " +
					"Changing this forces a new volume to be created.",
			},
			"provisioned_size_in_bytes": schema.Int64Attribute{
				Optional:      true,
				Computed:      true,
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
				Validators: []validator.Int64{
					int64validator.AtLeast(1048576),
					multipleOf512(),
					int64validator.ConflictsWith(path.MatchRoot("provisioned_size_in_gib")),
				},
				MarkdownDescription: "Provisioned size of the volume, in bytes. Must be at least 1048576 " +
					"(1 MiB) and a multiple of 512. The canonical size attribute: it is always populated, " +
					"including when the volume's size comes from `provisioned_size_in_gib` or from a " +
					"source that isn't a whole number of gibibytes. Conflicts with " +
					"`provisioned_size_in_gib`. Can be changed in place, but to a larger value only.",
			},
			"provisioned_size_in_gib": schema.Int64Attribute{
				Optional: true,
				Validators: []validator.Int64{
					int64validator.AtLeast(1),
					int64validator.ConflictsWith(path.MatchRoot("provisioned_size_in_bytes")),
				},
				MarkdownDescription: "Provisioned size of the volume, in gibibytes (GiB, 1024^3 bytes). " +
					"A shorthand for `provisioned_size_in_bytes` that saves writing the size out in " +
					"bytes, and the preferred way to set the size when it is a whole number of " +
					"gibibytes. Conflicts with `provisioned_size_in_bytes`. Can be changed in place, to a larger " +
					"value only.",
			},
			"source_type": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Default:       stringdefault.StaticString(volumeSourceTypeNone),
				PlanModifiers: immutable,
				Validators: []validator.String{
					stringvalidator.OneOf(volumeSourceTypeValues()...),
				},
				MarkdownDescription: "How the volume's initial contents are created. One of `none` (empty, " +
					"the default, sized from `provisioned_size_in_bytes`/`provisioned_size_in_gib`), " +
					"`volume` (clone from `source_volume_id`), `serialNumber` (clone from " +
					"`source_serial_number`), `snapshot` (restore from `source_snapshot`), or " +
					"`recoverableVolume` (recover from `source_recoverable_volume_id`). An imported " +
					"volume always shows `none` here, since the API cannot report a volume's origin " +
					"after the fact. Changing this forces a new volume to be created.",
			},
			"source_volume_id": schema.StringAttribute{
				Optional:   true,
				CustomType: armids.IDType{},
				PlanModifiers: append([]planmodifier.String{armids.PreserveEquivalentIDSpelling()},
					immutable...),
				Validators: []validator.String{
					armids.TypeVolume.Validator(),
				},
				MarkdownDescription: "Full Azure Resource Manager ID of the volume to clone from. Required " +
					"when `source_type` is `volume`, and must be unset otherwise. Must be in the same " +
					"storage pool as `volume_group_id`. Changing this forces a new volume to be created.",
			},
			"source_serial_number": schema.StringAttribute{
				Optional:      true,
				PlanModifiers: immutable,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
				MarkdownDescription: "Serial number of the volume to clone from, for cloning a volume that " +
					"was created through Platform Console rather than through Azure. The source " +
					"volume is left unchanged. Required when `source_type` is `serialNumber`, and must " +
					"be unset otherwise. The source must be in the same storage pool as " +
					"`volume_group_id`, and cannot be a volume managed through Azure; use " +
					"`source_volume_id` to clone those. Changing this forces a new volume to be created.",
			},
			"source_recoverable_volume_id": schema.StringAttribute{
				Optional:   true,
				CustomType: armids.IDType{},
				PlanModifiers: append([]planmodifier.String{armids.PreserveEquivalentIDSpelling()},
					immutable...),
				Validators: []validator.String{
					armids.TypeVolume.Validator(),
				},
				MarkdownDescription: "Full Azure Resource Manager ID of the soft-deleted volume to recover " +
					"— a soft-deleted volume keeps its own, renamed volume resource path, so this has the " +
					"same shape as `source_volume_id`. Required when `source_type` is `recoverableVolume`, " +
					"and must be unset otherwise. Must be in the same volume group as `volume_group_id`. " +
					"Changing this forces a new volume to be created.",
			},
			"serial_number": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "The volume's own serial number. Visible in the VM as the disk/drive " +
					"serial number. Unique for each volume, so can be used to distinguish volumes with " +
					"identical sizes. It cannot be used as another volume's `source_serial_number`: cloning " +
					"by serial number applies only to volumes created directly on the storage pool, for " +
					"those created through Azure use `source_volume_id`.",
			},
			"deletion_protection": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether to block destroying this volume, or replacing it (which " +
					"destroys it as part of the replacement). Defaults to `true`. Set to `false`, and " +
					"apply, before a destroy or a change that would otherwise force replacement.",
			},
			"eradicate_on_destroy": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Whether destroy permanently eradicates the volume instead of " +
					"soft-deleting it. Defaults to `false`. There is no recovery window once eradicated.",
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeoutsBlock(ctx, "the volume", timeoutsDefaults{
				Create: defaultVolumeCreateTimeout,
				Read:   defaultVolumeReadTimeout,
				Update: defaultVolumeUpdateTimeout,
				Delete: defaultVolumeDeleteTimeout,
			}, ", including eradicating its recoverable copy when eradicate_on_destroy is set"),
			"source_snapshot": schema.ListNestedBlock{
				MarkdownDescription: "The volume group snapshot and member volume to restore from. " +
					"Required when `source_type` is `snapshot`, and must be absent otherwise. Create-only: " +
					"changing, adding or removing this block forces a new volume to be created.",
				Validators: []validator.List{
					listvalidator.SizeAtMost(1),
				},
				PlanModifiers: []planmodifier.List{
					requiresReplaceProtectedList(kindVolume),
				},
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"id": schema.StringAttribute{
							Required:   true,
							CustomType: armids.IDType{},
							PlanModifiers: []planmodifier.String{
								armids.PreserveEquivalentIDSpelling(),
							},
							Validators: []validator.String{
								armids.TypeVolumeGroupSnapshot.Validator(),
							},
							MarkdownDescription: "Full Azure Resource Manager ID of the volume group " +
								"snapshot containing the volume snapshot to restore. Must be in the same " +
								"storage pool as `volume_group_id`.",
						},
						"volume_name": schema.StringAttribute{
							Required: true,
							Validators: []validator.String{
								stringvalidator.LengthAtLeast(1),
							},
							MarkdownDescription: "Name of the member volume, within the volume group " +
								"snapshot named by `id`, to restore — i.e. the source volume's own name " +
								"at the time the snapshot was taken.",
						},
					},
				},
			},
		},
	}
}

// ValidateConfig enforces the source_type / source input matrix: exactly the one
// source input source_type names must be set (the source_snapshot block counts as one
// input, its own two fields enforced Required by the schema once the block is
// present), and every other row's input must not be.
func (r *VolumeResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var data VolumeResourceModel
	if resp.Diagnostics.Append(req.Config.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	if data.SourceType.IsUnknown() {
		return
	}
	sourceType := volumeSourceTypeOrDefault(data.SourceType)

	if sourceType != volumeSourceTypeNone {
		// Only a null value fails the required check here: an unknown one (e.g. a
		// reference to another resource's id, not yet known at plan time) still
		// satisfies it, since Create's own recheck runs again once the value has
		// resolved.
		if wantPath, missing := missingVolumeSourceAttribute(sourceType, data); missing {
			resp.Diagnostics.Append(diag.WithPath(wantPath, invalidAttributeCombinationDiagnostic(newSourceAttributeRequiredError(wantPath, sourceType))))
		}
		if _, ok := volumeSourceRowFor(sourceType); !ok {
			// An invalid source_type is already reported by the OneOf validator.
			return
		}
	}

	// Every row not named by sourceType must be unset. This also covers
	// source_type = "none", since "none" names no row at all.
	for _, row := range volumeSourceRows {
		if row.sourceType == sourceType || !isDefinitelySet(row.get(data)) {
			continue
		}
		reason := newSourceAttributeNotAllowedError(row.attribute, sourceType, sourceType == volumeSourceTypeNone)
		resp.Diagnostics.Append(diag.WithPath(row.attribute, invalidAttributeCombinationDiagnostic(reason)))
	}
}

// ModifyPlan derives provisioned_size_in_bytes from provisioned_size_in_gib, enforces
// the create-only size-presence rule and the two source-ID guards, rejects a size
// decrease, and blocks a destroy plan while the prior state's deletion_protection is
// true. A plan that would force this volume to be replaced is blocked separately, by
// each immutable attribute's (and the source_snapshot block's) own
// requiresReplaceProtected/requiresReplaceProtectedList plan modifier — see
// requires_replace_protected.go — since each of those already knows, on its own,
// whether its own change is what triggered the replacement. Delete carries a matching
// deletion_protection check as defence in depth, in case a future change bypasses
// either guard.
func (r *VolumeResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		if req.State.Raw.IsNull() {
			return // a plan to destroy something that never existed: nothing to do.
		}
		var state VolumeResourceModel
		if resp.Diagnostics.Append(req.State.Get(ctx, &state)...); resp.Diagnostics.HasError() {
			return
		}
		if !state.DeletionProtection.ValueBool() {
			return
		}
		resp.Diagnostics.Append(deletionProtectedDiagnostic(kindVolume, newDeletionProtectedError(kindVolume, state.Name.ValueString())))
		return
	}

	var plan VolumeResourceModel
	if resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...); resp.Diagnostics.HasError() {
		return
	}

	// Fold provisioned_size_in_gib into provisioned_size_in_bytes so the plan displays
	// the byte value that will actually reach the API, whichever attribute the config
	// used. resolveVolumeSizeInBytes owns the rules, including the one that matters
	// most here: a null gib must leave the carried-forward byte value alone rather
	// than recomputing it as zero. Create and Update call the same function, so an
	// unresolved gib that leaves the size unknown in the plan becomes concrete at
	// apply instead of silently resolving to the old size.
	plan.ProvisionedSizeInBytes = resolveVolumeSizeInBytes(plan)

	isCreate := req.State.Raw.IsNull()
	if isCreate {
		resp.Diagnostics.Append(checkVolumeSizePresence(plan)...)
	}

	sourceType := volumeSourceTypeOrDefault(plan.SourceType)
	resp.Diagnostics.Append(checkVolumeSourceRules(ctx, sourceType, plan)...)

	if !isCreate && !plan.ProvisionedSizeInBytes.IsUnknown() && !plan.ProvisionedSizeInBytes.IsNull() {
		var state VolumeResourceModel
		if resp.Diagnostics.Append(req.State.Get(ctx, &state)...); resp.Diagnostics.HasError() {
			return
		}
		if !state.ProvisionedSizeInBytes.IsNull() &&
			plan.ProvisionedSizeInBytes.ValueInt64() < state.ProvisionedSizeInBytes.ValueInt64() {
			// The attribute the user wrote: the bytes one is only the plan's derived
			// value when the size came from provisioned_size_in_gib.
			sizePath := path.Root("provisioned_size_in_bytes")
			if isDefinitelySet(plan.ProvisionedSizeInGiB) {
				sizePath = path.Root("provisioned_size_in_gib")
			}
			resp.Diagnostics.Append(diag.WithPath(sizePath, invalidAttributeValueDiagnostic(
				newVolumeSizeDecreaseError(plan.ProvisionedSizeInBytes.ValueInt64(), state.ProvisionedSizeInBytes.ValueInt64()))))
		}
	}

	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

func (r *VolumeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	client, ok := r.volumesClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data VolumeResourceModel
	if resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	createTimeout, diags := data.Timeouts.Create(ctx, defaultVolumeCreateTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()

	op := operation{action: actionCreate, kind: kindVolume, name: data.Name.ValueString()}
	vg, idErr := parseIDInSubscription(armids.ParseVolumeGroupID, data.VolumeGroupID.ValueString(), kindVolumeGroup, r.clients.subscriptionID)
	if idErr != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(*idErr)))
		return
	}
	op.id = vg.Volume(op.name).FullID()

	sourceType := volumeSourceTypeOrDefault(data.SourceType)

	// Every value is concrete by now, so re-resolve the size: a provisioned_size_in_gib
	// that was still unknown when ModifyPlan ran has a real value here.
	data.ProvisionedSizeInBytes = resolveVolumeSizeInBytes(data)

	// ValidateConfig and ModifyPlan let an unknown source id or size (a reference to
	// another resource's value, not yet known at plan time) through; by now the plan
	// has resolved every value to something concrete, so recheck both rules against
	// the resolved data.
	if resp.Diagnostics.Append(checkVolumeSizePresence(data)...); resp.Diagnostics.HasError() {
		return
	}
	if resp.Diagnostics.Append(checkVolumeSourceRules(ctx, sourceType, data)...); resp.Diagnostics.HasError() {
		return
	}

	vol, diags := toVolume(ctx, data)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	existing, err := client.Get(ctx, vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName, data.Name.ValueString(), nil)
	var existingState string
	if err == nil && existing.Properties != nil {
		existingState = provisioningStateName(existing.Properties.ProvisioningState)
	}
	if d := r.clients.existingObjectDiagnostic(op, "everpure_azure_volume", existing.ID, existingState, err); d != nil {
		resp.Diagnostics.Append(d)
		return
	}

	poller, err := client.BeginCreate(ctx, vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName, data.Name.ValueString(), vol, nil)
	if err != nil {
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	// Save minimal state as soon as the create is accepted, so a failed poll leaves
	// a tainted resource Terraform knows about instead of an orphan.
	// The id is set from the requested components so the tainted state carries an
	// id and an identity that agree; the final pair below comes from the API's id.
	data.ID = armids.IDValue{StringValue: types.StringValue(vg.Volume(data.Name.ValueString()).FullID())}
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, data.ID.ValueString(), armids.ParseVolumeID)...)
	// deletion_protection guards an object the configuration has relied on. A create that
	// fails after the PUT leaves a tainted object that Terraform replaces on the next
	// apply, and the planned true in that state would make the replacement's Delete refuse
	// with advice (set it to false and apply) that cannot be followed on a tainted
	// resource. The final state below keeps the planned value.
	partial := data
	partial.DeletionProtection = types.BoolValue(false)
	resp.Diagnostics.Append(setPartialState(ctx, &resp.State, &partial)...)
	if resp.Diagnostics.HasError() {
		return
	}

	result, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	resp.Diagnostics.Append(applyVolume(&data, result.Volume)...)
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, data.ID.ValueString(), armids.ParseVolumeID)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *VolumeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	client, ok := r.volumesClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data VolumeResourceModel
	if resp.Diagnostics.Append(req.State.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	readTimeout, diags := data.Timeouts.Read(ctx, defaultVolumeReadTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	op := operation{action: actionRetrieve, kind: kindVolume, name: data.Name.ValueString()}
	vg, err := armids.ParseVolumeGroupID(data.VolumeGroupID.ValueString())
	if err != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(newInvalidStoredIDError(storedInState, idErrorCause(err)))))
		return
	}
	op.id = vg.Volume(op.name).FullID()
	if c := subscriptionMismatchCause(vg, kindVolumeGroup, r.clients.subscriptionID); c != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(*c)))
		return
	}

	result, err := client.Get(ctx, vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName, data.Name.ValueString(), nil)
	if err != nil {
		if isNotFound(err) {
			// Covers both "deleted outside Terraform" and a soft delete: either way,
			// the backend renames the object on delete, so GET by the original name
			// 404s immediately, before eradication.
			resp.Diagnostics.Append(setIdentityFromIDIfNull(ctx, resp.Identity, idOrFallback(data.ID, vg.Volume(data.Name.ValueString()).FullID()), armids.ParseVolumeID)...)
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	// A GET by name can land on a destroyed twin, which it returns as a normal 200: a
	// volume imported by its timestamped name, or one soft-deleted outside Terraform
	// under a state that recorded that name. It is gone as far as this resource is
	// concerned, so on import this ends in "Cannot import non-existent remote object"
	// rather than a state holding a volume that is not live.
	if isVolumeSoftDeleted(result.Volume) {
		resp.Diagnostics.Append(setIdentityFromIDIfNull(ctx, resp.Identity, idOrFallback(data.ID, vg.Volume(data.Name.ValueString()).FullID()), armids.ParseVolumeID)...)
		resp.State.RemoveResource(ctx)
		return
	}

	// The identity comes from the id already in state, read before the apply
	// below overwrites it with the API's spelling. The id is null in a tainted state
	// from a failed create; the API's id then becomes the state id, so the identity follows it.
	identityID := data.ID.ValueString()

	// applyVolume never touches source_type, any source input (including the
	// block), deletion_protection, eradicate_on_destroy, provisioned_size_in_gib or
	// volume_group_id, so starting from state (above) and mapping only what it does
	// touch is what preserves all of those exactly as configured.
	resp.Diagnostics.Append(applyVolume(&data, result.Volume)...)
	if identityID == "" {
		identityID = data.ID.ValueString()
	}
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, identityID, armids.ParseVolumeID)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *VolumeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data VolumeResourceModel
	if resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	var priorState VolumeResourceModel
	if resp.Diagnostics.Append(req.State.Get(ctx, &priorState)...); resp.Diagnostics.HasError() {
		return
	}

	// An in-place update never changes the id, so the identity is carried over
	// from state unchanged.
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, priorState.ID.ValueString(), armids.ParseVolumeID)...)

	// Re-resolve before comparing, for the same reason Create does: a
	// provisioned_size_in_gib that was unknown at plan time is known now, and
	// comparing the stale planned value would skip the resize it asks for.
	data.ProvisionedSizeInBytes = resolveVolumeSizeInBytes(data)

	// Comparing the resolved byte value alone covers both units — switching an
	// equivalent gib config to the same byte count sends nothing. This comparison and
	// toVolumeUpdate's PATCH body cover the same attribute, deliberately kept next to
	// each other so neither can grow to cover something the other doesn't.
	if data.ProvisionedSizeInBytes.Equal(priorState.ProvisionedSizeInBytes) {
		resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
		return
	}

	client, ok := r.volumesClient(&resp.Diagnostics)
	if !ok {
		return
	}

	updateTimeout, diags := data.Timeouts.Update(ctx, defaultVolumeUpdateTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()

	op := operation{action: actionUpdate, kind: kindVolume, name: data.Name.ValueString()}
	vg, err := armids.ParseVolumeGroupID(data.VolumeGroupID.ValueString())
	if err != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(newInvalidStoredIDError(storedInPlan, idErrorCause(err)))))
		return
	}
	op.id = vg.Volume(op.name).FullID()
	if c := subscriptionMismatchCause(vg, kindVolumeGroup, r.clients.subscriptionID); c != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(*c)))
		return
	}

	update := toVolumeUpdate(data)

	poller, err := client.BeginUpdate(ctx, vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName, data.Name.ValueString(), update, nil)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(op.fail(newDeletedOutsideTerraformError(kindVolume, op.id)))
			return
		}
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	result, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(op.fail(newDeletedOutsideTerraformError(kindVolume, op.id)))
			return
		}
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	resp.Diagnostics.Append(applyVolume(&data, result.Volume)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *VolumeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data VolumeResourceModel
	if resp.Diagnostics.Append(req.State.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	if data.DeletionProtection.ValueBool() {
		resp.Diagnostics.Append(deletionProtectedDiagnostic(kindVolume, newDeletionProtectedError(kindVolume, data.Name.ValueString())))
		return
	}

	client, ok := r.volumesClient(&resp.Diagnostics)
	if !ok {
		return
	}

	deleteTimeout, diags := data.Timeouts.Delete(ctx, defaultVolumeDeleteTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()

	op := operation{action: actionDelete, kind: kindVolume, name: data.Name.ValueString()}
	vg, err := armids.ParseVolumeGroupID(data.VolumeGroupID.ValueString())
	if err != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(newInvalidStoredIDError(storedInState, idErrorCause(err)))))
		return
	}
	op.id = vg.Volume(op.name).FullID()
	if c := subscriptionMismatchCause(vg, kindVolumeGroup, r.clients.subscriptionID); c != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(*c)))
		return
	}
	eradicateOp := op
	eradicateOp.action = actionEradicate
	listVolumesStep := step{action: actionList, kind: kindVolumes, id: vg.FullID()}

	var before []*armpurestorageblock.Volume
	if data.EradicateOnDestroy.ValueBool() {
		before, err = listVolumes(ctx, client, vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName)
		if err != nil {
			if isNotFound(err) {
				return
			}
			resp.Diagnostics.Append(eradicateOp.fail(newApiStepFailedError(eradicateOp, listVolumesStep, r.clients.apiFailure(err))))
			return
		}
	}

	poller, err := client.BeginDelete(ctx, vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName, data.Name.ValueString(), nil)
	if err != nil {
		if !isNotFound(err) {
			resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		}
		return
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		if !isNotFound(err) {
			resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		}
		return
	}

	if !data.EradicateOnDestroy.ValueBool() {
		return
	}

	// Find the twin this delete itself just created by set difference — present now,
	// absent in the listing taken before the delete — rather than by timestamp: no
	// clock or name-parsing heuristic can tell "this run's twin" apart from a
	// same-named twin that already existed, but "new since before this call" always
	// can.
	after, err := listVolumes(ctx, client, vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName)
	if err != nil {
		// A 404 here means the volume group itself is gone, which takes its children
		// with it: a literal volume_group_id gives Terraform no ordering against the group,
		// so the group's delete can finish first.
		if isNotFound(err) {
			return
		}
		resp.Diagnostics.Append(eradicateOp.fail(newApiStepFailedError(eradicateOp, listVolumesStep, r.clients.apiFailure(err))))
		return
	}

	twinName, ok := volumeTwins.eradicationTarget(before, after, data.Name.ValueString())
	if !ok {
		resp.Diagnostics.Append(softDeletedTwinNotFoundWarning(kindVolume, data.Name.ValueString()))
		return
	}

	eradicatePoller, err := client.BeginDelete(ctx, vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName, twinName, nil)
	if err != nil {
		if !isNotFound(err) {
			resp.Diagnostics.Append(r.clients.apiCallError(eradicateOp, err))
		}
		return
	}
	if _, err := eradicatePoller.PollUntilDone(ctx, nil); err != nil && !isNotFound(err) {
		resp.Diagnostics.Append(r.clients.apiCallError(eradicateOp, err))
	}
}

// listVolumes pages through every volume — live and destroyed alike — in a volume
// group. The operation takes only its four path parameters and has no filter
// parameter, so both kinds always come back together in one collection, distinguished
// only by each entry's own softDeletion.destroyed.
func listVolumes(
	ctx context.Context, client *armpurestorageblock.VolumesClient, resourceGroupName, storagePoolName, volumeGroupName string,
) ([]*armpurestorageblock.Volume, error) {
	var all []*armpurestorageblock.Volume

	pager := client.NewListByVolumeGroupPager(resourceGroupName, storagePoolName, volumeGroupName, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Value...)
	}

	return all, nil
}

func (r *VolumeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if r.clients == nil {
		resp.Diagnostics.Append(operationConfigureProvider.fail(newProviderNotConfiguredError()))
		return
	}
	expectedSubscriptionID := r.clients.subscriptionID

	importID, diags := resolveImportID(ctx, req, kindVolume, armids.VolumeID.FullID, armids.ParseVolumeID)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	importOp := operation{action: actionImport, kind: kindVolume, id: importID, unnamed: true}

	parsed, idErr := parseIDInSubscription(armids.ParseVolumeID, importID, kindVolume, expectedSubscriptionID)
	if idErr != nil {
		resp.Diagnostics.Append(importOp.fail(*idErr))
		return
	}

	resp.Diagnostics.Append(resp.Identity.Set(ctx, parsed)...)
	resp.Diagnostics.Append(seedVolumeState(ctx, &resp.State, importID, parsed)...)
}

// MoveState takes over an azapi_resource of type PureStorage.Block/storagePools/volumeGroups/volumes.
func (r *VolumeResource) MoveState(context.Context) []resource.StateMover {
	return resourceMove[armids.VolumeID]{
		kind: kindVolume, sources: []moveSource{azapiResourceSource}, parse: armids.ParseVolumeID, seed: seedVolumeState,
	}.stateMovers()
}
