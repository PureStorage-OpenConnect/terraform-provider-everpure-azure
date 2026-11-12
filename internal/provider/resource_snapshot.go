// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

var (
	_ resource.Resource                = &SnapshotResource{}
	_ resource.ResourceWithConfigure   = &SnapshotResource{}
	_ resource.ResourceWithImportState = &SnapshotResource{}
	_ resource.ResourceWithMoveState   = &SnapshotResource{}
	_ resource.ResourceWithModifyPlan  = &SnapshotResource{}
	_ resource.ResourceWithIdentity    = &SnapshotResource{}
)

// Per-operation defaults, overridable per resource instance via the `timeouts`
// attribute. The values match the volume's: create adds margin over the backend's own
// 15-minute cap on a single workflow, and delete covers up to two sequential workflows
// (soft delete, then eradication when eradicate_on_destroy is set). There is no update
// timeout because Update makes no API call.
const (
	defaultSnapshotCreateTimeout = 20 * time.Minute
	defaultSnapshotReadTimeout   = 5 * time.Minute
	defaultSnapshotDeleteTimeout = 35 * time.Minute
)

// The API does the snapshot lists' filtering, ordering and paging: snapshotsOrderBy puts
// the newest first, liveSnapshotsFilter drops soft-deleted snapshots, manualSnapshotsFilter
// also drops the ones the snapshot schedule took, and maxSnapshotListPageSize is the
// largest $top the list accepts.
const (
	snapshotsOrderBy        = "createdAt desc"
	liveSnapshotsFilter     = "not(softDeletion/destroyed)"
	manualSnapshotsFilter   = liveSnapshotsFilter + " and createdByPolicy eq false"
	maxSnapshotListPageSize = 100
)

type SnapshotResource struct {
	// clientsHolder supplies Configure and volumeGroupSnapshotsClient via promotion; see
	// its own doc comment.
	clientsHolder
}

func NewSnapshotResource() resource.Resource {
	return &SnapshotResource{}
}

func (r *SnapshotResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_snapshot"
}

func (r *SnapshotResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = armids.VolumeGroupSnapshotIdentitySchema()
}

