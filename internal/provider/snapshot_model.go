// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

// SnapshotResourceModel is the Terraform-side representation of everpure_azure_snapshot.
// VolumeSnapshots is a types.List, never a Go slice: it is Computed, so it is unknown
// in every plan that creates the snapshot.
type SnapshotResourceModel struct {
	ID                          armids.IDValue `tfsdk:"id"`
	Name                        types.String   `tfsdk:"name"`
	VolumeGroupID               armids.IDValue `tfsdk:"volume_group_id"`
	SourceRecoverableSnapshotID armids.IDValue `tfsdk:"source_recoverable_snapshot_id"`
	CreatedAt                   types.String   `tfsdk:"created_at"`
	VolumeSnapshots             types.List     `tfsdk:"volume_snapshots"`
	DeletionProtection          types.Bool     `tfsdk:"deletion_protection"`
	EradicateOnDestroy          types.Bool     `tfsdk:"eradicate_on_destroy"`
	Timeouts                    timeouts.Value `tfsdk:"timeouts"`
}

// Descriptions of the "volume_snapshots" attribute and its elements. The resource and
// data source schemas use different schema packages, so the attribute itself cannot be
// shared; both use these strings so their generated docs cannot drift.
const (
	volumeSnapshotsDescription = "The snapshot of each volume in the volume group at the time the snapshot " +
		"was taken."
	volumeSnapshotVolumeNameDescription = "Name of the source volume when the snapshot was taken. This is " +
		"the value for `source_snapshot.volume_name` on an `everpure_azure_volume` " +
		"restoring from this snapshot."
	volumeSnapshotSizeDescription   = "Provisioned size of the source volume, in bytes."
	volumeSnapshotSerialDescription = "Serial number of the source volume, as recorded when the snapshot was taken."
)

// volumeSnapshotObjectType mirrors the "volume_snapshots" attribute's element schema, so
// types.ObjectValue and types.ListValueFrom can build it directly.
var volumeSnapshotObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"volume_name":               types.StringType,
	"provisioned_size_in_bytes": types.Int64Type,
	"serial_number":             types.StringType,
}}

// volumeSnapshotModel is one element of "volume_snapshots".
type volumeSnapshotModel struct {
	VolumeName             types.String `tfsdk:"volume_name"`
	ProvisionedSizeInBytes types.Int64  `tfsdk:"provisioned_size_in_bytes"`
	SerialNumber           types.String `tfsdk:"serial_number"`
}

// volumeSnapshotDataSourceObjectType is the element schema of the data source's
// "volume_snapshots": the resource's element plus "space". Only the data source has it,
// because space changes as data is written and deduplicated, and a resource attribute
// that changes on its own shows as drift on every refresh.
var volumeSnapshotDataSourceObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"volume_name":               types.StringType,
	"provisioned_size_in_bytes": types.Int64Type,
	"serial_number":             types.StringType,
	"space":                     spaceObjectType,
}}

// volumeSnapshotFromAPI maps one member's name, provisioned size and serial number; the
// resource and the data source both build their elements from it.
func volumeSnapshotFromAPI(info *armpurestorageblock.VolumeSnapshotInfo) volumeSnapshotModel {
	return volumeSnapshotModel{
		VolumeName:             types.StringPointerValue(info.Name),
		ProvisionedSizeInBytes: types.Int64PointerValue(info.ProvisionedSize),
		SerialNumber:           types.StringPointerValue(info.SerialNumber),
	}
}

// volumeSnapshotDataSourceModel is one element of the data source's "volume_snapshots".
type volumeSnapshotDataSourceModel struct {
	volumeSnapshotModel
	Space types.Object `tfsdk:"space"`
}

// volumeSnapshotsDataSourceFromAPI is volumeSnapshotsFromAPI for the data source: the
// same members, each with its space.
func volumeSnapshotsDataSourceFromAPI(ctx context.Context, infos []*armpurestorageblock.VolumeSnapshotInfo) (types.List, diag.Diagnostics) {
	var diags diag.Diagnostics
	items := make([]volumeSnapshotDataSourceModel, 0, len(infos))
	for _, info := range infos {
		if info == nil {
			continue
		}
		space, d := spaceFromAPI(info.Space)
		diags.Append(d...)
		items = append(items, volumeSnapshotDataSourceModel{
			volumeSnapshotModel: volumeSnapshotFromAPI(info),
			Space:               space,
		})
	}
	if diags.HasError() {
		return types.ListNull(volumeSnapshotDataSourceObjectType), diags
	}
	list, d := types.ListValueFrom(ctx, volumeSnapshotDataSourceObjectType, items)
	diags.Append(d...)
	return list, diags
}

