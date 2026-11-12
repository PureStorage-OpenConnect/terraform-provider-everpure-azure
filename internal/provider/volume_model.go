// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"strings"

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

// bytesPerGiB is the multiplier provisioned_size_in_gib is defined in terms of:
// gibibytes (1024^3), not the decimal gigabytes the storage pool's and volume group's
// _in_mbps attributes use. A whole GiB is always both >= 1 MiB and a multiple of 512
// bytes, so a value built this way never needs its own bounds or rounding rule.
const bytesPerGiB = 1073741824

// source_type's accepted values are the API's VolumeSourceType enum verbatim, sent
// as-is including an explicit "none" — unlike the volume group, whose create
// validation rejects "none" if sent explicitly, an explicit "none" here is accepted, so
// there is no omit-the-field workaround to carry over: the mapping is the identity.
// These constants are defined in terms of the SDK's own VolumeSourceType* constants
// rather than repeating the literal strings, so that an upstream rename breaks this
// build instead of silently drifting from the wire.
const (
	volumeSourceTypeNone              = string(armpurestorageblock.VolumeSourceTypeNone)
	volumeSourceTypeVolume            = string(armpurestorageblock.VolumeSourceTypeVolume)
	volumeSourceTypeSerialNumber      = string(armpurestorageblock.VolumeSourceTypeSerialNumber)
	volumeSourceTypeSnapshot          = string(armpurestorageblock.VolumeSourceTypeSnapshot)
	volumeSourceTypeRecoverableVolume = string(armpurestorageblock.VolumeSourceTypeRecoverableVolume)
)

// VolumeResourceModel is the Terraform-side representation of everpure_azure_volume.
type VolumeResourceModel struct {
	ID                        armids.IDValue `tfsdk:"id"`
	Name                      types.String   `tfsdk:"name"`
	VolumeGroupID             armids.IDValue `tfsdk:"volume_group_id"`
	ProvisionedSizeInBytes    types.Int64    `tfsdk:"provisioned_size_in_bytes"`
	ProvisionedSizeInGiB      types.Int64    `tfsdk:"provisioned_size_in_gib"`
	SourceType                types.String   `tfsdk:"source_type"`
	SourceVolumeID            armids.IDValue `tfsdk:"source_volume_id"`
	SourceSerialNumber        types.String   `tfsdk:"source_serial_number"`
	SourceRecoverableVolumeID armids.IDValue `tfsdk:"source_recoverable_volume_id"`
	SourceSnapshot            types.List     `tfsdk:"source_snapshot"`
	SerialNumber              types.String   `tfsdk:"serial_number"`
	DeletionProtection        types.Bool     `tfsdk:"deletion_protection"`
	EradicateOnDestroy        types.Bool     `tfsdk:"eradicate_on_destroy"`
	Timeouts                  timeouts.Value `tfsdk:"timeouts"`
}

// VolumeSourceSnapshotModel is the "source_snapshot" nested block: a restore needs both
// the volume group snapshot's own ID and the name of the member volume within it, and
// GET never returns either (create-only, like every other source input), so this is
// always read from prior config/state, never rebuilt from an API response — unlike the
// volume group's snapshot_protection_policy, nothing here ever needs an ObjectType
// constant to construct a fresh types.List.
type VolumeSourceSnapshotModel struct {
	ID         armids.IDValue `tfsdk:"id"`
	VolumeName types.String   `tfsdk:"volume_name"`
}

// isDefinitelySet reports whether an attribute or block value is genuinely present in
// the resolved configuration, as opposed to merely "not yet proven absent" (null) or
// "not yet proven present" (unknown).
//
// A nested block needs its element count consulted, not just its null-ness: Terraform
// represents a block that appears zero times as a known, empty list rather than as a
// null one, so an absent block is emphatically not null and treating it as present
// would reject every configuration that simply omits it.
func isDefinitelySet(v attr.Value) bool {
	if v.IsNull() || v.IsUnknown() {
		return false
	}
	if list, ok := v.(types.List); ok {
		return len(list.Elements()) > 0
	}
	return true
}

