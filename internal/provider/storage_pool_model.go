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

// StoragePoolResourceModel is the Terraform-side representation of
// everpure_azure_storage_pool.
type StoragePoolResourceModel struct {
	ID                           armids.IDValue `tfsdk:"id"`
	Name                         types.String   `tfsdk:"name"`
	ResourceGroupName            types.String   `tfsdk:"resource_group_name"`
	Location                     types.String   `tfsdk:"location"`
	Zone                         types.String   `tfsdk:"zone"`
	ReservationID                armids.IDValue `tfsdk:"reservation_id"`
	ProvisionedBandwidthInMbps   types.Int64    `tfsdk:"provisioned_bandwidth_in_mbps"`
	Tags                         types.Map      `tfsdk:"tags"`
	InternalID                   types.String   `tfsdk:"internal_id"`
	ProvisionedIops              types.Int64    `tfsdk:"provisioned_iops"`
	DataRetentionPeriodInSeconds types.Int64    `tfsdk:"data_retention_period_in_seconds"`
	VnetInjection                types.List     `tfsdk:"vnet_injection"`
	PlatformConsole              types.List     `tfsdk:"platform_console"`
	Timeouts                     timeouts.Value `tfsdk:"timeouts"`
}

type VnetInjectionModel struct {
	SubnetID         armids.IDValue `tfsdk:"subnet_id"`
	VirtualNetworkID armids.IDValue `tfsdk:"virtual_network_id"`
}

// vnetInjectionObjectType mirrors the "vnet_injection" nested block's schema,
// so ListValueFrom can build a types.List of it from a []VnetInjectionModel
// via its tfsdk tags.
var vnetInjectionObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"subnet_id":          armids.IDType{},
	"virtual_network_id": armids.IDType{},
}}

type PlatformConsoleModel struct {
	GUIEnabled      types.Bool   `tfsdk:"gui_enabled"`
	APIEnabled      types.Bool   `tfsdk:"api_enabled"`
	CLIEnabled      types.Bool   `tfsdk:"cli_enabled"`
	SubnetIDs       types.List   `tfsdk:"subnet_ids"`
	DefaultUsername types.String `tfsdk:"default_username"`
	// Subnet must be types.List, not a plain Go slice: it is a Computed-only
	// nested attribute list, so at plan time (before the API has responded)
	// its value is genuinely unknown — a state only a types.* value, not a
	// bare Go slice, is able to represent.
	Subnet types.List `tfsdk:"subnet"`
}

// platformConsoleSubnetObjectType mirrors the "subnet" nested attribute
// object's schema, so ListValueFrom can build a types.List of it from a
// []PlatformConsoleSubnetModel via its tfsdk tags.
var platformConsoleSubnetObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"id":                           types.StringType,
	"management_ip_address":        types.StringType,
	"service_backend_ip_addresses": types.ListType{ElemType: types.StringType},
}}

type PlatformConsoleSubnetModel struct {
	ID                        types.String `tfsdk:"id"`
	ManagementIPAddress       types.String `tfsdk:"management_ip_address"`
	ServiceBackendIPAddresses types.List   `tfsdk:"service_backend_ip_addresses"`
}

// platformConsoleObjectType mirrors the "platform_console" nested block's
// schema, so ListValueFrom can build a types.List of it from a
// []PlatformConsoleModel via its tfsdk tags.
var platformConsoleObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"gui_enabled":      types.BoolType,
	"api_enabled":      types.BoolType,
	"cli_enabled":      types.BoolType,
	"subnet_ids":       types.ListType{ElemType: types.StringType},
	"default_username": types.StringType,
	"subnet":           types.ListType{ElemType: platformConsoleSubnetObjectType},
}}

