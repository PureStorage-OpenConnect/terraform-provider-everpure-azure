// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"

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

// source_type's accepted values are the API's VolumeGroupSourceType enum verbatim
// (azurerm convention: attribute values mirror the API's own enum strings exactly,
// case-sensitive), so the TF <-> API mapping is the identity — toVolumeGroup below
// sends data.SourceType straight through. These constants are defined in terms of the
// SDK's own VolumeGroupSourceType* constants rather than repeating the literal
// strings, so that an upstream rename breaks this build instead of silently drifting
// from the wire.
const (
	volumeGroupSourceTypeNone                   = string(armpurestorageblock.VolumeGroupSourceTypeNone)
	volumeGroupSourceTypeVolumeGroup            = string(armpurestorageblock.VolumeGroupSourceTypeVolumeGroup)
	volumeGroupSourceTypeSnapshot               = string(armpurestorageblock.VolumeGroupSourceTypeSnapshot)
	volumeGroupSourceTypeRecoverableVolumeGroup = string(armpurestorageblock.VolumeGroupSourceTypeRecoverableVolumeGroup)
)

// VolumeGroupResourceModel is the Terraform-side representation of
// everpure_azure_volume_group.
type VolumeGroupResourceModel struct {
	ID                             armids.IDValue `tfsdk:"id"`
	Name                           types.String   `tfsdk:"name"`
	StoragePoolID                  armids.IDValue `tfsdk:"storage_pool_id"`
	SourceType                     types.String   `tfsdk:"source_type"`
	SourceVolumeGroupID            armids.IDValue `tfsdk:"source_volume_group_id"`
	SourceSnapshotID               armids.IDValue `tfsdk:"source_snapshot_id"`
	SourceRecoverableVolumeGroupID armids.IDValue `tfsdk:"source_recoverable_volume_group_id"`
	BandwidthLimitInMbps           types.Int64    `tfsdk:"bandwidth_limit_in_mbps"`
	IopsLimit                      types.Int64    `tfsdk:"iops_limit"`
	Tags                           types.Map      `tfsdk:"tags"`
	DeletionProtection             types.Bool     `tfsdk:"deletion_protection"`
	EradicateOnDestroy             types.Bool     `tfsdk:"eradicate_on_destroy"`
	InternalID                     types.String   `tfsdk:"internal_id"`
	SnapshotProtectionPolicy       types.List     `tfsdk:"snapshot_protection_policy"`
	Timeouts                       timeouts.Value `tfsdk:"timeouts"`
}

type SnapshotProtectionPolicyModel struct {
	Frequency durationValue `tfsdk:"frequency"`
	Retention durationValue `tfsdk:"retention"`
}

// snapshotProtectionPolicyObjectType mirrors the "snapshot_protection_policy"
// nested block's schema, so ListValueFrom can build a types.List of it from a
// []SnapshotProtectionPolicyModel via its tfsdk tags.
var snapshotProtectionPolicyObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"frequency": durationType{},
	"retention": durationType{},
}}

// volumeGroupSourceTypeOrDefault resolves an unset (null) source_type to "none", the
// schema's own default, so callers that build API bodies don't have to special-case an
// unresolved value that should never actually reach them once defaults have applied.
func volumeGroupSourceTypeOrDefault(v types.String) string {
	if v.IsNull() || v.ValueString() == "" {
		return volumeGroupSourceTypeNone
	}
	return v.ValueString()
}

// volumeGroupSourceRow is one non-"none" source_type value's row: the
// source_*_id attribute it requires, how to read that attribute off the model,
// and how to set the corresponding field on the create request's properties.
// "none" has no row — it requires no source attribute, so the schema's OneOf
// validator, ValidateConfig's matrix and Create's own recheck all treat "names no
// row" as the generic "must not be set"/"nothing to require" case rather than
// needing a special case for it.
type volumeGroupSourceRow struct {
	sourceType string
	attribute  path.Path
	get        func(VolumeGroupResourceModel) types.String
	set        func(props *armpurestorageblock.VolumeGroupProperties, resourceID string) error
}