// isDefinitelyAbsent is isDefinitelySet's counterpart for the "required input is
// missing" direction, and is deliberately not its negation: an unknown value is
// neither definitely set nor definitely absent, so it satisfies a required check at
// plan time and is rechecked at apply once it has resolved. As in isDefinitelySet, a
// block with no elements counts as absent whether it arrives null or empty.
func isDefinitelyAbsent(v attr.Value) bool {
	if v.IsUnknown() {
		return false
	}
	if v.IsNull() {
		return true
	}
	if list, ok := v.(types.List); ok {
		return len(list.Elements()) == 0
	}
	return false
}

// volumeSourceTypeOrDefault resolves an unset (null) source_type to "none", the
// schema's own default, so callers that build API bodies don't have to special-case an
// unresolved value that should never actually reach them once defaults have applied.
func volumeSourceTypeOrDefault(v types.String) string {
	if v.IsNull() || v.ValueString() == "" {
		return volumeSourceTypeNone
	}
	return v.ValueString()
}

// volumeSourceRow is one non-"none" source_type value's row: the attribute (or block)
// it requires present, read generically via attr.Value so the same table drives the
// matrix check for both a plain string attribute and the source_snapshot block. "none"
// has no row — it requires no source input, so the schema's OneOf validator,
// ValidateConfig's matrix and the create-time recheck all treat "names no row" as the
// generic "must not be set"/"nothing to require" case rather than needing a special
// case for it.
type volumeSourceRow struct {
	sourceType string
	attribute  path.Path
	get        func(VolumeResourceModel) attr.Value
}

var volumeSourceRows = []volumeSourceRow{
	{
		sourceType: volumeSourceTypeVolume,
		attribute:  path.Root("source_volume_id"),
		get:        func(m VolumeResourceModel) attr.Value { return m.SourceVolumeID },
	},
	{
		sourceType: volumeSourceTypeSerialNumber,
		attribute:  path.Root("source_serial_number"),
		get:        func(m VolumeResourceModel) attr.Value { return m.SourceSerialNumber },
	},
	{
		sourceType: volumeSourceTypeSnapshot,
		attribute:  path.Root("source_snapshot"),
		get:        func(m VolumeResourceModel) attr.Value { return m.SourceSnapshot },
	},
	{
		sourceType: volumeSourceTypeRecoverableVolume,
		attribute:  path.Root("source_recoverable_volume_id"),
		get:        func(m VolumeResourceModel) attr.Value { return m.SourceRecoverableVolumeID },
	},
}

// volumeSourceRowFor returns the row for sourceType. ok is false for
// sourceType == "none" and for any value the schema's OneOf validator doesn't accept,
// neither of which name a source input.
func volumeSourceRowFor(sourceType string) (volumeSourceRow, bool) {
	for _, row := range volumeSourceRows {
		if row.sourceType == sourceType {
			return row, true
		}
	}
	return volumeSourceRow{}, false
}

// volumeSourceTypeValues lists every value the source_type schema attribute's OneOf
// validator accepts, "none" first.
func volumeSourceTypeValues() []string {
	values := []string{volumeSourceTypeNone}
	for _, row := range volumeSourceRows {
		values = append(values, row.sourceType)
	}
	return values
}

// missingVolumeSourceAttribute checks whether sourceType names a source input (via
// volumeSourceRowFor) that is absent from data, returning that input's path, which both
// ValidateConfig and Create's own apply-time recheck report.
// Only a definitely-absent value is "missing" (see isDefinitelyAbsent): an unknown one
// (a reference to another resource's value, not yet known at plan time) still satisfies
// the check, since Create's own recheck runs again once the value has resolved.
// sourceType not naming a row at all (either "none",
// or an otherwise-invalid value the OneOf validator already reports) is not "missing"
// either — it returns missing = false.
func missingVolumeSourceAttribute(sourceType string, data VolumeResourceModel) (wantPath path.Path, missing bool) {
	row, ok := volumeSourceRowFor(sourceType)
	if !ok || !isDefinitelyAbsent(row.get(data)) {
		return path.Path{}, false
	}
	return row.attribute, true
}