// toStoragePool builds the PUT body for Create from the planned configuration.
func toStoragePool(ctx context.Context, data StoragePoolResourceModel) (armpurestorageblock.StoragePool, diag.Diagnostics) {
	var diags diag.Diagnostics

	tags, d := tagsToAPI(ctx, data.Tags)
	diags.Append(d...)

	vnetInjection, d := blockElements[VnetInjectionModel](ctx, data.VnetInjection)
	diags.Append(d...)

	platformConsole, d := blockElements[PlatformConsoleModel](ctx, data.PlatformConsole)
	diags.Append(d...)

	pool := armpurestorageblock.StoragePool{
		Location: to.Ptr(data.Location.ValueString()),
		Tags:     tags,
		Properties: &armpurestorageblock.StoragePoolProperties{
			AvailabilityZone:             to.Ptr(data.Zone.ValueString()),
			ProvisionedBandwidthMbPerSec: data.ProvisionedBandwidthInMbps.ValueInt64Pointer(),
			ReservationResourceID:        to.Ptr(data.ReservationID.ValueString()),
			VnetInjection: &armpurestorageblock.VnetInjection{
				SubnetID: to.Ptr(vnetInjection[0].SubnetID.ValueString()),
				VnetID:   to.Ptr(vnetInjection[0].VirtualNetworkID.ValueString()),
			},
		},
	}

	if len(platformConsole) == 1 {
		pool.Properties.PlatformConsoleSettings = platformConsoleToAPI(platformConsole[0], vnetInjection[0].SubnetID.ValueString())
	}

	return pool, diags
}

// toStoragePoolUpdate builds the PATCH body for Update. Tags and bandwidth are always
// sent from the plan: Terraform's plan already carries the unchanged value for
// anything the practitioner didn't touch, so sending the full section is equivalent
// to a selective diff. The platform console is sent only when its configuration
// changed against priorPlatformConsole (see platformConsoleConfigChanged), so a
// tags-only or bandwidth-only change does not re-send settings the pool already has.
// vnetSubnetID is the (immutable) vnet_injection subnet, needed both to populate
// platform console subnets when enabling it and to keep them populated when disabling
// it.
func toStoragePoolUpdate(ctx context.Context, plan StoragePoolResourceModel, priorPlatformConsole types.List, vnetSubnetID string) (armpurestorageblock.StoragePoolUpdate, diag.Diagnostics) {
	var diags diag.Diagnostics

	tags, d := tagsToAPIForUpdate(ctx, plan.Tags)
	diags.Append(d...)

	platformConsole, d := blockElements[PlatformConsoleModel](ctx, plan.PlatformConsole)
	diags.Append(d...)

	consoleChanged, d := platformConsoleConfigChanged(ctx, priorPlatformConsole, plan.PlatformConsole)
	diags.Append(d...)

	update := armpurestorageblock.StoragePoolUpdate{
		Tags: tags,
		Properties: &armpurestorageblock.StoragePoolUpdateProperties{
			ProvisionedBandwidthMbPerSec: plan.ProvisionedBandwidthInMbps.ValueInt64Pointer(),
		},
	}

	switch {
	case !consoleChanged:
	case len(platformConsole) == 1:
		update.Properties.PlatformConsoleSettings = platformConsoleToAPI(platformConsole[0], vnetSubnetID)
	default:
		// Backend requires every service off when disabling, and still wants
		// the current subnet named, not an empty list.
		update.Properties.PlatformConsoleSettings = &armpurestorageblock.PlatformConsoleSettings{
			Enabled: to.Ptr(false),
			Gui:     &armpurestorageblock.PlatformConsoleAccessSettings{Enabled: to.Ptr(false)},
			API:     &armpurestorageblock.PlatformConsoleAccessSettings{Enabled: to.Ptr(false)},
			Cli:     &armpurestorageblock.PlatformConsoleAccessSettings{Enabled: to.Ptr(false)},
			Subnets: []*armpurestorageblock.PlatformConsoleSubnet{{ID: to.Ptr(vnetSubnetID)}},
		}
	}

	return update, diags
}

func platformConsoleToAPI(pc PlatformConsoleModel, vnetSubnetID string) *armpurestorageblock.PlatformConsoleSettings {
	return &armpurestorageblock.PlatformConsoleSettings{
		Enabled: to.Ptr(true),
		Gui:     &armpurestorageblock.PlatformConsoleAccessSettings{Enabled: pc.GUIEnabled.ValueBoolPointer()},
		API:     &armpurestorageblock.PlatformConsoleAccessSettings{Enabled: pc.APIEnabled.ValueBoolPointer()},
		Cli:     &armpurestorageblock.PlatformConsoleAccessSettings{Enabled: pc.CLIEnabled.ValueBoolPointer()},
		Subnets: []*armpurestorageblock.PlatformConsoleSubnet{{ID: to.Ptr(vnetSubnetID)}},
	}
}

