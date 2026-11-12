// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

var (
	_ resource.Resource                   = &VolumeGroupResource{}
	_ resource.ResourceWithConfigure      = &VolumeGroupResource{}
	_ resource.ResourceWithImportState    = &VolumeGroupResource{}
	_ resource.ResourceWithMoveState      = &VolumeGroupResource{}
	_ resource.ResourceWithIdentity       = &VolumeGroupResource{}
	_ resource.ResourceWithModifyPlan     = &VolumeGroupResource{}
	_ resource.ResourceWithValidateConfig = &VolumeGroupResource{}
)

// Per-operation defaults, overridable per resource instance via the `timeouts`
// attribute. Derived from the backend's own 15-minute cap on each volume group
// workflow, plus a 5-minute margin for the initial request and ARM/LRO polling;
// delete covers up to two sequential workflows (soft delete, then twin eradication
// when eradicate_on_destroy is set).
const (
	defaultVolumeGroupCreateTimeout = 20 * time.Minute
	defaultVolumeGroupReadTimeout   = 5 * time.Minute
	defaultVolumeGroupUpdateTimeout = 20 * time.Minute
	defaultVolumeGroupDeleteTimeout = 35 * time.Minute
)

type VolumeGroupResource struct {
	// clientsHolder supplies Configure, volumeGroupsClient, storagePoolsClient and
	// recoverableVolumeGroupsClient via promotion; see its own doc comment.
	clientsHolder
}

func NewVolumeGroupResource() resource.Resource {
	return &VolumeGroupResource{}
}

func (r *VolumeGroupResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_volume_group"
}

func (r *VolumeGroupResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = armids.VolumeGroupIdentitySchema()
}