var volumeGroupSourceRows = []volumeGroupSourceRow{
	{
		sourceType: volumeGroupSourceTypeVolumeGroup,
		attribute:  path.Root("source_volume_group_id"),
		get:        func(m VolumeGroupResourceModel) types.String { return m.SourceVolumeGroupID.StringValue },
		set: func(props *armpurestorageblock.VolumeGroupProperties, resourceID string) error {
			props.SourceVolumeGroupResourceID = to.Ptr(resourceID)
			return nil
		},
	},
	{
		sourceType: volumeGroupSourceTypeSnapshot,
		attribute:  path.Root("source_snapshot_id"),
		get:        func(m VolumeGroupResourceModel) types.String { return m.SourceSnapshotID.StringValue },
		set: func(props *armpurestorageblock.VolumeGroupProperties, resourceID string) error {
			// A snapshot-sourced create also has to carry the snapshot's parent volume
			// group ID: that volume group is what the caller's access to the source is
			// authorized against, and the create is rejected when the ID is absent or
			// isn't the snapshot's direct parent. It holds no information the snapshot
			// ID doesn't already, so it's derived here instead of being a second
			// attribute whose only legal value is this one.
			snapshot, err := armids.TypeVolumeGroupSnapshot.Parse(resourceID)
			if err != nil {
				return err
			}
			sourceVolumeGroupID, err := snapshot.ParentID()
			if err != nil {
				return err
			}
			props.SourceSnapshotResourceID = to.Ptr(resourceID)
			props.SourceVolumeGroupResourceID = to.Ptr(sourceVolumeGroupID)
			return nil
		},
	},
	{
		sourceType: volumeGroupSourceTypeRecoverableVolumeGroup,
		attribute:  path.Root("source_recoverable_volume_group_id"),
		get: func(m VolumeGroupResourceModel) types.String {
			return m.SourceRecoverableVolumeGroupID.StringValue
		},
		set: func(props *armpurestorageblock.VolumeGroupProperties, resourceID string) error {
			props.SourceRecoverableVolumeGroupResourceID = to.Ptr(resourceID)
			return nil
		},
	},
}

// volumeGroupSourceRowFor returns the row for sourceType. ok is false for
// sourceType == "none" and for any value the schema's OneOf validator doesn't
// accept, neither of which name a source_*_id attribute.
func volumeGroupSourceRowFor(sourceType string) (volumeGroupSourceRow, bool) {
	for _, row := range volumeGroupSourceRows {
		if row.sourceType == sourceType {
			return row, true
		}
	}
	return volumeGroupSourceRow{}, false
}

// volumeGroupSourceTypeValues lists every value the source_type schema
// attribute's OneOf validator accepts, "none" first.
func volumeGroupSourceTypeValues() []string {
	values := []string{volumeGroupSourceTypeNone}
	for _, row := range volumeGroupSourceRows {
		values = append(values, row.sourceType)
	}
	return values
}

// missingSourceAttribute checks whether sourceType names a source_*_id attribute
// (via volumeGroupSourceRowFor) that is null on data, returning that attribute's
// path, which both ValidateConfig and Create's own apply-time recheck report. sourceType not naming one at all (either "none", or an
// otherwise-invalid value the OneOf validator already reports) is not "missing" —
// it returns missing = false.
func missingSourceAttribute(sourceType string, data VolumeGroupResourceModel) (wantPath path.Path, missing bool) {
	row, ok := volumeGroupSourceRowFor(sourceType)
	if !ok || !row.get(data).IsNull() {
		return path.Path{}, false
	}
	return row.attribute, true
}

// toVolumeGroup builds the PUT body for Create from the planned configuration.
// location is the parent storage pool's location: nothing in the create chain
// validates a volume group's location against its pool's, so copying it here is what
// keeps the two from silently diverging.
func toVolumeGroup(ctx context.Context, data VolumeGroupResourceModel, location string) (armpurestorageblock.VolumeGroup, diag.Diagnostics) {
	var diags diag.Diagnostics

	tags, d := tagsToAPI(ctx, data.Tags)
	diags.Append(d...)

	policy, d := blockElements[SnapshotProtectionPolicyModel](ctx, data.SnapshotProtectionPolicy)
	diags.Append(d...)

	vg := armpurestorageblock.VolumeGroup{
		Location: to.Ptr(location),
		Tags:     tags,
		Properties: &armpurestorageblock.VolumeGroupProperties{
			PerformanceParameters: toPerformanceParameters(data.BandwidthLimitInMbps, data.IopsLimit),
			ProtectionParameters:  toProtectionParametersCreate(policy),
		},
	}

	// A blank volume group must OMIT sourceType from the create body entirely.
	// The API's enum documents "none" as a valid value, but the service's create
	// validation rejects it when sent explicitly ("Invalid sourceType 'none'.
	// Allowed values are: volumeGroup, snapshot, recoverableVolumeGroup") —
	// confirmed against a real deployment. Only a real source sets the field.
	sourceType := volumeGroupSourceTypeOrDefault(data.SourceType)
	if row, ok := volumeGroupSourceRowFor(sourceType); ok {
		vg.Properties.SourceType = to.Ptr(armpurestorageblock.VolumeGroupSourceType(sourceType))
		if err := row.set(vg.Properties, row.get(data).ValueString()); err != nil {
			diags.Append(diag.WithPath(row.attribute, invalidAttributeValueDiagnostic(idErrorCause(err))))
		}
	}

	return vg, diags
}