// resolveVolumeSizeInBytes returns the byte size a given configuration resolves to,
// and is the single definition of how the two size attributes combine.
//
// A known, non-null provisioned_size_in_gib wins, multiplied out. A null one leaves
// provisioned_size_in_bytes exactly as it stands: that value is whatever Core carried
// forward from state for an Optional+Computed attribute the config doesn't mention, and
// overwriting it would read the null gib as 0 and shrink the volume to nothing purely
// because a config line was deleted. An unknown gib resolves to an unknown size rather
// than to the prior state's — the derived value genuinely isn't known yet, and pinning
// it to the old size would let a plan claim "no change" and then skip a resize the
// apply-time value actually called for.
//
// Plan time and apply time both call this, so the value a plan displays and the value
// that reaches the API cannot diverge: at apply the gib operand is always resolved, so
// a size left unknown in the plan becomes concrete here.
func resolveVolumeSizeInBytes(data VolumeResourceModel) types.Int64 {
	switch {
	case data.ProvisionedSizeInGiB.IsUnknown():
		return types.Int64Unknown()
	case data.ProvisionedSizeInGiB.IsNull():
		return data.ProvisionedSizeInBytes
	default:
		return types.Int64Value(data.ProvisionedSizeInGiB.ValueInt64() * bytesPerGiB)
	}
}

// checkVolumeSizePresence enforces the create-only size-presence rule: exactly one of
// provisioned_size_in_gib/provisioned_size_in_bytes is required when source_type is
// "none", and neither may be set for any other source type, since every other source
// type inherits its size from wherever it's cloned or restored from. It is a no-op
// whenever source_type or provisioned_size_in_gib is still unknown (e.g. a value that
// names another resource not yet created in this plan): ModifyPlan then defers to
// Create's identical recheck, once apply has resolved every value to something
// concrete. There is no equivalent check on update: provisioned_size_in_bytes is
// Optional+Computed and carries forward from state whenever config leaves both size
// attributes null, so either one can be dropped or reintroduced later with no side
// effect — see toVolumeUpdate.
func checkVolumeSizePresence(data VolumeResourceModel) diag.Diagnostics {
	var diags diag.Diagnostics
	if data.SourceType.IsUnknown() || data.ProvisionedSizeInGiB.IsUnknown() {
		return diags
	}

	sourceType := volumeSourceTypeOrDefault(data.SourceType)
	gibSet := isDefinitelySet(data.ProvisionedSizeInGiB)
	bytesSet := isDefinitelySet(data.ProvisionedSizeInBytes)

	switch {
	case sourceType == volumeSourceTypeNone && !gibSet && !bytesSet:
		diags.Append(diag.WithPath(path.Root("provisioned_size_in_gib"), invalidAttributeCombinationDiagnostic(newVolumeSizeRequiredError())))
	case sourceType != volumeSourceTypeNone && (gibSet || bytesSet):
		sizePath := path.Root("provisioned_size_in_bytes")
		if gibSet {
			sizePath = path.Root("provisioned_size_in_gib")
		}
		diags.Append(diag.WithPath(sizePath, invalidAttributeCombinationDiagnostic(newVolumeSizeNotAllowedError(sourceType))))
	}
	return diags
}