func (r *VolumeGroupResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	immutable := []planmodifier.String{requiresReplaceProtected(kindVolumeGroup)}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Volume Group — a set of volumes sharing performance limits and a " +
			"snapshot protection schedule, on a Storage Pool.\n\n" +
			"~> **Destroying a volume group soft-deletes it and everything inside it.** The backend " +
			"disconnects any hosts and destroys the volume group's volumes along with it as one unit; " +
			"all of it stays recoverable until eradication. Volumes Terraform destroyed before the group " +
			"are soft-deleted separately and must each be recovered on their own. See " +
			"[Deletion policies](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/guides/deletion-policies).\n\n" +
			"To read a volume group managed elsewhere without taking ownership of it, use the " +
			"[`everpure_azure_volume_group` data source](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/data-sources/azure_volume_group).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				CustomType:    armids.IDType{},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "The full Azure Resource Manager ID of the volume group, e.g. " +
					"`/subscriptions/.../resourceGroups/.../providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg1`.",
			},
			"name": schema.StringAttribute{
				Required:            true,
				PlanModifiers:       immutable,
				Validators:          []validator.String{storageObjectNameValidator()},
				MarkdownDescription: "Name of the volume group. The name " + storageObjectNameRule + ". Changing this forces a new volume group to be created.",
			},
			"storage_pool_id": schema.StringAttribute{
				Required:   true,
				CustomType: armids.IDType{},
				PlanModifiers: append([]planmodifier.String{armids.PreserveEquivalentIDSpelling()},
					immutable...),
				Validators: []validator.String{
					armids.TypeStoragePool.Validator(),
				},
				MarkdownDescription: "Full Azure Resource Manager ID of the Storage Pool this volume group " +
					"lives on. Changing this forces a new volume group to be created.",
			},
			"source_type": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Default:       stringdefault.StaticString(volumeGroupSourceTypeNone),
				PlanModifiers: immutable,
				Validators: []validator.String{
					stringvalidator.OneOf(volumeGroupSourceTypeValues()...),
				},
				MarkdownDescription: "How the volume group's initial contents are created. One of `none` " +
					"(empty, the default), `volumeGroup` (clone from `source_volume_group_id`), `snapshot` " +
					"(restore from `source_snapshot_id`), or `recoverableVolumeGroup` (recover from " +
					"`source_recoverable_volume_group_id`). Changing this forces a new volume group to be created.",
			},
			"source_volume_group_id": schema.StringAttribute{
				Optional:   true,
				CustomType: armids.IDType{},
				PlanModifiers: append([]planmodifier.String{armids.PreserveEquivalentIDSpelling()},
					immutable...),
				Validators: []validator.String{
					armids.TypeVolumeGroup.Validator(),
				},
				MarkdownDescription: "Full Azure Resource Manager ID of the volume group to clone from. " +
					"Required when `source_type` is `volumeGroup`, and must be unset otherwise. Changing " +
					"this forces a new volume group to be created.",
			},
			"source_snapshot_id": schema.StringAttribute{
				Optional:   true,
				CustomType: armids.IDType{},
				PlanModifiers: append([]planmodifier.String{armids.PreserveEquivalentIDSpelling()},
					immutable...),
				Validators: []validator.String{
					armids.TypeVolumeGroupSnapshot.Validator(),
				},
				MarkdownDescription: "Full Azure Resource Manager ID of the volume group snapshot to restore " +
					"from. Required when `source_type` is `snapshot`, and must be unset otherwise. " +
					"Terraform needs read access to the snapshot's volume group, not just the snapshot. " +
					"Changing this forces a new volume group to be created.",
			},
			"source_recoverable_volume_group_id": schema.StringAttribute{
				Optional:   true,
				CustomType: armids.IDType{},
				PlanModifiers: append([]planmodifier.String{armids.PreserveEquivalentIDSpelling()},
					immutable...),
				Validators: []validator.String{
					armids.TypeRecoverableVolumeGroup.Validator(),
				},
				MarkdownDescription: "Full Azure Resource Manager ID of the soft-deleted volume group to " +
					"recover. Required when `source_type` is `recoverableVolumeGroup`, and must be unset " +
					"otherwise. Changing this forces a new volume group to be created.",
			},
			"bandwidth_limit_in_mbps": schema.Int64Attribute{
				Optional: true,
				Validators: []validator.Int64{
					int64validator.AtLeast(1),
				},
				MarkdownDescription: "Bandwidth limit for the volume group, in megabytes per second (MB/s). " +
					"Omit for no limit. Removing this attribute from configuration clears an existing limit. " +
					"Can be changed in place.",
			},
			"iops_limit": schema.Int64Attribute{
				Optional: true,
				Validators: []validator.Int64{
					int64validator.AtLeast(100),
				},
				MarkdownDescription: "I/O operations per second (IOPS) limit for the volume group. Omit for " +
					"no limit. Must be at least 100. Removing this attribute from configuration clears an " +
					"existing limit. Can be " +
					"changed in place.",
			},
			"tags": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "A mapping of tags to assign to the volume group. Can be changed in place. " +
					"Some environments append their own tags outside Terraform (e.g. an Azure Policy, or the " +
					"resource provider itself); including those tags in this configuration, or excluding them " +
					"via a `lifecycle.ignore_changes` block, avoids a persistent diff against them.",
			},
			"deletion_protection": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether to block destroying this volume group, or replacing it (which " +
					"destroys it as part of the replacement). Defaults to `true`. Set to `false`, and apply, " +
					"before a destroy or a change that would otherwise force replacement.",
			},
			"eradicate_on_destroy": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Whether destroy permanently eradicates the volume group (and the " +
					"volumes inside it) instead of soft-deleting it. Defaults to `false`. There is no " +
					"recovery window once eradicated.",
			},
			"internal_id": schema.StringAttribute{
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "Everpure's internal ID of the volume group.",
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeoutsBlock(ctx, "the volume group", timeoutsDefaults{
				Create: defaultVolumeGroupCreateTimeout,
				Read:   defaultVolumeGroupReadTimeout,
				Update: defaultVolumeGroupUpdateTimeout,
				Delete: defaultVolumeGroupDeleteTimeout,
			}, ", including eradicating its recoverable copy when eradicate_on_destroy is set"),
			"snapshot_protection_policy": schema.ListNestedBlock{
				MarkdownDescription: "The volume group's snapshot schedule. Absent means no schedule. Can be " +
					"added, changed or removed in place.",
				Validators: []validator.List{
					listvalidator.SizeAtMost(1),
				},
				NestedObject: schema.NestedBlockObject{
					// Each durationType attribute needs both halves of the semantic
					// comparison: CustomType covers apply, the plan modifier covers plan.
					// Adding one without the other reintroduces a diff after import.
					Attributes: map[string]schema.Attribute{
						"frequency": schema.StringAttribute{
							Required:      true,
							CustomType:    durationType{},
							PlanModifiers: []planmodifier.String{preserveEquivalentDurationSpelling()},
							MarkdownDescription: "How often to take a snapshot, as an ISO 8601 duration using " +
								"days or smaller units, e.g. `\"PT1H\"` or `\"P1D\"`.",
						},
						"retention": schema.StringAttribute{
							Required:      true,
							CustomType:    durationType{},
							PlanModifiers: []planmodifier.String{preserveEquivalentDurationSpelling()},
							MarkdownDescription: "How long to keep snapshots taken by this schedule, as an " +
								"ISO 8601 duration using days or smaller units, e.g. `\"P7D\"`.",
						},
					},
				},
			},
		},
	}
}