func (r *SnapshotResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	immutable := []planmodifier.String{requiresReplaceProtected(kindSnapshot)}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a manual Volume Group Snapshot: a point-in-time snapshot of every volume " +
			"in a Volume Group.\n\n" +
			"A snapshot is immutable once created, and snapshots taken by this resource are never expired by " +
			"the volume group's snapshot schedule: each one stays until it is destroyed.\n\n" +
			"~> **Destroying the parent volume group soft-deletes this snapshot with it.** The backend " +
			"destroys every snapshot in a volume group as part of destroying the group itself, and " +
			"eradicating the group eradicates its snapshots. See " +
			"[Deletion policies](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/guides/deletion-policies).\n\n" +
			"To read a snapshot managed elsewhere, including one taken by the volume group's snapshot " +
			"schedule, use the " +
			"[`everpure_azure_snapshot` data source](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/data-sources/azure_snapshot). A snapshot " +
			"taken by the schedule cannot be managed by this resource.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				CustomType:    armids.IDType{},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "The full Azure Resource Manager ID of the snapshot, e.g. " +
					"`/subscriptions/.../resourceGroups/.../providers/PureStorage.Block/storagePools/pool1/" +
					"volumeGroups/vg1/snapshots/snap1`.",
			},
			"name": schema.StringAttribute{
				Required:            true,
				PlanModifiers:       immutable,
				Validators:          []validator.String{storageObjectNameValidator()},
				MarkdownDescription: "Name of the snapshot. The name " + storageObjectNameRule + ". Changing this forces a new snapshot to be created.",
			},
			"volume_group_id": schema.StringAttribute{
				Required:   true,
				CustomType: armids.IDType{},
				PlanModifiers: append([]planmodifier.String{armids.PreserveEquivalentIDSpelling()},
					immutable...),
				Validators: []validator.String{
					armids.TypeVolumeGroup.Validator(),
				},
				MarkdownDescription: "Full Azure Resource Manager ID of the volume group to snapshot. " +
					"Changing this forces a new snapshot to be created.",
			},
			"source_recoverable_snapshot_id": schema.StringAttribute{
				Optional:   true,
				CustomType: armids.IDType{},
				PlanModifiers: append([]planmodifier.String{armids.PreserveEquivalentIDSpelling()},
					immutable...),
				Validators: []validator.String{
					armids.TypeVolumeGroupSnapshot.Validator(),
				},
				MarkdownDescription: "Full Azure Resource Manager ID of a soft-deleted snapshot to recover " +
					"instead of taking a new one. The snapshot is restored (not copied) under `name` and must " +
					"be in the same volume group. Snapshots taken by the snapshot schedule can't be recovered " +
					"through this resource. Changing this forces a new snapshot to be created. Imported " +
					"snapshots always have it unset.",
			},
			"created_at": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "When the snapshot was created, as an RFC 3339 timestamp in UTC. " +
					"Timestamps sort chronologically as strings.",
			},
			"volume_snapshots": schema.ListNestedAttribute{
				Computed:            true,
				PlanModifiers:       []planmodifier.List{listplanmodifier.UseStateForUnknown()},
				MarkdownDescription: volumeSnapshotsDescription,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"volume_name": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: volumeSnapshotVolumeNameDescription,
						},
						"provisioned_size_in_bytes": schema.Int64Attribute{
							Computed:            true,
							MarkdownDescription: volumeSnapshotSizeDescription,
						},
						"serial_number": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: volumeSnapshotSerialDescription,
						},
					},
				},
			},
			"deletion_protection": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether to block destroying this snapshot, or replacing it (which " +
					"destroys it as part of the replacement). Defaults to `true`. Set to `false`, and " +
					"apply, before a destroy or a change that would otherwise force replacement.",
			},
			"eradicate_on_destroy": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Whether destroy permanently eradicates the snapshot instead of " +
					"soft-deleting it. Defaults to `false`. There is no recovery window once eradicated.",
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeoutsBlock(ctx, "the snapshot", timeoutsDefaults{
				Create: defaultSnapshotCreateTimeout,
				Read:   defaultSnapshotReadTimeout,
				Delete: defaultSnapshotDeleteTimeout,
			}, ", including eradicating its recoverable copy when eradicate_on_destroy is set"),
		},
	}
}