// checkVolumeSourcePool reports a diagnostic unless sourceID sits under the same
// storage pool as volumeGroupID. The backend itself rejects a cross-pool clone
// synchronously ("cloning volumes across storage pools is not allowed"), a restriction
// that exists only in that error message and nowhere in the API's published
// documentation; this catches the same mistake locally, with no round trip. sourceType
// states which ARM resource type sourceID actually is (a volume for source_volume_id, a
// volume group snapshot for source_snapshot.id) and sourcePath says which attribute
// holds it — the caller always knows both. Every diagnostic about the source is
// addressed to sourcePath and names it, so the two attributes checked this way stay
// distinguishable; a malformed volume_group_id is reported against volume_group_id.
//
// A malformed sourceID is reported here rather than left to the attribute's own schema
// validator, which sees less than it looks like it does: it runs against the config
// only, and skips any value still unknown there, so an ID that resolves from another
// resource's output reaches this check never having been validated at all. Nor is the
// report a duplicate when the validator does fire — a validation error stops the run
// before plan and apply, so the two paths are mutually exclusive.
func checkVolumeSourcePool(volumeGroupID string, sourceType *armids.ResourceType, sourceID string, sourcePath path.Path) diag.Diagnostics {
	var diags diag.Diagnostics

	wantPool, err := armids.TypeVolumeGroup.AncestorIDOf(volumeGroupID, &armids.TypeStoragePool)
	if err != nil {
		diags.Append(diag.WithPath(path.Root("volume_group_id"), invalidAttributeValueDiagnostic(idErrorCause(err))))
		return diags
	}
	gotPool, err := sourceType.AncestorIDOf(sourceID, &armids.TypeStoragePool)
	if err != nil {
		diags.Append(diag.WithPath(sourcePath, invalidAttributeValueDiagnostic(idErrorCause(err))))
		return diags
	}
	if !strings.EqualFold(wantPool, gotPool) {
		diags.Append(diag.WithPath(sourcePath, invalidAttributeCombinationDiagnostic(newSourceInOtherStoragePoolError(sourcePath, gotPool, wantPool))))
	}
	return diags
}

// checkVolumeSourceGroup reports a diagnostic unless sourceRecoverableVolumeID sits
// directly under volumeGroupID. The backend's recovery documents its own field as
// recovering a volume "within the same volume group", but a bug (confirmed against a
// real deployment) parses only the source's volume group out of the recoverable
// volume's ID and never validates it against the URL's own volume group, so naming a
// recoverable volume in a different group silently recreates the volume in its
// original group instead of the one requested. This check enforces the documented
// contract locally rather than relying on a fix. It reports a malformed ID itself for
// the same reason checkVolumeSourcePool does, and here it is the only thing that will:
// source_recoverable_volume_id is read nowhere else on the create path.
func checkVolumeSourceGroup(volumeGroupID, sourceRecoverableVolumeID string) diag.Diagnostics {
	var diags diag.Diagnostics
	sourcePath := path.Root("source_recoverable_volume_id")

	gotGroup, err := armids.TypeVolume.ParentIDOf(sourceRecoverableVolumeID)
	if err != nil {
		diags.Append(diag.WithPath(sourcePath, invalidAttributeValueDiagnostic(idErrorCause(err))))
		return diags
	}
	if !strings.EqualFold(gotGroup, volumeGroupID) {
		diags.Append(diag.WithPath(sourcePath, invalidAttributeCombinationDiagnostic(newRecoverySourceInOtherVolumeGroupError(sourcePath, kindVolume, gotGroup, volumeGroupID))))
	}
	return diags
}

// checkVolumeSourceRules enforces the two provider-side ID guards that need no API
// call: every source ID must sit under the same storage pool as volume_group_id, and
// source_recoverable_volume_id must additionally sit under volume_group_id itself. A
// malformed ID is reported too, since reading one is the first thing either guard does.
// It is a no-op wherever the relevant IDs aren't yet known — e.g. a source referencing
// a resource still being created in the same plan — so ModifyPlan can call it
// best-effort and Create calls the same function once every value has resolved.
func checkVolumeSourceRules(ctx context.Context, sourceType string, data VolumeResourceModel) diag.Diagnostics {
	var diags diag.Diagnostics
	if !isDefinitelySet(data.VolumeGroupID) {
		return diags
	}
	volumeGroupID := data.VolumeGroupID.ValueString()

	switch sourceType {
	case volumeSourceTypeVolume:
		if !isDefinitelySet(data.SourceVolumeID) {
			return diags
		}
		diags.Append(checkVolumeSourcePool(volumeGroupID, &armids.TypeVolume,
			data.SourceVolumeID.ValueString(), path.Root("source_volume_id"))...)
	case volumeSourceTypeSnapshot:
		if data.SourceSnapshot.IsUnknown() {
			return diags
		}
		snapshots, d := blockElements[VolumeSourceSnapshotModel](ctx, data.SourceSnapshot)
		diags.Append(d...)
		if len(snapshots) != 1 || !isDefinitelySet(snapshots[0].ID) {
			return diags
		}
		diags.Append(checkVolumeSourcePool(volumeGroupID, &armids.TypeVolumeGroupSnapshot,
			snapshots[0].ID.ValueString(), path.Root("source_snapshot").AtName("id"))...)
	case volumeSourceTypeRecoverableVolume:
		if !isDefinitelySet(data.SourceRecoverableVolumeID) {
			return diags
		}
		diags.Append(checkVolumeSourceGroup(volumeGroupID, data.SourceRecoverableVolumeID.ValueString())...)
	}
	return diags
}