// ValidateConfig enforces the source_type / source_*_id matrix: exactly the one
// source_*_id attribute source_type names must be set, and the other two must not be.
func (r *VolumeGroupResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var data VolumeGroupResourceModel
	if resp.Diagnostics.Append(req.Config.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	if data.SourceType.IsUnknown() {
		return
	}
	sourceType := volumeGroupSourceTypeOrDefault(data.SourceType)
	isSet := func(v types.String) bool { return !v.IsNull() && !v.IsUnknown() }

	if sourceType != volumeGroupSourceTypeNone {
		// Only a null value fails the required check here: an unknown one (e.g. a
		// reference to another resource's id, not yet known at plan time — the
		// standard create-from-source pattern) still satisfies it, since Create's own
		// requireSourceAttribute check below runs again once the value has resolved.
		if wantPath, missing := missingSourceAttribute(sourceType, data); missing {
			resp.Diagnostics.Append(diag.WithPath(wantPath, invalidAttributeCombinationDiagnostic(newSourceAttributeRequiredError(wantPath, sourceType))))
		}
		if _, ok := volumeGroupSourceRowFor(sourceType); !ok {
			// An invalid source_type is already reported by the OneOf validator.
			return
		}
	}

	// Every row not named by sourceType must be unset. This also covers
	// source_type = "none", since "none" names no row at all.
	for _, row := range volumeGroupSourceRows {
		if row.sourceType == sourceType || !isSet(row.get(data)) {
			continue
		}
		reason := newSourceAttributeNotAllowedError(row.attribute, sourceType, false)
		if sourceType == volumeGroupSourceTypeNone {
			reason = newSourceAttributeNotAllowedError(row.attribute, volumeGroupSourceTypeNone, true)
		}
		resp.Diagnostics.Append(diag.WithPath(row.attribute, invalidAttributeCombinationDiagnostic(reason)))
	}
}

// ModifyPlan blocks a destroy plan while the prior state's deletion_protection is
// true — matching deletion_protection = true, which is the default. A plan that would
// force this volume group to be replaced is blocked too, but not here: each immutable
// attribute's own requiresReplaceProtected plan modifier (see
// requires_replace_protected.go) raises that error itself, since it is the one place
// that already knows, attribute by attribute, whether its own change is what
// triggered the replacement. Delete carries a matching deletion_protection check as
// defence in depth, in case a future change bypasses either guard.
func (r *VolumeGroupResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() {
		return // create: nothing to protect yet.
	}
	if !req.Plan.Raw.IsNull() {
		return // not a destroy: any replace this plan forces is guarded on the attribute itself.
	}

	var state VolumeGroupResourceModel
	if resp.Diagnostics.Append(req.State.Get(ctx, &state)...); resp.Diagnostics.HasError() {
		return
	}

	if !state.DeletionProtection.ValueBool() {
		return
	}

	resp.Diagnostics.Append(deletionProtectedDiagnostic(kindVolumeGroup, newDeletionProtectedError(kindVolumeGroup, state.Name.ValueString())))
}

func (r *VolumeGroupResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	client, ok := r.volumeGroupsClient(&resp.Diagnostics)
	if !ok {
		return
	}
	poolsClient, ok := r.storagePoolsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data VolumeGroupResourceModel
	if resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	// ValidateConfig lets an unknown source_*_id (a reference to another
	// resource's id, not yet known at plan time) through; by now the plan has
	// resolved it to a concrete value, so recheck it didn't resolve to null —
	// toVolumeGroup would otherwise send source_type with an empty-string
	// resource ID, surfacing as an opaque backend 400 instead of this.
	sourceType := volumeGroupSourceTypeOrDefault(data.SourceType)
	if wantPath, missing := missingSourceAttribute(sourceType, data); missing {
		resp.Diagnostics.Append(diag.WithPath(wantPath, invalidAttributeCombinationDiagnostic(newSourceAttributeRequiredError(wantPath, sourceType))))
		return
	}

	createTimeout, diags := data.Timeouts.Create(ctx, defaultVolumeGroupCreateTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()

	op := operation{action: actionCreate, kind: kindVolumeGroup, name: data.Name.ValueString()}
	pool, idErr := parseIDInSubscription(armids.ParseStoragePoolID, data.StoragePoolID.ValueString(), kindStoragePool, r.clients.subscriptionID)
	if idErr != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(*idErr)))
		return
	}
	op.id = pool.VolumeGroup(op.name).FullID()

	poolResult, err := poolsClient.Get(ctx, pool.ResourceGroupName, pool.StoragePoolName, nil)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(newNotFoundError(kindStoragePool, pool.FullID(), nil, r.clients.apiFailure(err)))))
			return
		}
		resp.Diagnostics.Append(op.fail(newApiStepFailedError(op, step{action: actionRetrieve, kind: kindStoragePool, id: pool.FullID()}, r.clients.apiFailure(err))))
		return
	}
	if poolResult.Location == nil {
		resp.Diagnostics.Append(unexpectedResponseDiagnostic(newEmptyLocationError(kindStoragePool)))
		return
	}

	vg, diags := toVolumeGroup(ctx, data, *poolResult.Location)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	existing, err := client.Get(ctx, pool.ResourceGroupName, pool.StoragePoolName, data.Name.ValueString(), nil)
	var existingState string
	if err == nil && existing.Properties != nil {
		existingState = provisioningStateName(existing.Properties.ProvisioningState)
	}
	if d := r.clients.existingObjectDiagnostic(op, "everpure_azure_volume_group", existing.ID, existingState, err); d != nil {
		resp.Diagnostics.Append(d)
		return
	}

	poller, err := client.BeginCreate(ctx, pool.ResourceGroupName, pool.StoragePoolName, data.Name.ValueString(), vg, nil)
	if err != nil {
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	// Save minimal state as soon as the create is accepted, so a failed poll leaves
	// a tainted resource Terraform knows about instead of an orphan.
	// The id is set from the requested components so the tainted state carries an
	// id and an identity that agree; the final pair below comes from the API's id.
	data.ID = armids.IDValue{StringValue: types.StringValue(pool.VolumeGroup(data.Name.ValueString()).FullID())}
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, data.ID.ValueString(), armids.ParseVolumeGroupID)...)
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

	resp.Diagnostics.Append(applyVolumeGroup(ctx, &data, result.VolumeGroup)...)
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, data.ID.ValueString(), armids.ParseVolumeGroupID)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *VolumeGroupResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	client, ok := r.volumeGroupsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data VolumeGroupResourceModel
	if resp.Diagnostics.Append(req.State.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	readTimeout, diags := data.Timeouts.Read(ctx, defaultVolumeGroupReadTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	op := operation{action: actionRetrieve, kind: kindVolumeGroup, name: data.Name.ValueString()}
	pool, err := armids.ParseStoragePoolID(data.StoragePoolID.ValueString())
	if err != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(newInvalidStoredIDError(storedInState, idErrorCause(err)))))
		return
	}
	op.id = pool.VolumeGroup(op.name).FullID()
	if c := subscriptionMismatchCause(pool, kindStoragePool, r.clients.subscriptionID); c != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(*c)))
		return
	}

	result, err := client.Get(ctx, pool.ResourceGroupName, pool.StoragePoolName, data.Name.ValueString(), nil)
	if err != nil {
		if isNotFound(err) {
			// Covers both "deleted outside Terraform" and a soft delete: either
			// way, the backend renames the object on delete, so GET by the
			// original name 404s immediately, before eradication.
			resp.Diagnostics.Append(setIdentityFromIDIfNull(ctx, resp.Identity, idOrFallback(data.ID, pool.VolumeGroup(data.Name.ValueString()).FullID()), armids.ParseVolumeGroupID)...)
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	// The identity comes from the id already in state, read before the apply
	// below overwrites it with the API's spelling. The id is null in a tainted state
	// from a failed create; the API's id then becomes the state id, so the identity follows it.
	identityID := data.ID.ValueString()
	resp.Diagnostics.Append(applyVolumeGroupRead(ctx, &data, result.VolumeGroup)...)
	if identityID == "" {
		identityID = data.ID.ValueString()
	}
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, identityID, armids.ParseVolumeGroupID)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *VolumeGroupResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data VolumeGroupResourceModel
	if resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	var priorState VolumeGroupResourceModel
	if resp.Diagnostics.Append(req.State.Get(ctx, &priorState)...); resp.Diagnostics.HasError() {
		return
	}

	// An in-place update never changes the id, so the identity is carried over
	// from state unchanged.
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, priorState.ID.ValueString(), armids.ParseVolumeGroupID)...)

	// deletion_protection and eradicate_on_destroy are provider-only: the API has
	// never heard of them, so a plan that changes only those two has nothing for a
	// PATCH to do. When every API-backed attribute below is unchanged from state,
	// the PATCH body built from the plan would just restate the volume group's
	// current values back at it — a no-op round trip — so skip building a client
	// and calling the API entirely and persist the plan as-is.
	propertiesChanged := !data.BandwidthLimitInMbps.Equal(priorState.BandwidthLimitInMbps) ||
		!data.IopsLimit.Equal(priorState.IopsLimit) ||
		!data.SnapshotProtectionPolicy.Equal(priorState.SnapshotProtectionPolicy)
	tagsChanged := !data.Tags.Equal(priorState.Tags)
	if !propertiesChanged && !tagsChanged {
		resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
		return
	}

	client, ok := r.volumeGroupsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	updateTimeout, diags := data.Timeouts.Update(ctx, defaultVolumeGroupUpdateTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()

	op := operation{action: actionUpdate, kind: kindVolumeGroup, name: data.Name.ValueString()}
	pool, err := armids.ParseStoragePoolID(data.StoragePoolID.ValueString())
	if err != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(newInvalidStoredIDError(storedInPlan, idErrorCause(err)))))
		return
	}
	op.id = pool.VolumeGroup(op.name).FullID()
	if c := subscriptionMismatchCause(pool, kindStoragePool, r.clients.subscriptionID); c != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(*c)))
		return
	}

	// Tags and properties go in separate PATCHes: the backend ignores the tags of a
	// PATCH that also carries properties. Each PATCH is sent only if its part changed,
	// and each completes before the next is sent.
	var updates []armpurestorageblock.VolumeGroupUpdate
	if tagsChanged {
		update, diags := toVolumeGroupTagsUpdate(ctx, data)
		if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
			return
		}
		updates = append(updates, update)
	}
	if propertiesChanged {
		update, diags := toVolumeGroupUpdate(ctx, data)
		if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
			return
		}
		updates = append(updates, update)
	}

	var result armpurestorageblock.VolumeGroupsClientUpdateResponse
	for _, update := range updates {
		poller, err := client.BeginUpdate(ctx, pool.ResourceGroupName, pool.StoragePoolName, data.Name.ValueString(), update, nil)
		if err == nil {
			result, err = poller.PollUntilDone(ctx, nil)
		}
		if err != nil {
			if isNotFound(err) {
				resp.Diagnostics.Append(op.fail(newDeletedOutsideTerraformError(kindVolumeGroup, op.id)))
				return
			}
			resp.Diagnostics.Append(r.clients.apiCallError(op, err))
			return
		}
	}

	resp.Diagnostics.Append(applyVolumeGroup(ctx, &data, result.VolumeGroup)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *VolumeGroupResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data VolumeGroupResourceModel
	if resp.Diagnostics.Append(req.State.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	if data.DeletionProtection.ValueBool() {
		resp.Diagnostics.Append(deletionProtectedDiagnostic(kindVolumeGroup, newDeletionProtectedError(kindVolumeGroup, data.Name.ValueString())))
		return
	}

	client, ok := r.volumeGroupsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	deleteTimeout, diags := data.Timeouts.Delete(ctx, defaultVolumeGroupDeleteTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()

	op := operation{action: actionDelete, kind: kindVolumeGroup, name: data.Name.ValueString()}
	pool, err := armids.ParseStoragePoolID(data.StoragePoolID.ValueString())
	if err != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(newInvalidStoredIDError(storedInState, idErrorCause(err)))))
		return
	}
	op.id = pool.VolumeGroup(op.name).FullID()
	if c := subscriptionMismatchCause(pool, kindStoragePool, r.clients.subscriptionID); c != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(*c)))
		return
	}
	eradicateOp := op
	eradicateOp.action = actionEradicate
	listRecoverable := step{action: actionList, kind: kindRecoverableVolumeGroups, id: pool.FullID()}

	var recoverableClient *armpurestorageblock.RecoverableVolumeGroupsClient
	var before []*armpurestorageblock.RecoverableVolumeGroup
	// matchName is what a candidate twin's softDeletion.previousName is compared
	// against below. It starts out as state's own copy of the name — the fallback
	// used only if the GET just below doesn't override it — and is never folded to
	// ignore letter case: soft delete frees the exact spelling it renamed away from,
	// so two twins can coexist whose previousName values differ only by case, and
	// folding the comparison could eradicate the wrong one, a mistake with no way
	// back.
	matchName := data.Name.ValueString()
	if data.EradicateOnDestroy.ValueBool() {
		var ok bool
		recoverableClient, ok = r.recoverableVolumeGroupsClient(&resp.Diagnostics)
		if !ok {
			return
		}
		before, err = listRecoverableVolumeGroups(ctx, recoverableClient, pool.ResourceGroupName, pool.StoragePoolName)
		if err != nil {
			resp.Diagnostics.Append(eradicateOp.fail(newApiStepFailedError(eradicateOp, listRecoverable, r.clients.apiFailure(err))))
			return
		}

		// Names fold case-insensitively for lookups but preserve whatever case they
		// were created or renamed under, so state's copy can legitimately disagree
		// with the server's own record on letter case alone — a volume group
		// imported under a different case than the server's own spelling is the
		// route in. Read does not correct it: name is a Required attribute, and
		// replacing state's value with the API's spelling would make a later plan
		// disagree with the configured one. Unlike the volume
		// resource, there's no earlier listing of this exact volume group to take
		// its spelling from instead (recoverable volume groups are their own
		// collection, and this one isn't in it yet), so learn the server's own
		// spelling directly with a GET before deleting it. If the GET fails for any
		// reason, matchName is left at its state-derived fallback above: proceeding
		// with the delete and, if nothing then matches, warning rather than
		// aborting the destroy is better than failing it over a lookup that isn't
		// itself required to delete anything.
		if got, getErr := client.Get(ctx, pool.ResourceGroupName, pool.StoragePoolName, data.Name.ValueString(), nil); getErr == nil && got.Name != nil {
			matchName = *got.Name
		}
	}

	poller, err := client.BeginDelete(ctx, pool.ResourceGroupName, pool.StoragePoolName, data.Name.ValueString(), nil)
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

	// Find the twin this delete itself just created by set difference — present
	// now, absent in the listing taken before the delete — rather than by
	// timestamp: no clock or name-parsing heuristic can tell "this run's twin"
	// apart from a same-named twin that already existed, but "new since before
	// this call" always can.
	after, err := listRecoverableVolumeGroups(ctx, recoverableClient, pool.ResourceGroupName, pool.StoragePoolName)
	if err != nil {
		resp.Diagnostics.Append(eradicateOp.fail(newApiStepFailedError(eradicateOp, listRecoverable, r.clients.apiFailure(err))))
		return
	}

	target := selectEradicationTarget(newRecoverableVolumeGroups(before, after), matchName)
	if target == nil || target.Name == nil {
		// The desired end state (gone for good) is already true — either
		// auto-eradication won the race, or someone eradicated it manually — and a
		// hard error here would be unretryable. This is also the fallback outcome
		// when the pre-delete GET above failed and matchName's own guess (state's
		// name) doesn't happen to match either.
		resp.Diagnostics.Append(recoverableVolumeGroupNotFoundWarning(data.Name.ValueString()))
		return
	}

	eradicatePoller, err := recoverableClient.BeginDelete(ctx, pool.ResourceGroupName, pool.StoragePoolName, *target.Name, nil)
	if err != nil {
		resp.Diagnostics.Append(r.clients.apiCallError(eradicateOp, err))
		return
	}
	if _, err := eradicatePoller.PollUntilDone(ctx, nil); err != nil {
		resp.Diagnostics.Append(r.clients.apiCallError(eradicateOp, err))
	}
}