// toVolumeGroupUpdate builds the properties PATCH body for Update. Like the storage
// pool's own toStoragePoolUpdate, it always reflects the plan's full desired end state
// for every properties section (limits, protection): both are plain Optional, never
// Computed, so the plan already carries the config's exact value for anything the
// practitioner didn't touch, and sending the full section is equivalent to a selective
// diff. Tags are deliberately absent: the backend ignores the tags of a PATCH that also
// carries properties, so they travel in toVolumeGroupTagsUpdate's own request.
func toVolumeGroupUpdate(ctx context.Context, plan VolumeGroupResourceModel) (armpurestorageblock.VolumeGroupUpdate, diag.Diagnostics) {
	var diags diag.Diagnostics

	policy, d := blockElements[SnapshotProtectionPolicyModel](ctx, plan.SnapshotProtectionPolicy)
	diags.Append(d...)

	update := armpurestorageblock.VolumeGroupUpdate{
		Properties: &armpurestorageblock.VolumeGroupUpdateProperties{
			PerformanceParameters: toPerformanceParametersUpdate(plan.BandwidthLimitInMbps, plan.IopsLimit),
			ProtectionParameters:  toProtectionParametersUpdate(policy),
		},
	}

	return update, diags
}

// toVolumeGroupTagsUpdate builds the tags-only PATCH body for Update. A PATCH that
// carries only tags is applied in place by the backend, whereas tags sent alongside
// properties are silently dropped.
func toVolumeGroupTagsUpdate(ctx context.Context, plan VolumeGroupResourceModel) (armpurestorageblock.VolumeGroupUpdate, diag.Diagnostics) {
	tags, diags := tagsToAPIForUpdate(ctx, plan.Tags)
	return armpurestorageblock.VolumeGroupUpdate{Tags: tags}, diags
}

// toPerformanceParameters builds Create's performance parameters. A limit that is
// null in config is left out of the object entirely (a nil pointer), matching how the
// API itself treats "no limit" as absence rather than an explicit zero; if neither
// limit is configured, the whole object is nil.
func toPerformanceParameters(bandwidthLimitInMbps, iopsLimit types.Int64) *armpurestorageblock.PerformanceParameters {
	if bandwidthLimitInMbps.IsNull() && iopsLimit.IsNull() {
		return nil
	}

	pp := &armpurestorageblock.PerformanceParameters{}
	if !bandwidthLimitInMbps.IsNull() {
		pp.BandwidthLimitMbPerSec = bandwidthLimitInMbps.ValueInt64Pointer()
	}
	if !iopsLimit.IsNull() {
		pp.IopsLimit = iopsLimit.ValueInt64Pointer()
	}
	return pp
}

// toPerformanceParametersUpdate builds Update's performance parameters. Since both
// limit attributes are plain Optional (not Computed), the plan always carries the
// full desired end state, known at apply time either way: a configured value sets
// the limit, and a null one asserts "clear it" via an explicit 0 — the API's own
// signal for "no limit" on PATCH. There is no "leave unchanged" case to represent.
func toPerformanceParametersUpdate(bandwidthLimitInMbps, iopsLimit types.Int64) *armpurestorageblock.PerformanceParameters {
	// types.Int64.ValueInt64() already returns 0 for a null value, which is
	// exactly the explicit-clear signal this PATCH needs to send.
	return &armpurestorageblock.PerformanceParameters{
		BandwidthLimitMbPerSec: to.Ptr(bandwidthLimitInMbps.ValueInt64()),
		IopsLimit:              to.Ptr(iopsLimit.ValueInt64()),
	}
}