// volumeSnapshotsFromAPI maps the member volume snapshots of a single-GET response.
// A nil slice maps to an empty, known list rather than a null or unknown one: the
// attribute is Computed and always describes the snapshot's members, and an omitted
// array means "none reported", not "not yet known". A nil entry is skipped.
func volumeSnapshotsFromAPI(ctx context.Context, infos []*armpurestorageblock.VolumeSnapshotInfo) (types.List, diag.Diagnostics) {
	items := make([]volumeSnapshotModel, 0, len(infos))
	for _, info := range infos {
		if info == nil {
			continue
		}
		items = append(items, volumeSnapshotFromAPI(info))
	}
	return types.ListValueFrom(ctx, volumeSnapshotObjectType, items)
}

// snapshotCreatedAt renders createdAt as an RFC 3339 timestamp in UTC, so that string
// order is chronological order whatever offset the API used.
func snapshotCreatedAt(createdAt *time.Time) types.String {
	if createdAt == nil {
		return types.StringNull()
	}
	return types.StringValue(createdAt.UTC().Format(time.RFC3339))
}

// toSnapshot builds the PUT body for Create. Only a recovery carries a property at all:
// sourceSnapshotResourceId names the soft-deleted snapshot to un-destroy under the
// requested name, and a plain create sends an empty body.
func toSnapshot(data SnapshotResourceModel) armpurestorageblock.VolumeGroupSnapshot {
	snapshot := armpurestorageblock.VolumeGroupSnapshot{}
	if isDefinitelySet(data.SourceRecoverableSnapshotID) {
		snapshot.Properties = &armpurestorageblock.VolumeGroupSnapshotProperties{
			SourceSnapshotResourceID: to.Ptr(data.SourceRecoverableSnapshotID.ValueString()),
		}
	}
	return snapshot
}

// applySnapshot maps an API response onto data, in place. It touches only id,
// created_at and volume_snapshots: name and volume_group_id are Required and must equal
// the configured value exactly (ARM may echo a different casing), and
// source_recoverable_snapshot_id, deletion_protection and eradicate_on_destroy have no
// API representation, so leaving them alone preserves them across Create/Read.
func applySnapshot(ctx context.Context, data *SnapshotResourceModel, s armpurestorageblock.VolumeGroupSnapshot) diag.Diagnostics {
	var diags diag.Diagnostics

	if s.Properties == nil {
		diags.Append(unexpectedResponseDiagnostic(newNoPropertiesError(kindSnapshot)))
		return diags
	}
	props := s.Properties

	volumeSnapshots, d := volumeSnapshotsFromAPI(ctx, props.VolumeSnapshots)
	diags.Append(d...)
	if diags.HasError() {
		return diags
	}

	data.ID = armids.IDValue{StringValue: types.StringPointerValue(s.ID)}
	data.CreatedAt = snapshotCreatedAt(props.CreatedAt)
	data.VolumeSnapshots = volumeSnapshots
	return diags
}

// isSnapshotSoftDeleted reports whether s is itself a soft-deleted (destroyed, not yet
// eradicated) copy rather than a live snapshot. GET returns one when the requested name
// is a destroyed twin's own timestamped name.
func isSnapshotSoftDeleted(s armpurestorageblock.VolumeGroupSnapshot) bool {
	return s.Properties != nil && isDestroyed(s.Properties.SoftDeletion)
}

// snapshotCreatedByPolicy reads createdByPolicy, treating an absent value as false.
func snapshotCreatedByPolicy(s armpurestorageblock.VolumeGroupSnapshot) bool {
	return s.Properties != nil && s.Properties.CreatedByPolicy != nil && *s.Properties.CreatedByPolicy
}

// checkSnapshotRecoverySource reports a diagnostic unless sourceSnapshotID sits directly
// under volumeGroupID. A recovery un-destroys a snapshot of the same volume group and
// renames it, so a source in another volume group cannot work. It reports a malformed ID
// itself: an ID that resolves from another resource's output reaches this check never
// having been through the attribute's own schema validator. It is a no-op wherever
// either ID isn't yet known.
func checkSnapshotRecoverySource(volumeGroupID, sourceSnapshotID armids.IDValue) diag.Diagnostics {
	var diags diag.Diagnostics
	if !isDefinitelySet(volumeGroupID) || !isDefinitelySet(sourceSnapshotID) {
		return diags
	}
	sourcePath := path.Root("source_recoverable_snapshot_id")

	sourceGroup, err := armids.TypeVolumeGroupSnapshot.ParentIDOf(sourceSnapshotID.ValueString())
	if err != nil {
		diags.Append(diag.WithPath(sourcePath, invalidAttributeValueDiagnostic(idErrorCause(err))))
		return diags
	}
	if !strings.EqualFold(sourceGroup, volumeGroupID.ValueString()) {
		diags.Append(diag.WithPath(sourcePath, invalidAttributeCombinationDiagnostic(
			newRecoverySourceInOtherVolumeGroupError(sourcePath, kindSnapshot, sourceGroup, volumeGroupID.ValueString()))))
	}
	return diags
}