// listRecoverableVolumeGroups pages through every recoverable volume group on the pool.
func listRecoverableVolumeGroups(
	ctx context.Context, client *armpurestorageblock.RecoverableVolumeGroupsClient, resourceGroupName, storagePoolName string,
) ([]*armpurestorageblock.RecoverableVolumeGroup, error) {
	var all []*armpurestorageblock.RecoverableVolumeGroup

	pager := client.NewListByStoragePoolPager(resourceGroupName, storagePoolName, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Value...)
	}

	return all, nil
}

// newRecoverableVolumeGroups returns the entries in after whose name isn't present in
// before, so callers can find exactly the twin a specific delete just created —
// without relying on clocks or parsing a timestamp out of a name — even when an
// unrelated twin with the same previousName already existed.
func newRecoverableVolumeGroups(
	before, after []*armpurestorageblock.RecoverableVolumeGroup,
) []*armpurestorageblock.RecoverableVolumeGroup {
	beforeNames := make(map[string]bool, len(before))
	for _, twin := range before {
		if twin != nil && twin.Name != nil {
			beforeNames[*twin.Name] = true
		}
	}

	var result []*armpurestorageblock.RecoverableVolumeGroup
	for _, twin := range after {
		if twin == nil || twin.Name == nil {
			continue
		}
		if !beforeNames[*twin.Name] {
			result = append(result, twin)
		}
	}
	return result
}