// performanceParametersFromAPI maps a GET/response performance parameters object back
// to the two limit attributes. Absence — a nil object, or a nil field within one —
// always maps to null: unlike tags, there's no separate "cleared" vs "never set"
// distinction to preserve, so no prior-value-aware mapping is needed here.
func performanceParametersFromAPI(pp *armpurestorageblock.PerformanceParameters) (bandwidthLimitInMbps, iopsLimit types.Int64) {
	if pp == nil {
		return types.Int64Null(), types.Int64Null()
	}
	return types.Int64PointerValue(pp.BandwidthLimitMbPerSec), types.Int64PointerValue(pp.IopsLimit)
}

// toProtectionParametersCreate builds Create's protection parameters. An absent block
// omits the object entirely, matching the API's own "no schedule" representation;
// the API rejects a request where only one of frequency/retention is set, so the
// schema requires both once the block is present, and this always sends both together.
func toProtectionParametersCreate(policy []SnapshotProtectionPolicyModel) *armpurestorageblock.ProtectionParameters {
	if len(policy) != 1 {
		return nil
	}
	return &armpurestorageblock.ProtectionParameters{
		Frequency: to.Ptr(policy[0].Frequency.ValueString()),
		Retention: to.Ptr(policy[0].Retention.ValueString()),
	}
}

// toProtectionParametersUpdate builds Update's protection parameters. Removing the
// block must still send a non-nil, empty ProtectionParameters{}: the generated SDK's
// pointer fields all carry omitempty, so a nil pointer here would omit the whole
// "protectionParameters" key from the PATCH body (meaning "leave unchanged" to the
// API) instead of disabling it, while an explicit empty struct serializes as "{}",
// the one wire shape the backend treats as "disable protection".
func toProtectionParametersUpdate(policy []SnapshotProtectionPolicyModel) *armpurestorageblock.ProtectionParameters {
	if len(policy) != 1 {
		return &armpurestorageblock.ProtectionParameters{}
	}
	return &armpurestorageblock.ProtectionParameters{
		Frequency: to.Ptr(policy[0].Frequency.ValueString()),
		Retention: to.Ptr(policy[0].Retention.ValueString()),
	}
}

// protectionParametersFromAPI maps a GET/response protection parameters object back to
// the snapshot_protection_policy block, as the types.List the schema needs since the
// block's for_each can be a dynamic, unknown value at plan time (see
// blockElements in block_elements.go). The API returns nil for "no functioning policy"
// (disabled, or frequency 0); anything short of both fields present is treated the
// same way, since a schedule can't exist with only one of them set — both map to a
// null list, not an empty one, so a config that never had the block sees no diff.
func protectionParametersFromAPI(ctx context.Context, pp *armpurestorageblock.ProtectionParameters) (types.List, diag.Diagnostics) {
	if pp == nil || pp.Frequency == nil || pp.Retention == nil {
		return types.ListNull(snapshotProtectionPolicyObjectType), nil
	}
	return types.ListValueFrom(ctx, snapshotProtectionPolicyObjectType, []SnapshotProtectionPolicyModel{{
		Frequency: durationValue{StringValue: types.StringPointerValue(pp.Frequency)},
		Retention: durationValue{StringValue: types.StringPointerValue(pp.Retention)},
	}})
}