// deriveSourceVolumeGroupID derives the sourceVolumeGroupResourceId the create body
// needs for a "volume" or "snapshot" source: the source's own parent, cut from its ID
// rather than parsed and rebuilt so the result keeps whatever casing the source ID had.
// It returns null for every other source type (none, serialNumber, recoverableVolume),
// which have no source volume group of their own.
func deriveSourceVolumeGroupID(ctx context.Context, sourceType string, data VolumeResourceModel) (types.String, diag.Diagnostics) {
	var diags diag.Diagnostics

	switch sourceType {
	case volumeSourceTypeVolume:
		parent, err := armids.TypeVolume.ParentIDOf(data.SourceVolumeID.ValueString())
		if err != nil {
			diags.Append(diag.WithPath(path.Root("source_volume_id"), invalidAttributeValueDiagnostic(idErrorCause(err))))
			return types.StringNull(), diags
		}
		return types.StringValue(parent), diags

	case volumeSourceTypeSnapshot:
		snapshots, d := blockElements[VolumeSourceSnapshotModel](ctx, data.SourceSnapshot)
		diags.Append(d...)
		if len(snapshots) != 1 {
			diags.Append(diag.WithPath(path.Root("source_snapshot"),
				invalidAttributeCombinationDiagnostic(newSourceAttributeRequiredError(path.Root("source_snapshot"), volumeSourceTypeSnapshot))))
			return types.StringNull(), diags
		}
		parent, err := armids.TypeVolumeGroupSnapshot.ParentIDOf(snapshots[0].ID.ValueString())
		if err != nil {
			diags.Append(diag.WithPath(path.Root("source_snapshot").AtName("id"), invalidAttributeValueDiagnostic(idErrorCause(err))))
			return types.StringNull(), diags
		}
		return types.StringValue(parent), diags

	default:
		return types.StringNull(), diags
	}
}

// toVolume builds the PUT body for Create from the planned configuration. For a
// "volume" or "snapshot" source it calls deriveSourceVolumeGroupID itself to fill
// sourceVolumeGroupResourceId, rather than reading it off data: the API needs that value
// only in this create body and never returns it, so nothing durable holds it.
func toVolume(ctx context.Context, data VolumeResourceModel) (armpurestorageblock.Volume, diag.Diagnostics) {
	var diags diag.Diagnostics

	sourceType := volumeSourceTypeOrDefault(data.SourceType)
	props := &armpurestorageblock.AzureVolumeProperties{
		SourceType: to.Ptr(armpurestorageblock.VolumeSourceType(sourceType)),
	}

	switch sourceType {
	case volumeSourceTypeNone:
		props.ProvisionedSize = to.Ptr(data.ProvisionedSizeInBytes.ValueInt64())
	case volumeSourceTypeVolume:
		props.SourceVolumeResourceID = to.Ptr(data.SourceVolumeID.ValueString())
		sourceVolumeGroupID, d := deriveSourceVolumeGroupID(ctx, sourceType, data)
		diags.Append(d...)
		props.SourceVolumeGroupResourceID = to.Ptr(sourceVolumeGroupID.ValueString())
	case volumeSourceTypeSerialNumber:
		props.SourceSerialNumber = to.Ptr(data.SourceSerialNumber.ValueString())
	case volumeSourceTypeSnapshot:
		snapshots, d := blockElements[VolumeSourceSnapshotModel](ctx, data.SourceSnapshot)
		diags.Append(d...)
		if len(snapshots) == 1 {
			props.SourceVolumeSnapshot = &armpurestorageblock.VolumeSnapshotSource{
				VolumeGroupSnapshotResourceID: to.Ptr(snapshots[0].ID.ValueString()),
				VolumeSnapshotName:            to.Ptr(snapshots[0].VolumeName.ValueString()),
			}
		}
		sourceVolumeGroupID, d := deriveSourceVolumeGroupID(ctx, sourceType, data)
		diags.Append(d...)
		props.SourceVolumeGroupResourceID = to.Ptr(sourceVolumeGroupID.ValueString())
	case volumeSourceTypeRecoverableVolume:
		props.SourceRecoverableVolumeResourceID = to.Ptr(data.SourceRecoverableVolumeID.ValueString())
	}

	return armpurestorageblock.Volume{Properties: props}, diags
}