// ModifyPlan blocks a destroy plan while the prior state's deletion_protection is true,
// and on a plan that carries source_recoverable_snapshot_id, checks the recovery source.
// The source must sit in volume_group_id's own volume group (no API call). On create or
// replacement it must also exist, be soft-deleted and not have been taken by the
// snapshot schedule (one GET, see checkRecoverySource). A plan that would force
// replacement is blocked separately, by each immutable attribute's own
// requiresReplaceProtected plan modifier. Delete carries a matching deletion_protection
// check as defence in depth.
func (r *SnapshotResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		if req.State.Raw.IsNull() {
			return // a plan to destroy something that never existed: nothing to do.
		}
		var state SnapshotResourceModel
		if resp.Diagnostics.Append(req.State.Get(ctx, &state)...); resp.Diagnostics.HasError() {
			return
		}
		if !state.DeletionProtection.ValueBool() {
			return
		}
		resp.Diagnostics.Append(deletionProtectedDiagnostic(kindSnapshot, newDeletionProtectedError(kindSnapshot, state.Name.ValueString())))
		return
	}

	var plan SnapshotResourceModel
	if resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...); resp.Diagnostics.HasError() {
		return
	}

	if !isDefinitelySet(plan.SourceRecoverableSnapshotID) {
		return
	}

	if resp.Diagnostics.Append(checkSnapshotRecoverySource(plan.VolumeGroupID, plan.SourceRecoverableSnapshotID)...); resp.Diagnostics.HasError() {
		return
	}

	// A replacement re-runs the recovery, which needs the source to still be
	// soft-deleted, but the first recovery renamed it away. Checking it again catches
	// that at plan time instead of failing at apply. The framework hands ModifyPlan a
	// fresh, empty resp.RequiresReplace (attribute plan modifiers' results are merged in
	// only after it returns), so replacement is detected here by comparing the immutable
	// attributes.
	creating := req.State.Raw.IsNull()
	replacing := false
	if !creating {
		var state SnapshotResourceModel
		if resp.Diagnostics.Append(req.State.Get(ctx, &state)...); resp.Diagnostics.HasError() {
			return
		}
		replacing = !state.Name.Equal(plan.Name) ||
			!state.VolumeGroupID.Equal(plan.VolumeGroupID) ||
			!state.SourceRecoverableSnapshotID.Equal(plan.SourceRecoverableSnapshotID)
	}

	// r.clients == nil means Configure deferred (a provider configuration value,
	// subscription_id or an authentication attribute, isn't known yet in this plan):
	// the GET would only fail with "provider not configured", hard-failing
	// a plan that a later Configure call could still complete. Create runs the same
	// check at apply.
	if (creating || replacing) && r.clients != nil {
		ctx, cancel := context.WithTimeout(ctx, defaultSnapshotReadTimeout)
		defer cancel()
		op := snapshotPlanOperation(plan)
		r.checkRecoverySource(ctx, op, plan.SourceRecoverableSnapshotID.ValueString(), &resp.Diagnostics)
	}
}

// snapshotPlanOperation is the operation ModifyPlan reports Azure failures under. A
// volume_group_id that does not parse has been reported already, so the snapshot has no ID
// of its own and the operation reports the volume group it acts in instead.
func snapshotPlanOperation(plan SnapshotResourceModel) operation {
	op := operation{action: actionPlan, kind: kindSnapshot, name: plan.Name.ValueString(), in: plan.VolumeGroupID.ValueString()}
	if vg, err := armids.ParseVolumeGroupID(op.in); err == nil {
		op.id = vg.Snapshot(op.name).FullID()
	}
	return op
}

// checkRecoverySource GETs the snapshot named by sourceID and reports, as an error on
// source_recoverable_snapshot_id, a source that does not exist, is not soft-deleted, or
// was taken by the volume group's snapshot schedule. It reports whether the source is
// usable. Shared by ModifyPlan and Create, so a bad source fails the plan, not just the
// apply.
//
// The API's own createdByPolicy is authoritative here; a guess from the snapshot's name
// can disagree with it. The other two problems are reported here too, so they surface
// as a plan error rather than a failed create operation.
func (r *SnapshotResource) checkRecoverySource(ctx context.Context, op operation, sourceID string, diags *diag.Diagnostics) bool {
	sourcePath := path.Root("source_recoverable_snapshot_id")

	source, idErr := parseIDInSubscription(armids.ParseVolumeGroupSnapshotID, sourceID, kindSnapshot, r.clients.subscriptionID)
	if idErr != nil {
		diags.Append(diag.WithPath(sourcePath, invalidAttributeValueDiagnostic(*idErr)))
		return false
	}

	client, ok := r.volumeGroupSnapshotsClient(diags)
	if !ok {
		return false
	}

	result, err := client.Get(ctx, source.ResourceGroupName, source.StoragePoolName, source.VolumeGroupName, source.SnapshotName, nil)
	if err != nil {
		if isNotFound(err) {
			diags.Append(diag.WithPath(sourcePath, invalidAttributeValueDiagnostic(newRecoverySourceNotFoundError(source.FullID(), r.clients.apiFailure(err)))))
			return false
		}
		diags.Append(op.fail(newApiStepFailedError(op, step{action: actionRetrieve, kind: kindSnapshot, id: source.FullID()}, r.clients.apiFailure(err))))
		return false
	}

	if snapshotCreatedByPolicy(result.VolumeGroupSnapshot) {
		diags.Append(diag.WithPath(sourcePath, invalidAttributeValueDiagnostic(newRecoverySourceCreatedByScheduleError(source.FullID()))))
		return false
	}
	if !isSnapshotSoftDeleted(result.VolumeGroupSnapshot) {
		diags.Append(diag.WithPath(sourcePath, invalidAttributeValueDiagnostic(newRecoverySourceNotSoftDeletedError(source.FullID()))))
		return false
	}
	return true
}