// applyVolumeGroup maps an API response onto data, in place. It deliberately never
// touches source_type, the three source_*_id attributes, storage_pool_id or the
// provider-only deletion attributes: the API never returns any of them (the source
// fields are create-only by the swagger's own x-ms-mutability, and the rest have no
// API representation at all), so leaving data's existing fields alone is what
// preserves them across Create/Read/Update.
//
// name is not mapped either, though the response carries it. It is a Required
// attribute, so the state saved after Create or Update must equal the configured value
// exactly; ARM treats resource names case-insensitively and may echo back a casing the
// practitioner never wrote, which would fail the apply with "Provider produced
// inconsistent result after apply". Nothing is lost by ignoring it: the name is always
// already on data by the time this runs — from the plan on Create and Update, from
// state on Read, and from the parsed ARM ID on import — and a volume group cannot be
// renamed in place, so the response can never carry a name the caller didn't ask for.
//
// It also never touches tags — see applyVolumeGroupTags below for why — so Create
// and Update leave data.Tags exactly as the plan set it; Read calls
// applyVolumeGroupTags itself afterward to map the response's tags.
func applyVolumeGroup(ctx context.Context, data *VolumeGroupResourceModel, vg armpurestorageblock.VolumeGroup) diag.Diagnostics {
	var diags diag.Diagnostics

	if vg.Properties == nil {
		diags.Append(unexpectedResponseDiagnostic(newNoPropertiesError(kindVolumeGroup)))
		return diags
	}
	props := vg.Properties

	data.ID = armids.IDValue{StringValue: types.StringPointerValue(vg.ID)}
	data.InternalID = types.StringPointerValue(props.VolumeGroupInternalID)

	data.BandwidthLimitInMbps, data.IopsLimit = performanceParametersFromAPI(props.PerformanceParameters)

	policy, d := protectionParametersFromAPI(ctx, props.ProtectionParameters)
	diags.Append(d...)
	data.SnapshotProtectionPolicy = policy

	return diags
}

// applyVolumeGroupTags maps the API response's tags onto data.Tags. It exists apart
// from applyVolumeGroup, and is called only by Read, because tags is Optional but not
// Computed: the framework requires the state saved after Create or Update to equal
// the plan's value exactly, but the tags an API response carries can differ from what
// was sent — e.g. the resource provider layer stamping extra tags (env/org/owner,
// confirmed against a real deployment) onto every created volume group. Mapping the
// response into state on Create/Update would then fail every create with "Provider
// produced inconsistent result after apply" whenever that happens. Read has no such
// constraint — tags is not Computed there either, but Read's whole job is to surface
// exactly this kind of out-of-band drift — so it maps the response as usual.
func applyVolumeGroupTags(ctx context.Context, data *VolumeGroupResourceModel, apiTags map[string]*string) diag.Diagnostics {
	tags, diags := tagsFromAPI(ctx, apiTags, data.Tags)
	data.Tags = tags
	return diags
}

// applyVolumeGroupRead maps a GET or list response onto data exactly as Read does: the
// properties (applyVolumeGroup) plus the tags (applyVolumeGroupTags), which Create and
// Update deliberately leave alone. Read and the list resource both call this, so a
// volume group found by listing carries the same values as the same volume group after
// import and Read.
func applyVolumeGroupRead(ctx context.Context, data *VolumeGroupResourceModel, vg armpurestorageblock.VolumeGroup) diag.Diagnostics {
	diags := applyVolumeGroup(ctx, data, vg)
	diags.Append(applyVolumeGroupTags(ctx, data, vg.Tags)...)
	return diags
}

// seedImportedVolumeGroup sets the attributes ImportState writes before the first Read:
// what can be derived from the ID alone, plus the provider-side defaults. The defaults
// must be explicit rather than left null: left null, the next plan would fill them in
// from the schema default, and source_type being RequiresReplace would then show a
// spurious replace. An imported volume group declares source_type = "none" whatever it
// was created from, since the API cannot report a volume group's origin after the fact.
func seedImportedVolumeGroup(data *VolumeGroupResourceModel, importID string, parsed armids.VolumeGroupID) {
	data.ID = armids.IDValue{StringValue: types.StringValue(importID)}
	data.Name = types.StringValue(parsed.VolumeGroupName)
	// Composed rather than cut from the import ID's own text, which normalises the
	// segment keywords and namespace to their canonical spelling.
	data.StoragePoolID = armids.IDValue{StringValue: types.StringValue(parsed.StoragePool().FullID())}
	data.SourceType = types.StringValue(volumeGroupSourceTypeNone)
	data.DeletionProtection = types.BoolValue(true)
	data.EradicateOnDestroy = types.BoolValue(false)
}