// toVolumeUpdate builds the PATCH body for Update. VolumeUpdate carries only
// provisionedSize and nothing else — every other attribute means replace — and Update
// only ever calls this once it has already established the resolved size changed from
// state, so this always sends the plan's current resolved value, whichever of the two
// size attributes produced it.
func toVolumeUpdate(data VolumeResourceModel) armpurestorageblock.VolumeUpdate {
	return armpurestorageblock.VolumeUpdate{
		Properties: &armpurestorageblock.VolumeUpdateProperties{
			ProvisionedSize: to.Ptr(data.ProvisionedSizeInBytes.ValueInt64()),
		},
	}
}

// applyVolume maps an API response onto data, in place. It deliberately touches only
// id, provisioned_size_in_bytes and serial_number: GET returns none of the source*
// fields (create-only by the swagger's own mutability) and volume_group_id,
// provisioned_size_in_gib, deletion_protection and eradicate_on_destroy have no API
// representation at all, so leaving data's existing fields alone is what preserves them
// across Create/Read/Update.
//
// name is not mapped either, though the response carries it. It is a Required
// attribute, so the state saved after Create or Update must equal the configured value
// exactly; ARM treats resource names case-insensitively and may echo back a casing the
// practitioner never wrote, which would fail the apply with "Provider produced
// inconsistent result after apply". Nothing is lost by ignoring it: the name is always
// already on data by the time this runs — from the plan on Create and Update, from
// state on Read, and from the parsed ARM ID on import — and a volume cannot be renamed
// in place, so the response can never carry a name the caller didn't ask for.
func applyVolume(data *VolumeResourceModel, v armpurestorageblock.Volume) diag.Diagnostics {
	var diags diag.Diagnostics

	if v.Properties == nil {
		diags.Append(unexpectedResponseDiagnostic(newNoPropertiesError(kindVolume)))
		return diags
	}
	props := v.Properties

	data.ID = armids.IDValue{StringValue: types.StringPointerValue(v.ID)}
	data.ProvisionedSizeInBytes = types.Int64PointerValue(props.ProvisionedSize)
	data.SerialNumber = types.StringPointerValue(props.SerialNumber)

	return diags
}

// seedImportedVolume sets the attributes ImportState writes before the first Read: what
// can be derived from the ID alone, plus the provider-side defaults. The defaults must
// be explicit rather than left null: left null, the next plan would fill source_type in
// from the schema default, and it being RequiresReplace would then show a spurious
// replace. An imported volume declares source_type = "none" whatever it was actually
// created from, since the API cannot report a volume's origin after the fact.
func seedImportedVolume(data *VolumeResourceModel, importID string, parsed armids.VolumeID) {
	data.ID = armids.IDValue{StringValue: types.StringValue(importID)}
	data.Name = types.StringValue(parsed.VolumeName)
	// Composed rather than cut from the import ID's own text, which normalises the
	// segment keywords and namespace to their canonical spelling.
	data.VolumeGroupID = armids.IDValue{StringValue: types.StringValue(parsed.VolumeGroup().FullID())}
	data.SourceType = types.StringValue(volumeSourceTypeNone)
	data.DeletionProtection = types.BoolValue(true)
	data.EradicateOnDestroy = types.BoolValue(false)
}