// applyStoragePool maps an API response onto data, in place.
//
// name is not mapped, though the response carries it. It is a Required attribute, so
// the state saved after Create or Update must equal the configured value exactly; ARM
// treats resource names case-insensitively and may echo back a casing the practitioner
// never wrote, which would fail the apply with "Provider produced inconsistent result
// after apply". Nothing is lost by ignoring it: the name is always already on data by
// the time this runs — from the plan on Create and Update, from state on Read, and
// from the parsed ARM ID on import — and a pool cannot be renamed in place, so the
// response can never carry a name the caller didn't ask for.
//
// It deliberately never touches tags — see applyStoragePoolTags below for why — so
// Create and Update leave data.Tags exactly as the plan set it; Read calls
// applyStoragePoolTags itself afterward to map the response's tags.
func applyStoragePool(ctx context.Context, data *StoragePoolResourceModel, pool armpurestorageblock.StoragePool) diag.Diagnostics {
	var diags diag.Diagnostics

	if pool.Properties == nil {
		diags.Append(unexpectedResponseDiagnostic(newNoPropertiesError(kindStoragePool)))
		return diags
	}

	props := pool.Properties

	data.ID = armids.IDValue{StringValue: types.StringPointerValue(pool.ID)}
	data.Location = types.StringPointerValue(canonicalLocationPtr(pool.Location))
	data.Zone = types.StringPointerValue(props.AvailabilityZone)
	data.ProvisionedBandwidthInMbps = types.Int64PointerValue(props.ProvisionedBandwidthMbPerSec)
	data.ReservationID = armids.IDValue{StringValue: types.StringPointerValue(props.ReservationResourceID)}
	data.InternalID = types.StringPointerValue(props.StoragePoolInternalID)
	data.ProvisionedIops = types.Int64PointerValue(props.ProvisionedIops)
	data.DataRetentionPeriodInSeconds = types.Int64PointerValue(props.DataRetentionPeriod)

	if props.VnetInjection != nil {
		vnetInjection, d := types.ListValueFrom(ctx, vnetInjectionObjectType, []VnetInjectionModel{{
			SubnetID:         armids.IDValue{StringValue: types.StringPointerValue(props.VnetInjection.SubnetID)},
			VirtualNetworkID: armids.IDValue{StringValue: types.StringPointerValue(props.VnetInjection.VnetID)},
		}})
		diags.Append(d...)
		data.VnetInjection = vnetInjection
	}

	platformConsole, d := platformConsoleFromAPI(ctx, props.PlatformConsoleSettings)
	diags.Append(d...)
	data.PlatformConsole = platformConsole

	return diags
}

// applyStoragePoolTags maps the API response's tags onto data.Tags. It exists apart
// from applyStoragePool, and is called only by Read, because tags is Optional but not
// Computed: the framework requires the state saved after Create or Update to equal
// the plan's value exactly, but the tags an API response carries can differ from what
// was sent — e.g. an environment's own Azure Policy, or the resource provider layer,
// appending tags outside Terraform's control. Mapping the response into state on
// Create/Update would then fail every create with "Provider produced inconsistent
// result after apply" whenever that happens. Read has no such constraint — tags is
// not Computed there either, but Read's whole job is to surface exactly this kind of
// out-of-band drift — so it maps the response as usual.
func applyStoragePoolTags(ctx context.Context, data *StoragePoolResourceModel, apiTags map[string]*string) diag.Diagnostics {
	tags, diags := tagsFromAPI(ctx, apiTags, data.Tags)
	data.Tags = tags
	return diags
}

// applyStoragePoolRead maps a GET or list response onto data exactly as Read does: the
// properties (applyStoragePool) plus the tags (applyStoragePoolTags), which Create and
// Update deliberately leave alone. Read and the list resource both call this, so a
// pool found by listing carries the same values as the same pool after import and Read.
func applyStoragePoolRead(ctx context.Context, data *StoragePoolResourceModel, pool armpurestorageblock.StoragePool) diag.Diagnostics {
	diags := applyStoragePool(ctx, data, pool)
	diags.Append(applyStoragePoolTags(ctx, data, pool.Tags)...)
	return diags
}