func (r *SnapshotResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	client, ok := r.volumeGroupSnapshotsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data SnapshotResourceModel
	if resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	createTimeout, diags := data.Timeouts.Create(ctx, defaultSnapshotCreateTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()

	op := operation{action: actionCreate, kind: kindSnapshot, name: data.Name.ValueString()}
	vg, idErr := parseIDInSubscription(armids.ParseVolumeGroupID, data.VolumeGroupID.ValueString(), kindVolumeGroup, r.clients.subscriptionID)
	if idErr != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(*idErr)))
		return
	}
	op.id = vg.Snapshot(op.name).FullID()

	// ModifyPlan lets an unknown source id through (a reference to another resource's
	// value, not yet known at plan time) and skips the GET when the provider itself was
	// not yet configured; by now every value is concrete, so run both checks again.
	if isDefinitelySet(data.SourceRecoverableSnapshotID) {
		if resp.Diagnostics.Append(checkSnapshotRecoverySource(data.VolumeGroupID, data.SourceRecoverableSnapshotID)...); resp.Diagnostics.HasError() {
			return
		}
		if !r.checkRecoverySource(ctx, op, data.SourceRecoverableSnapshotID.ValueString(), &resp.Diagnostics) {
			return
		}
	}

	name := data.Name.ValueString()
	existing, err := client.Get(ctx, vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName, name, nil)
	var existingState string
	if err == nil && existing.Properties != nil {
		existingState = provisioningStateName(existing.Properties.ProvisioningState)
	}
	if d := r.clients.existingObjectDiagnostic(op, "everpure_azure_snapshot", existing.ID, existingState, err); d != nil {
		resp.Diagnostics.Append(d)
		return
	}

	poller, err := client.BeginCreate(ctx, vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName, name, toSnapshot(data), nil)
	if err != nil {
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	// Save minimal state as soon as the create is accepted, so a failed poll leaves
	// a tainted resource Terraform knows about instead of an orphan.
	// The id is set from the requested components so the tainted state carries an
	// id and an identity that agree; the final pair below comes from the API's id.
	data.ID = armids.IDValue{StringValue: types.StringValue(vg.Snapshot(name).FullID())}
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, data.ID.ValueString(), armids.ParseVolumeGroupSnapshotID)...)
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

	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	// The member volume snapshots are filled in only by a single GET, not by the
	// create response, so read the snapshot back rather than map the poller's result.
	result, err := client.Get(ctx, vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName, name, nil)
	if err != nil {
		resp.Diagnostics.Append(op.fail(newApiStepFailedError(op, step{action: actionRetrieve, kind: kindSnapshot, id: op.id}, r.clients.apiFailure(err))))
		return
	}

	resp.Diagnostics.Append(applySnapshot(ctx, &data, result.VolumeGroupSnapshot)...)
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, data.ID.ValueString(), armids.ParseVolumeGroupSnapshotID)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *SnapshotResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	client, ok := r.volumeGroupSnapshotsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data SnapshotResourceModel
	if resp.Diagnostics.Append(req.State.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	readTimeout, diags := data.Timeouts.Read(ctx, defaultSnapshotReadTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	op := operation{action: actionRetrieve, kind: kindSnapshot, name: data.Name.ValueString()}
	vg, err := armids.ParseVolumeGroupID(data.VolumeGroupID.ValueString())
	if err != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(newInvalidStoredIDError(storedInState, idErrorCause(err)))))
		return
	}
	op.id = vg.Snapshot(op.name).FullID()
	if c := subscriptionMismatchCause(vg, kindVolumeGroup, r.clients.subscriptionID); c != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(*c)))
		return
	}

	result, err := client.Get(ctx, vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName, data.Name.ValueString(), nil)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(setIdentityFromIDIfNull(ctx, resp.Identity, idOrFallback(data.ID, vg.Snapshot(data.Name.ValueString()).FullID()), armids.ParseVolumeGroupSnapshotID)...)
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	// A GET by name can land on a destroyed twin, which it returns as a normal 200. That
	// is gone as far as this resource is concerned: its configured name is free again,
	// so the next plan proposes to create it.
	if isSnapshotSoftDeleted(result.VolumeGroupSnapshot) {
		resp.Diagnostics.Append(setIdentityFromIDIfNull(ctx, resp.Identity, idOrFallback(data.ID, vg.Snapshot(data.Name.ValueString()).FullID()), armids.ParseVolumeGroupSnapshotID)...)
		resp.State.RemoveResource(ctx)
		return
	}

	// A state written by a move has not yet been checked for a snapshot of the volume
	// group's schedule, which ImportState refuses: the move itself could not ask the API.
	// The check is made once, and the marker is cleared with it.
	moved, d := req.Private.GetKey(ctx, movedPrivateKey)
	resp.Diagnostics.Append(d...)
	if len(moved) > 0 {
		if snapshotCreatedByPolicy(result.VolumeGroupSnapshot) {
			moveOp := operation{action: actionMove, kind: kindSnapshot, id: op.id, unnamed: true}
			resp.Diagnostics.Append(moveOp.fail(newSnapshotOwnedByScheduleError(op.id)))
			return
		}
		if resp.Private != nil {
			resp.Diagnostics.Append(resp.Private.SetKey(ctx, movedPrivateKey, nil)...)
		}
	}

	// The identity comes from the id already in state, read before the apply below
	// overwrites it with the API's spelling. The id is null in a tainted state from a
	// failed create; the API's id then becomes the state id, so the identity follows it.
	identityID := data.ID.ValueString()

	// applySnapshot never touches name, volume_group_id, source_recoverable_snapshot_id,
	// deletion_protection or eradicate_on_destroy, so starting from state (above) and
	// mapping only what it does touch preserves all of those exactly as configured.
	resp.Diagnostics.Append(applySnapshot(ctx, &data, result.VolumeGroupSnapshot)...)
	if identityID == "" {
		identityID = data.ID.ValueString()
	}
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, identityID, armids.ParseVolumeGroupSnapshotID)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// Update changes only provider-side fields (deletion_protection, eradicate_on_destroy,
// timeouts): the snapshot itself has no updatable property, and the API has no update
// operation, so it copies the plan into state and makes no API call.
func (r *SnapshotResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data SnapshotResourceModel
	if resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	var priorState SnapshotResourceModel
	if resp.Diagnostics.Append(req.State.Get(ctx, &priorState)...); resp.Diagnostics.HasError() {
		return
	}
	// An update never changes the id, so the identity is carried over from state.
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, priorState.ID.ValueString(), armids.ParseVolumeGroupSnapshotID)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *SnapshotResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data SnapshotResourceModel
	if resp.Diagnostics.Append(req.State.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	if data.DeletionProtection.ValueBool() {
		resp.Diagnostics.Append(deletionProtectedDiagnostic(kindSnapshot, newDeletionProtectedError(kindSnapshot, data.Name.ValueString())))
		return
	}

	client, ok := r.volumeGroupSnapshotsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	deleteTimeout, diags := data.Timeouts.Delete(ctx, defaultSnapshotDeleteTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()

	op := operation{action: actionDelete, kind: kindSnapshot, name: data.Name.ValueString()}
	vg, err := armids.ParseVolumeGroupID(data.VolumeGroupID.ValueString())
	if err != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(newInvalidStoredIDError(storedInState, idErrorCause(err)))))
		return
	}
	op.id = vg.Snapshot(op.name).FullID()
	if c := subscriptionMismatchCause(vg, kindVolumeGroup, r.clients.subscriptionID); c != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("volume_group_id"), op.fail(*c)))
		return
	}
	eradicateOp := op
	eradicateOp.action = actionEradicate
	listSnapshotsStep := step{action: actionList, kind: kindSnapshots, id: vg.FullID()}

	var before []*armpurestorageblock.VolumeGroupSnapshot
	if data.EradicateOnDestroy.ValueBool() {
		before, err = listSnapshots(ctx, client, vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName, "", 0)
		if err != nil {
			if isNotFound(err) {
				return
			}
			resp.Diagnostics.Append(eradicateOp.fail(newApiStepFailedError(eradicateOp, listSnapshotsStep, r.clients.apiFailure(err))))
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

	// A second DELETE on the original name finds nothing and succeeds without doing
	// anything: eradication has to address the renamed twin by its own name, which is
	// only discoverable by listing. Find the twin this delete itself just created by set
	// difference — present now, absent in the listing taken before the delete.
	after, err := listSnapshots(ctx, client, vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName, "", 0)
	if err != nil {
		// A 404 here means the volume group itself is gone, which takes its children
		// with it: a literal volume_group_id gives Terraform no ordering against the group,
		// so the group's delete can finish first.
		if isNotFound(err) {
			return
		}
		resp.Diagnostics.Append(eradicateOp.fail(newApiStepFailedError(eradicateOp, listSnapshotsStep, r.clients.apiFailure(err))))
		return
	}

	twinName, ok := snapshotTwins.eradicationTarget(before, after, data.Name.ValueString())
	if !ok {
		resp.Diagnostics.Append(softDeletedTwinNotFoundWarning(kindSnapshot, data.Name.ValueString()))
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

// listSnapshots lists a volume group's snapshots newest first, keeping only those filter
// matches. An empty filter returns every one — live and destroyed, scheduled and manual
// alike — distinguished only by each entry's own softDeletion.destroyed. It returns at
// most limit entries (a limit of zero or less means all of them). Member volume snapshots
// are not included in a list.
//
// The list response never carries a nextLink, so the SDK pager would stop after one page
// of at most $top entries (25 when $top is unset). Pages are requested explicitly instead:
// $skip is the number of entries already read, and a page shorter than $top is the last.
// The fixed order keeps $skip pointing at the same entries from one request to the next,
// as long as nothing is created or deleted in between. A snapshot created mid-listing
// shifts every later entry one place down, so the next page repeats the previous page's
// last entry: entries are de-duplicated by ID (case-insensitively, as everywhere else this
// provider compares IDs), and only unique ones count toward limit. One deleted mid-listing
// shifts entries up instead, and the entry that moves across the page boundary is missed;
// nothing in the response reveals that. A non-empty page with nothing new ends the
// listing, so a backend that ignored $skip, returning the same page every time, can't
// keep it requesting pages until the timeout.
func listSnapshots(
	ctx context.Context, client *armpurestorageblock.VolumeGroupSnapshotsClient, resourceGroupName, storagePoolName, volumeGroupName string,
	filter string, limit int64,
) ([]*armpurestorageblock.VolumeGroupSnapshot, error) {
	pageSize := int64(maxSnapshotListPageSize)
	if limit > 0 {
		pageSize = min(limit, pageSize)
	}
	options := armpurestorageblock.VolumeGroupSnapshotsClientListByVolumeGroupOptions{
		Orderby: to.Ptr(snapshotsOrderBy),
		Top:     to.Ptr(int32(pageSize)),
	}
	if filter != "" {
		options.Filter = to.Ptr(filter)
	}

	var all []*armpurestorageblock.VolumeGroupSnapshot
	seen := map[string]bool{}
	read := 0
	for {
		options.Skip = to.Ptr(int32(read))
		page, err := client.NewListByVolumeGroupPager(resourceGroupName, storagePoolName, volumeGroupName, &options).NextPage(ctx)
		if err != nil {
			return nil, err
		}
		read += len(page.Value)
		before := len(all)
		for _, s := range page.Value {
			// An entry with no ID can't be matched to another, so it is kept for the
			// caller's own handling of malformed entries.
			if s != nil && s.ID != nil {
				id := strings.ToLower(*s.ID)
				if seen[id] {
					continue
				}
				seen[id] = true
			}
			all = append(all, s)
		}
		if limit > 0 && int64(len(all)) >= limit {
			return all[:limit], nil
		}
		if int64(len(page.Value)) < pageSize || len(all) == before {
			return all, nil
		}
	}
}

// ImportState takes the snapshot's full ARM ID. A snapshot taken by the volume group's
// snapshot schedule is rejected: the schedule's retention deletes those snapshots on its
// own clock, which would leave Terraform planning to create a snapshot whose name (all
// digits) this resource cannot create.
func (r *SnapshotResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if r.clients == nil {
		resp.Diagnostics.Append(operationConfigureProvider.fail(newProviderNotConfiguredError()))
		return
	}

	importID, diags := resolveImportID(ctx, req, kindSnapshot, armids.VolumeGroupSnapshotID.FullID, armids.ParseVolumeGroupSnapshotID)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	importOp := operation{action: actionImport, kind: kindSnapshot, id: importID, unnamed: true}

	parsed, idErr := parseIDInSubscription(armids.ParseVolumeGroupSnapshotID, importID, kindSnapshot, r.clients.subscriptionID)
	if idErr != nil {
		resp.Diagnostics.Append(importOp.fail(*idErr))
		return
	}

	client, ok := r.volumeGroupSnapshotsClient(&resp.Diagnostics)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, defaultSnapshotReadTimeout)
	defer cancel()

	result, err := client.Get(ctx, parsed.ResourceGroupName, parsed.StoragePoolName, parsed.VolumeGroupName, parsed.SnapshotName, nil)
	switch {
	case err == nil:
		if snapshotCreatedByPolicy(result.VolumeGroupSnapshot) {
			resp.Diagnostics.Append(importOp.fail(newSnapshotOwnedByScheduleError(parsed.FullID())))
			return
		}
	case isNotFound(err):
		// Not rejected here: the Read that follows an import reports a missing snapshot
		// in the standard way.
	default:
		resp.Diagnostics.Append(importOp.fail(newApiStepFailedError(importOp, step{action: actionRetrieve, kind: kindSnapshot, id: parsed.FullID()}, r.clients.apiFailure(err))))
		return
	}

	resp.Diagnostics.Append(resp.Identity.Set(ctx, parsed)...)
	resp.Diagnostics.Append(seedSnapshotState(ctx, &resp.State, importID, parsed)...)
}

// MoveState takes over an azapi_resource of type PureStorage.Block/storagePools/volumeGroups/snapshots.
// The move cannot ask the API whether the volume group's snapshot schedule took the
// snapshot, which ImportState refuses, so it marks the state and Read makes that check.
func (r *SnapshotResource) MoveState(context.Context) []resource.StateMover {
	return resourceMove[armids.VolumeGroupSnapshotID]{
		kind: kindSnapshot, sources: []moveSource{azapiResourceSource}, parse: armids.ParseVolumeGroupSnapshotID, seed: seedSnapshotState, markMoved: true,
	}.stateMovers()
}