// seedVolumeState is what ImportState and a move both write to a new state: the
// seedImportedVolume attributes, and nothing else, so that state before the first Read
// has the same shape whichever way the volume came under management.
func seedVolumeState(ctx context.Context, state *tfsdk.State, id string, parsed armids.VolumeID) diag.Diagnostics {
	var seed VolumeResourceModel
	seedImportedVolume(&seed, id, parsed)
	var diags diag.Diagnostics
	diags.Append(state.SetAttribute(ctx, path.Root("id"), seed.ID)...)
	diags.Append(state.SetAttribute(ctx, path.Root("name"), seed.Name)...)
	diags.Append(state.SetAttribute(ctx, path.Root("volume_group_id"), seed.VolumeGroupID)...)
	diags.Append(state.SetAttribute(ctx, path.Root("source_type"), seed.SourceType)...)
	diags.Append(state.SetAttribute(ctx, path.Root("deletion_protection"), seed.DeletionProtection)...)
	diags.Append(state.SetAttribute(ctx, path.Root("eradicate_on_destroy"), seed.EradicateOnDestroy)...)
	return diags
}

// importedVolumeFromAPI builds the resource object a fresh import of v followed by Read
// would leave in state. blank must be a model whose every attribute is a typed null, the
// shape state has before ImportState runs. The volume's own ID (as the API spells it)
// plays the part of the import ID.
func importedVolumeFromAPI(
	blank VolumeResourceModel, v armpurestorageblock.Volume,
) (VolumeResourceModel, armids.VolumeID, diag.Diagnostics) {
	var diags diag.Diagnostics
	data := blank
	if v.ID == nil {
		diags.Append(unexpectedResponseDiagnostic(newMissingResponseIDError(kindVolume)))
		return data, armids.VolumeID{}, diags
	}
	parsed, err := armids.ParseVolumeID(*v.ID)
	if err != nil {
		diags.Append(unexpectedResponseDiagnostic(newUnparseableResponseIDError(kindVolume, idErrorCause(err))))
		return data, armids.VolumeID{}, diags
	}
	seedImportedVolume(&data, *v.ID, parsed)
	diags.Append(applyVolume(&data, v)...)
	return data, parsed, diags
}

// volumeDataSourceItem is the read-side attribute set shared by the singular
// everpure_azure_volume data source and each element of the plural everpure_azure_volumes
// data source's volumes list (there, embedded in volumeListItem alongside
// soft_deletion). VolumeGroupID is left unset here: both callers set it themselves, to
// the same value either way — the data source's own configured volume_group_id,
// verbatim, since a full ARM ID already compares case-insensitively via armids.IDValue.
type volumeDataSourceItem struct {
	ID                     armids.IDValue `tfsdk:"id"`
	Name                   types.String   `tfsdk:"name"`
	VolumeGroupID          armids.IDValue `tfsdk:"volume_group_id"`
	ProvisionedSizeInBytes types.Int64    `tfsdk:"provisioned_size_in_bytes"`
	SerialNumber           types.String   `tfsdk:"serial_number"`
	Space                  types.Object   `tfsdk:"space"`
}

// volumeItemFrom maps an API response onto a fresh volumeDataSourceItem, by routing it
// through the resource's own applyVolume and copying the result across. Name is filled
// directly from v.Name rather than through applyVolume, which deliberately never maps
// it (see applyVolume's own doc comment): the singular data source overwrites it with
// its own configured value afterward, exactly as before this extraction, while the
// plural data source has no configured name of its own and keeps whatever the API
// returned.
func volumeItemFrom(v armpurestorageblock.Volume) (volumeDataSourceItem, diag.Diagnostics) {
	var scratch VolumeResourceModel
	diags := applyVolume(&scratch, v)
	if diags.HasError() {
		return volumeDataSourceItem{}, diags
	}

	// applyVolume has already rejected a response without properties.
	spaceObj, spaceDiags := spaceFromAPI(v.Properties.Space)
	diags.Append(spaceDiags...)

	return volumeDataSourceItem{
		ID:                     scratch.ID,
		Name:                   types.StringPointerValue(v.Name),
		ProvisionedSizeInBytes: scratch.ProvisionedSizeInBytes,
		SerialNumber:           scratch.SerialNumber,
		Space:                  spaceObj,
	}, diags
}