// snapshotDataSourceItem is the read-side attribute set shared by the singular
// everpure_azure_snapshot data source and, minus volume_snapshots, each element of the
// plural everpure_azure_snapshots data source.
type snapshotDataSourceItem struct {
	ID              armids.IDValue `tfsdk:"id"`
	Name            types.String   `tfsdk:"name"`
	VolumeGroupID   armids.IDValue `tfsdk:"volume_group_id"`
	CreatedAt       types.String   `tfsdk:"created_at"`
	CreatedByPolicy types.Bool     `tfsdk:"created_by_policy"`
	Space           types.Object   `tfsdk:"space"`
}

// snapshotItemFrom maps an API response onto a fresh snapshotDataSourceItem. VolumeGroupID
// is left unset: both callers set it themselves, to their own configured volume_group_id.
// Name comes from the response, which the singular data source overwrites with its
// configured value afterwards.
func snapshotItemFrom(s armpurestorageblock.VolumeGroupSnapshot) (snapshotDataSourceItem, diag.Diagnostics) {
	var diags diag.Diagnostics
	if s.Properties == nil {
		diags.Append(unexpectedResponseDiagnostic(newNoPropertiesError(kindSnapshot)))
		return snapshotDataSourceItem{}, diags
	}

	spaceObj, spaceDiags := spaceFromAPI(s.Properties.Space)
	diags.Append(spaceDiags...)

	return snapshotDataSourceItem{
		ID:              armids.IDValue{StringValue: types.StringPointerValue(s.ID)},
		Name:            types.StringPointerValue(s.Name),
		CreatedAt:       snapshotCreatedAt(s.Properties.CreatedAt),
		CreatedByPolicy: types.BoolValue(snapshotCreatedByPolicy(s)),
		Space:           spaceObj,
	}, diags
}

// seedImportedSnapshot sets the attributes ImportState writes before the first Read:
// what can be derived from the ID alone, plus the provider-side defaults. The defaults
// must be explicit rather than left null, so the first plan after an import shows no
// change to them. source_recoverable_snapshot_id stays null: it only ever describes a
// recovery at create time.
func seedImportedSnapshot(data *SnapshotResourceModel, importID string, parsed armids.VolumeGroupSnapshotID) {
	data.ID = armids.IDValue{StringValue: types.StringValue(importID)}
	data.Name = types.StringValue(parsed.SnapshotName)
	// Composed rather than cut from the import ID's own text, which normalises the
	// segment keywords and namespace to their canonical spelling.
	data.VolumeGroupID = armids.IDValue{StringValue: types.StringValue(parsed.VolumeGroup().FullID())}
	data.DeletionProtection = types.BoolValue(true)
	data.EradicateOnDestroy = types.BoolValue(false)
}

// seedSnapshotState is what ImportState and a move both write to a new state: the
// seedImportedSnapshot attributes, and nothing else, so that state before the first Read
// has the same shape whichever way the snapshot came under management.
func seedSnapshotState(ctx context.Context, state *tfsdk.State, id string, parsed armids.VolumeGroupSnapshotID) diag.Diagnostics {
	var seed SnapshotResourceModel
	seedImportedSnapshot(&seed, id, parsed)
	var diags diag.Diagnostics
	diags.Append(state.SetAttribute(ctx, path.Root("id"), seed.ID)...)
	diags.Append(state.SetAttribute(ctx, path.Root("name"), seed.Name)...)
	diags.Append(state.SetAttribute(ctx, path.Root("volume_group_id"), seed.VolumeGroupID)...)
	diags.Append(state.SetAttribute(ctx, path.Root("deletion_protection"), seed.DeletionProtection)...)
	diags.Append(state.SetAttribute(ctx, path.Root("eradicate_on_destroy"), seed.EradicateOnDestroy)...)
	return diags
}

// importedSnapshotFromAPI builds the resource object a fresh import of s followed by
// Read would leave in state. blank must be a model whose every attribute is a typed
// null, the shape state has before ImportState runs. The snapshot's own ID (as the API
// spells it) plays the part of the import ID. volume_snapshots reflects whatever s
// carries: only a single-GET response has member volumes, so a list element yields an
// empty list.
func importedSnapshotFromAPI(
	ctx context.Context, blank SnapshotResourceModel, s armpurestorageblock.VolumeGroupSnapshot,
) (SnapshotResourceModel, armids.VolumeGroupSnapshotID, diag.Diagnostics) {
	var diags diag.Diagnostics
	data := blank
	if s.ID == nil {
		diags.Append(unexpectedResponseDiagnostic(newMissingResponseIDError(kindSnapshot)))
		return data, armids.VolumeGroupSnapshotID{}, diags
	}
	parsed, err := armids.ParseVolumeGroupSnapshotID(*s.ID)
	if err != nil {
		diags.Append(unexpectedResponseDiagnostic(newUnparseableResponseIDError(kindSnapshot, idErrorCause(err))))
		return data, armids.VolumeGroupSnapshotID{}, diags
	}
	seedImportedSnapshot(&data, *s.ID, parsed)
	diags.Append(applySnapshot(ctx, &data, s)...)
	return data, parsed, diags
}