// seedImportedStoragePool sets the attributes ImportState writes before the first Read:
// everything that can be derived from the ID alone. A pool has no provider-side
// defaults to seed.
func seedImportedStoragePool(data *StoragePoolResourceModel, importID string, parsed armids.StoragePoolID) {
	data.ID = armids.IDValue{StringValue: types.StringValue(importID)}
	data.Name = types.StringValue(parsed.StoragePoolName)
	data.ResourceGroupName = types.StringValue(parsed.ResourceGroupName)
}

// seedStoragePoolState is what ImportState and a move both write to a new state: the
// seedImportedStoragePool attributes, and nothing else, so that state before the first
// Read has the same shape whichever way the pool came under management.
func seedStoragePoolState(ctx context.Context, state *tfsdk.State, id string, parsed armids.StoragePoolID) diag.Diagnostics {
	var seed StoragePoolResourceModel
	seedImportedStoragePool(&seed, id, parsed)
	var diags diag.Diagnostics
	diags.Append(state.SetAttribute(ctx, path.Root("id"), seed.ID)...)
	diags.Append(state.SetAttribute(ctx, path.Root("name"), seed.Name)...)
	diags.Append(state.SetAttribute(ctx, path.Root("resource_group_name"), seed.ResourceGroupName)...)
	return diags
}

// importedStoragePoolFromAPI builds the resource object a fresh import of pool followed
// by Read would leave in state. blank must be a model whose every attribute is a typed
// null, the shape state has before ImportState runs. The pool's own ID (as the API
// spells it) plays the part of the import ID.
func importedStoragePoolFromAPI(
	ctx context.Context, blank StoragePoolResourceModel, pool armpurestorageblock.StoragePool,
) (StoragePoolResourceModel, armids.StoragePoolID, diag.Diagnostics) {
	var diags diag.Diagnostics
	data := blank
	if pool.ID == nil {
		diags.Append(unexpectedResponseDiagnostic(newMissingResponseIDError(kindStoragePool)))
		return data, armids.StoragePoolID{}, diags
	}
	parsed, err := armids.ParseStoragePoolID(*pool.ID)
	if err != nil {
		diags.Append(unexpectedResponseDiagnostic(newUnparseableResponseIDError(kindStoragePool, idErrorCause(err))))
		return data, armids.StoragePoolID{}, diags
	}
	seedImportedStoragePool(&data, *pool.ID, parsed)
	diags.Append(applyStoragePoolRead(ctx, &data, pool)...)
	return data, parsed, diags
}

func platformConsoleAccessEnabled(a *armpurestorageblock.PlatformConsoleAccessSettings) *bool {
	if a == nil {
		return nil
	}
	return a.Enabled
}

// platformConsoleFromAPI maps a GET/response platform console settings object
// back to the platform_console block, as the types.List the schema needs
// since the block's for_each can be a dynamic, unknown value at plan time
// (see blockElements). Disabled or absent settings map to a null
// list, not an empty one, so a config that never had the block sees no diff.
func platformConsoleFromAPI(ctx context.Context, settings *armpurestorageblock.PlatformConsoleSettings) (types.List, diag.Diagnostics) {
	var diags diag.Diagnostics
	if settings == nil || settings.Enabled == nil || !*settings.Enabled {
		return types.ListNull(platformConsoleObjectType), diags
	}

	subnetIDs := make([]string, 0, len(settings.Subnets))
	subnetModels := make([]PlatformConsoleSubnetModel, 0, len(settings.Subnets))
	for _, s := range settings.Subnets {
		if s == nil {
			continue
		}
		subnetIDs = append(subnetIDs, derefString(s.ID))

		ips, d := types.ListValueFrom(ctx, types.StringType, derefStrings(s.ServiceBackendIPs))
		diags.Append(d...)

		subnetModels = append(subnetModels, PlatformConsoleSubnetModel{
			ID:                        types.StringPointerValue(s.ID),
			ManagementIPAddress:       types.StringPointerValue(s.ManagementIPAddress),
			ServiceBackendIPAddresses: ips,
		})
	}

	subnetIDList, d := types.ListValueFrom(ctx, types.StringType, subnetIDs)
	diags.Append(d...)

	subnetList, d := types.ListValueFrom(ctx, platformConsoleSubnetObjectType, subnetModels)
	diags.Append(d...)

	list, d := types.ListValueFrom(ctx, platformConsoleObjectType, []PlatformConsoleModel{{
		GUIEnabled:      types.BoolPointerValue(platformConsoleAccessEnabled(settings.Gui)),
		APIEnabled:      types.BoolPointerValue(platformConsoleAccessEnabled(settings.API)),
		CLIEnabled:      types.BoolPointerValue(platformConsoleAccessEnabled(settings.Cli)),
		SubnetIDs:       subnetIDList,
		DefaultUsername: types.StringPointerValue(settings.DefaultUsername),
		Subnet:          subnetList,
	}})
	diags.Append(d...)

	return list, diags
}