// seedVolumeGroupState is what ImportState and a move both write to a new state: the
// seedImportedVolumeGroup attributes, and nothing else, so that state before the first
// Read has the same shape whichever way the volume group came under management.
func seedVolumeGroupState(ctx context.Context, state *tfsdk.State, id string, parsed armids.VolumeGroupID) diag.Diagnostics {
	var seed VolumeGroupResourceModel
	seedImportedVolumeGroup(&seed, id, parsed)
	var diags diag.Diagnostics
	diags.Append(state.SetAttribute(ctx, path.Root("id"), seed.ID)...)
	diags.Append(state.SetAttribute(ctx, path.Root("name"), seed.Name)...)
	diags.Append(state.SetAttribute(ctx, path.Root("storage_pool_id"), seed.StoragePoolID)...)
	diags.Append(state.SetAttribute(ctx, path.Root("source_type"), seed.SourceType)...)
	diags.Append(state.SetAttribute(ctx, path.Root("deletion_protection"), seed.DeletionProtection)...)
	diags.Append(state.SetAttribute(ctx, path.Root("eradicate_on_destroy"), seed.EradicateOnDestroy)...)
	return diags
}

// importedVolumeGroupFromAPI builds the resource object a fresh import of vg followed by
// Read would leave in state. blank must be a model whose every attribute is a typed
// null, the shape state has before ImportState runs. The volume group's own ID (as the
// API spells it) plays the part of the import ID.
func importedVolumeGroupFromAPI(
	ctx context.Context, blank VolumeGroupResourceModel, vg armpurestorageblock.VolumeGroup,
) (VolumeGroupResourceModel, armids.VolumeGroupID, diag.Diagnostics) {
	var diags diag.Diagnostics
	data := blank
	if vg.ID == nil {
		diags.Append(unexpectedResponseDiagnostic(newMissingResponseIDError(kindVolumeGroup)))
		return data, armids.VolumeGroupID{}, diags
	}
	parsed, err := armids.ParseVolumeGroupID(*vg.ID)
	if err != nil {
		diags.Append(unexpectedResponseDiagnostic(newUnparseableResponseIDError(kindVolumeGroup, idErrorCause(err))))
		return data, armids.VolumeGroupID{}, diags
	}
	seedImportedVolumeGroup(&data, *vg.ID, parsed)
	diags.Append(applyVolumeGroupRead(ctx, &data, vg)...)
	return data, parsed, diags
}

// volumeGroupDataSourceItem is the read-side attribute set shared by the singular
// everpure_azure_volume_group data source and each element of the plural
// everpure_azure_volume_groups data source's volume_groups list. StoragePoolID is left
// unset here: both callers set it themselves to the data source's own configured
// storage_pool_id, verbatim.
type volumeGroupDataSourceItem struct {
	ID                       armids.IDValue `tfsdk:"id"`
	Name                     types.String   `tfsdk:"name"`
	StoragePoolID            armids.IDValue `tfsdk:"storage_pool_id"`
	BandwidthLimitInMbps     types.Int64    `tfsdk:"bandwidth_limit_in_mbps"`
	IopsLimit                types.Int64    `tfsdk:"iops_limit"`
	Tags                     types.Map      `tfsdk:"tags"`
	InternalID               types.String   `tfsdk:"internal_id"`
	SnapshotProtectionPolicy types.List     `tfsdk:"snapshot_protection_policy"`
}

// volumeGroupItemFrom maps an API response onto a fresh volumeGroupDataSourceItem, by
// routing it through the resource's own applyVolumeGroup/applyVolumeGroupTags and
// copying the result across — one mapping implementation for the resource and every
// data source built on top of it. Name is filled directly from vg.Name rather than
// through applyVolumeGroup, which deliberately never maps it (see applyVolumeGroup's
// own doc comment): the singular data source overwrites it with its own configured
// value afterward, while the plural data source has no configured name of its own and
// keeps the spelling the API returned.
func volumeGroupItemFrom(ctx context.Context, vg armpurestorageblock.VolumeGroup) (volumeGroupDataSourceItem, diag.Diagnostics) {
	var scratch VolumeGroupResourceModel
	scratch.Tags = types.MapNull(types.StringType)
	diags := applyVolumeGroup(ctx, &scratch, vg)
	diags.Append(applyVolumeGroupTags(ctx, &scratch, vg.Tags)...)
	if diags.HasError() {
		return volumeGroupDataSourceItem{}, diags
	}

	return volumeGroupDataSourceItem{
		ID:                       scratch.ID,
		Name:                     types.StringPointerValue(vg.Name),
		BandwidthLimitInMbps:     scratch.BandwidthLimitInMbps,
		IopsLimit:                scratch.IopsLimit,
		Tags:                     scratch.Tags,
		InternalID:               scratch.InternalID,
		SnapshotProtectionPolicy: scratch.SnapshotProtectionPolicy,
	}, diags
}