// selectEradicationTarget picks, among newly appeared recoverable twins (see
// newRecoverableVolumeGroups), the one whose softDeletion.previousName equals name.
// Normally there is at most one: a delete creates exactly one twin. If several
// somehow qualify, the newest by softDeletion.destroyedAt wins; nil means none did.
func selectEradicationTarget(newTwins []*armpurestorageblock.RecoverableVolumeGroup, name string) *armpurestorageblock.RecoverableVolumeGroup {
	var selected *armpurestorageblock.RecoverableVolumeGroup
	var selectedAt time.Time

	for _, twin := range newTwins {
		if twin.Properties == nil || twin.Properties.SoftDeletion == nil {
			continue
		}
		softDeletion := twin.Properties.SoftDeletion
		if softDeletion.PreviousName == nil || *softDeletion.PreviousName != name {
			continue
		}

		var destroyedAt time.Time
		if softDeletion.DestroyedAt != nil {
			destroyedAt = *softDeletion.DestroyedAt
		}
		if selected == nil || destroyedAt.After(selectedAt) {
			selected, selectedAt = twin, destroyedAt
		}
	}

	return selected
}

func (r *VolumeGroupResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if r.clients == nil {
		resp.Diagnostics.Append(operationConfigureProvider.fail(newProviderNotConfiguredError()))
		return
	}
	expectedSubscriptionID := r.clients.subscriptionID

	importID, diags := resolveImportID(ctx, req, kindVolumeGroup, armids.VolumeGroupID.FullID, armids.ParseVolumeGroupID)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	importOp := operation{action: actionImport, kind: kindVolumeGroup, id: importID, unnamed: true}

	parsed, idErr := parseIDInSubscription(armids.ParseVolumeGroupID, importID, kindVolumeGroup, expectedSubscriptionID)
	if idErr != nil {
		resp.Diagnostics.Append(importOp.fail(*idErr))
		return
	}

	resp.Diagnostics.Append(resp.Identity.Set(ctx, parsed)...)
	resp.Diagnostics.Append(seedVolumeGroupState(ctx, &resp.State, importID, parsed)...)
}

// MoveState takes over an azapi_resource of type PureStorage.Block/storagePools/volumeGroups.
func (r *VolumeGroupResource) MoveState(context.Context) []resource.StateMover {
	return resourceMove[armids.VolumeGroupID]{
		kind: kindVolumeGroup, sources: []moveSource{azapiResourceSource}, parse: armids.ParseVolumeGroupID, seed: seedVolumeGroupState,
	}.stateMovers()
}