// storagePoolDataSourceItem is the read-side attribute set shared by the singular
// everpure_azure_storage_pool data source and each element of the plural
// everpure_azure_storage_pools data source's storage_pools list. Name and
// ResourceGroupName are left at whatever storagePoolItemFrom below put there; each
// caller then sets them the way its own lookup key works (see that function's doc
// comment).
type storagePoolDataSourceItem struct {
	ID                           armids.IDValue `tfsdk:"id"`
	Name                         types.String   `tfsdk:"name"`
	ResourceGroupName            types.String   `tfsdk:"resource_group_name"`
	Location                     types.String   `tfsdk:"location"`
	Zone                         types.String   `tfsdk:"zone"`
	ReservationID                armids.IDValue `tfsdk:"reservation_id"`
	ProvisionedBandwidthInMbps   types.Int64    `tfsdk:"provisioned_bandwidth_in_mbps"`
	Tags                         types.Map      `tfsdk:"tags"`
	InternalID                   types.String   `tfsdk:"internal_id"`
	ProvisionedIops              types.Int64    `tfsdk:"provisioned_iops"`
	DataRetentionPeriodInSeconds types.Int64    `tfsdk:"data_retention_period_in_seconds"`
	VnetInjection                types.List     `tfsdk:"vnet_injection"`
	PlatformConsole              types.List     `tfsdk:"platform_console"`
}

// storagePoolItemFrom maps an API response onto a fresh storagePoolDataSourceItem, by
// routing it through the resource's own applyStoragePool/applyStoragePoolTags and
// copying the result across — one mapping implementation for the resource and every
// data source built on top of it. Name is filled directly from pool.Name rather than
// through applyStoragePool, which deliberately never maps it (see applyStoragePool's
// own doc comment): the singular data source overwrites it with its own configured
// value afterward, while the plural data source has no configured name of its own and
// keeps the spelling the API returned. ResourceGroupName is left unset here: the singular
// data source overwrites the whole item's Name and ResourceGroupName with its own
// configured values afterward (ARM's pool name lookup folds case, so a config spelled
// differently than the pool's own stored casing must still see its own spelling
// reflected back — Terraform core rejects a data source result that changes a
// configured, non-Computed value), while the plural data source parses
// ResourceGroupName from each returned ID instead, since it has no configured name or
// resource group of its own to preserve.
func storagePoolItemFrom(ctx context.Context, pool armpurestorageblock.StoragePool) (storagePoolDataSourceItem, diag.Diagnostics) {
	var scratch StoragePoolResourceModel
	scratch.Tags = types.MapNull(types.StringType)
	scratch.VnetInjection = types.ListNull(vnetInjectionObjectType)
	diags := applyStoragePool(ctx, &scratch, pool)
	diags.Append(applyStoragePoolTags(ctx, &scratch, pool.Tags)...)
	if diags.HasError() {
		return storagePoolDataSourceItem{}, diags
	}

	return storagePoolDataSourceItem{
		ID:                           scratch.ID,
		Name:                         types.StringPointerValue(pool.Name),
		Location:                     scratch.Location,
		Zone:                         scratch.Zone,
		ReservationID:                scratch.ReservationID,
		ProvisionedBandwidthInMbps:   scratch.ProvisionedBandwidthInMbps,
		Tags:                         scratch.Tags,
		InternalID:                   scratch.InternalID,
		ProvisionedIops:              scratch.ProvisionedIops,
		DataRetentionPeriodInSeconds: scratch.DataRetentionPeriodInSeconds,
		VnetInjection:                scratch.VnetInjection,
		PlatformConsole:              scratch.PlatformConsole,
	}, diags
}
