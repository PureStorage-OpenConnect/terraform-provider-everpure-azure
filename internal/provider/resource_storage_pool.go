// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"regexp"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

var (
	_ resource.Resource                   = &StoragePoolResource{}
	_ resource.ResourceWithConfigure      = &StoragePoolResource{}
	_ resource.ResourceWithImportState    = &StoragePoolResource{}
	_ resource.ResourceWithMoveState      = &StoragePoolResource{}
	_ resource.ResourceWithIdentity       = &StoragePoolResource{}
	_ resource.ResourceWithModifyPlan     = &StoragePoolResource{}
	_ resource.ResourceWithValidateConfig = &StoragePoolResource{}
)

// storagePoolNamePattern, locationPattern and zonePattern have exactly one use
// each, on the pool's own "name", "location" and "zone" attributes below;
// none is an ARM ID grammar, so none lives in the armids package's ResourceType table.
var (
	// storagePoolNamePattern is the storage pool's own name grammar: 1-127
	// characters, letters, digits, hyphens and underscores.
	storagePoolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9\-_]{1,127}$`)

	locationPattern = regexp.MustCompile(`^[a-z0-9]+$`)

	// Empty string is accepted alongside a positive integer: some Azure
	// regions have no availability zones, and the API's availabilityZone is
	// a plain string with no format/pattern/enum of its own to confirm what
	// such a region sends.
	zonePattern = regexp.MustCompile(`^([1-9][0-9]*)?$`)
)

// Per-operation defaults, overridable per resource instance via the `timeouts`
// attribute. Create and delete add 5m over the backend's own timeout, since
// our deadline also has to cover the initial request that kicks off the poll.
const (
	defaultStoragePoolCreateTimeout = 95 * time.Minute
	defaultStoragePoolReadTimeout   = 5 * time.Minute
	defaultStoragePoolUpdateTimeout = 30 * time.Minute
	defaultStoragePoolDeleteTimeout = 95 * time.Minute
)

type StoragePoolResource struct {
	// clientsHolder supplies Configure and storagePoolsClient via promotion; see its
	// own doc comment.
	clientsHolder
}

func NewStoragePoolResource() resource.Resource {
	return &StoragePoolResource{}
}

func (r *StoragePoolResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_storage_pool"
}

func (r *StoragePoolResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = armids.StoragePoolIdentitySchema()
}

func (r *StoragePoolResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	immutable := []planmodifier.String{stringplanmodifier.RequiresReplace()}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Storage Pool — a block storage instance injected into a " +
			"virtual network in your subscription.\n\n" +
			"Creation is asynchronous and can take up to 90 minutes. A Reservation must already " +
			"exist in the same subscription and region.\n\n" +
			"~> **Deleting a storage pool is permanent.** There is no soft delete. The API " +
			"refuses to delete a pool that still holds volume groups, volumes or other " +
			"resources, including soft-deleted ones awaiting eradication. See " +
			"[Deletion policies](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/guides/deletion-policies).\n\n" +
			"To read a storage pool managed elsewhere without taking ownership of it, use the " +
			"[`everpure_azure_storage_pool` data source](https://registry.terraform.io/providers/" +
			"PureStorage-OpenConnect/everpure-azure/latest/docs/data-sources/storage_pool).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				CustomType:    armids.IDType{},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "The full Azure Resource Manager ID of the storage pool, e.g. " +
					"`/subscriptions/.../resourceGroups/.../providers/PureStorage.Block/storagePools/pool1`.",
			},
			"name": schema.StringAttribute{
				Required:      true,
				PlanModifiers: immutable,
				Validators: []validator.String{
					stringvalidator.RegexMatches(storagePoolNamePattern, storagePoolNameRule),
				},
				MarkdownDescription: "Name of the storage pool. The name " + storagePoolNameRule + ". Changing this forces a new pool to be created.",
			},
			"resource_group_name": schema.StringAttribute{
				Required: true,
				PlanModifiers: append([]planmodifier.String{armids.PreserveEquivalentResourceGroupName()},
					immutable...),
				MarkdownDescription: "Name of the resource group the pool lives in. Changing this forces " +
					"a new pool to be created.",
			},
			"location": schema.StringAttribute{
				Required:      true,
				PlanModifiers: immutable,
				Validators: []validator.String{
					stringvalidator.RegexMatches(locationPattern, locationRule),
				},
				MarkdownDescription: "Azure region the pool is created in, using the canonical short name, " +
					"e.g. `eastus2`. Must match the region of the Reservation. Changing this forces a new " +
					"pool to be created.",
			},
			"zone": schema.StringAttribute{
				Required:      true,
				PlanModifiers: immutable,
				Validators: []validator.String{
					stringvalidator.RegexMatches(zonePattern, zoneRule),
				},
				MarkdownDescription: "Availability zone within the region, e.g. `\"1\"`. Set to `\"\"` for a " +
					"region with no availability zones. Changing this forces a new pool to be created, " +
					"destroying all data on the existing pool.",
			},
			"reservation_id": schema.StringAttribute{
				Required:   true,
				CustomType: armids.IDType{},
				PlanModifiers: append([]planmodifier.String{armids.PreserveEquivalentIDSpelling()},
					immutable...),
				Validators: []validator.String{
					armids.TypeReservation.Validator(),
				},
				MarkdownDescription: "Full Azure Resource Manager ID of the Reservation that funds this " +
					"pool, e.g. the `id` of an [`everpure_azure_reservation`](https://registry.terraform.io/" +
					"providers/PureStorage-OpenConnect/everpure-azure/latest/docs/resources/reservation) " +
					"resource. The Reservation must already exist in the same subscription and region. " +
					"Changing this forces a new pool to be created.",
			},
			"provisioned_bandwidth_in_mbps": schema.Int64Attribute{
				Required: true,
				Validators: []validator.Int64{
					// The backend rejects anything below 800 MB/s at apply
					// time regardless; validate it here so the error surfaces
					// at plan time instead.
					int64validator.AtLeast(800),
				},
				MarkdownDescription: "Bandwidth provisioned for the pool, in megabytes per second (MB/s). " +
					"Must be at least 800. Can be changed in place, without replacing the pool.",
			},
			"tags": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "A mapping of tags to assign to the pool. Can be changed in place. Some " +
					"environments append their own tags outside Terraform (e.g. an Azure Policy); including " +
					"those tags in this configuration, or excluding them via a `lifecycle.ignore_changes` " +
					"block, avoids a persistent diff against them.",
			},
			"internal_id": schema.StringAttribute{
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "Everpure's internal ID of the storage pool.",
			},
			"provisioned_iops": schema.Int64Attribute{
				Computed:      true,
				PlanModifiers: []planmodifier.Int64{useStateWhenSiblingUnchanged(path.Root("provisioned_bandwidth_in_mbps"))},
				MarkdownDescription: "Total I/O operations per second (IOPS) provisioned for the pool. " +
					"Derived from `provisioned_bandwidth_in_mbps`; shown as known only after apply whenever " +
					"the pool is created or updated.",
			},
			"data_retention_period_in_seconds": schema.Int64Attribute{
				Computed:      true,
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
				MarkdownDescription: "How long a destroyed volume group, volume or snapshot is kept before being " +
					"eradicated, in seconds.",
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeoutsBlock(ctx, "the pool", timeoutsDefaults{
				Create: defaultStoragePoolCreateTimeout,
				Read:   defaultStoragePoolReadTimeout,
				Update: defaultStoragePoolUpdateTimeout,
				Delete: defaultStoragePoolDeleteTimeout,
			}, ""),
			"vnet_injection": schema.ListNestedBlock{
				MarkdownDescription: "The virtual network and subnet the pool is injected into. Exactly " +
					"one injection is supported today. Changing this forces a new pool to be created.",
				Validators: []validator.List{
					listvalidator.IsRequired(),
					listvalidator.SizeAtMost(1),
				},
				PlanModifiers: []planmodifier.List{
					listplanmodifier.RequiresReplace(),
				},
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"subnet_id": schema.StringAttribute{
							Required:   true,
							CustomType: armids.IDType{},
							PlanModifiers: []planmodifier.String{
								armids.PreserveEquivalentIDSpelling(),
							},
							Validators: []validator.String{
								armids.TypeSubnet.Validator(),
							},
							MarkdownDescription: "Full Azure Resource Manager ID of the delegated subnet " +
								"the pool's network interfaces are placed in.",
						},
						"virtual_network_id": schema.StringAttribute{
							Computed:   true,
							CustomType: armids.IDType{},
							PlanModifiers: []planmodifier.String{
								stringplanmodifier.UseStateForUnknown(),
							},
							MarkdownDescription: "Full Azure Resource Manager ID of the virtual network " +
								"`subnet_id` belongs to. Derived from `subnet_id`.",
						},
					},
				},
			},
			"platform_console": schema.ListNestedBlock{
				MarkdownDescription: "Platform console access (GUI, REST API and CLI) from the injected " +
					"subnet. Remove this block to disable console access entirely; when present, at " +
					"least one of `gui_enabled`, `api_enabled` or `cli_enabled` must be explicitly set " +
					"to `true`. Can be changed in place.",
				Validators: []validator.List{
					listvalidator.SizeAtMost(1),
				},
				PlanModifiers: []planmodifier.List{
					platformConsoleKeepComputed(),
				},
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"gui_enabled": schema.BoolAttribute{
							Optional: true,
							Computed: true,
							Default:  booldefault.StaticBool(false),
							MarkdownDescription: "Whether GUI access is enabled. Must equal `api_enabled`. Defaults " +
								"to `false`, along with `api_enabled` and `cli_enabled`: an otherwise-empty " +
								"`platform_console` block enables no service, and is rejected — at least one " +
								"must be set to `true`.",
						},
						"api_enabled": schema.BoolAttribute{
							Optional: true,
							Computed: true,
							Default:  booldefault.StaticBool(false),
							MarkdownDescription: "Whether REST API access is enabled. Must equal `gui_enabled`. " +
								"Defaults to `false` for the same reason as `gui_enabled`.",
						},
						"cli_enabled": schema.BoolAttribute{
							Optional: true,
							Computed: true,
							Default:  booldefault.StaticBool(false),
							MarkdownDescription: "Whether CLI (SSH) access is enabled. Defaults to `false` for the " +
								"same reason as `gui_enabled` and `api_enabled`: at least one service must be " +
								"enabled whenever `platform_console` is present, so leaving all three unset is " +
								"rejected rather than silently enabling every service.",
						},
						"subnet_ids": schema.ListAttribute{
							Computed:            true,
							ElementType:         types.StringType,
							MarkdownDescription: "Subnets enabled for console access. Always the `vnet_injection` subnet.",
						},
						"default_username": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Default username for console access, system-populated.",
						},
						"subnet": schema.ListNestedAttribute{
							Computed:            true,
							MarkdownDescription: "Per-subnet console access details, system-populated.",
							NestedObject: schema.NestedAttributeObject{
								Attributes: map[string]schema.Attribute{
									"id": schema.StringAttribute{
										Computed:            true,
										MarkdownDescription: "Full Azure Resource Manager ID of the subnet.",
									},
									"management_ip_address": schema.StringAttribute{
										Computed:            true,
										MarkdownDescription: "Management IP address assigned to the subnet.",
									},
									"service_backend_ip_addresses": schema.ListAttribute{
										Computed:            true,
										ElementType:         types.StringType,
										MarkdownDescription: "Service backend IP addresses assigned to the subnet.",
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// ValidateConfig enforces the cross-attribute rules the backend imposes on
// platform console, at config-validate time (before any API call). Its
// gui/api/cli attributes default to false when omitted, so a null value
// here is resolved to its eventual default before checking — this lets
// these checks run without waiting on a plan.
func (r *StoragePoolResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var data StoragePoolResourceModel
	if resp.Diagnostics.Append(req.Config.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	// A dynamic block whose for_each isn't statically resolvable makes
	// Terraform core report the whole platform_console list as unknown during
	// config validation, even once the for_each value itself is known. There
	// is nothing to check yet in that case: fail open here and let this run
	// again — with the list known — on the next validate/plan.
	if data.PlatformConsole.IsUnknown() {
		return
	}

	platformConsole, diags := blockElements[PlatformConsoleModel](ctx, data.PlatformConsole)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	if len(platformConsole) == 1 {
		pc := platformConsole[0]
		gui := effectiveOrDefaultFalse(pc.GUIEnabled)
		api := effectiveOrDefaultFalse(pc.APIEnabled)
		cli := effectiveOrDefaultFalse(pc.CLIEnabled)

		if gui != nil && api != nil && *gui != *api {
			resp.Diagnostics.Append(diag.WithPath(path.Root("platform_console").AtListIndex(0).AtName("gui_enabled"), invalidAttributeCombinationDiagnostic(newConsoleGUIAPIMismatchError())))
		}
		if gui != nil && api != nil && cli != nil && !*gui && !*api && !*cli {
			resp.Diagnostics.Append(diag.WithPath(path.Root("platform_console").AtListIndex(0).AtName("cli_enabled"), invalidAttributeCombinationDiagnostic(newConsoleServiceRequiredError())))
		}
	}
}

// effectiveOrDefaultFalse resolves a platform console bool the way Terraform
// will once defaults are applied: an unset value defaults to false, so it is
// safe to check as such at config time; an unknown value can't be checked
// yet, so it is left out of the comparison (nil).
func effectiveOrDefaultFalse(v types.Bool) *bool {
	if v.IsUnknown() {
		return nil
	}
	if v.IsNull() {
		f := false
		return &f
	}
	b := v.ValueBool()
	return &b
}

// ModifyPlan derives vnet_injection.virtual_network_id and
// platform_console.subnet_ids from vnet_injection.subnet_id, at plan time so
// they show as known whenever the subnet ID itself is known. Both are
// skipped on a destroy plan, which has no planned values to derive into.
func (r *StoragePoolResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}

	var plan StoragePoolResourceModel
	if resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...); resp.Diagnostics.HasError() {
		return
	}

	// A dynamic block whose for_each isn't statically resolvable makes
	// Terraform core report the whole vnet_injection list as unknown here,
	// even once the for_each value itself is known. There is nothing to
	// derive yet in that case: apply always recomputes the plan with fully
	// known values first, so this modifier simply runs again then.
	if plan.VnetInjection.IsUnknown() {
		return
	}

	vnetInjection, diags := blockElements[VnetInjectionModel](ctx, plan.VnetInjection)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}
	if len(vnetInjection) != 1 {
		return
	}
	subnetID := vnetInjection[0].SubnetID
	if subnetID.IsUnknown() || subnetID.IsNull() {
		return
	}

	subnet, err := armids.TypeSubnet.Parse(subnetID.ValueString())
	if err != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("vnet_injection").AtListIndex(0).AtName("subnet_id"), invalidAttributeValueDiagnostic(idErrorCause(err))))
		return
	}
	vnetID, err := subnet.ParentID()
	if err != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("vnet_injection").AtListIndex(0).AtName("subnet_id"), invalidAttributeValueDiagnostic(idErrorCause(err))))
		return
	}
	vnetInjection[0].VirtualNetworkID = armids.IDValue{StringValue: types.StringValue(vnetID)}

	newVnetInjection, d := types.ListValueFrom(ctx, vnetInjectionObjectType, vnetInjection)
	resp.Diagnostics.Append(d...)
	plan.VnetInjection = newVnetInjection

	// Same dynamic-block-for_each unknown as above can apply independently to
	// platform_console.
	if !plan.PlatformConsole.IsUnknown() {
		platformConsole, diags := blockElements[PlatformConsoleModel](ctx, plan.PlatformConsole)
		resp.Diagnostics.Append(diags...)
		if len(platformConsole) == 1 {
			subnetIDs, d := types.ListValueFrom(ctx, types.StringType, []string{subnetID.ValueString()})
			resp.Diagnostics.Append(d...)
			platformConsole[0].SubnetIDs = subnetIDs

			newPlatformConsole, d := types.ListValueFrom(ctx, platformConsoleObjectType, platformConsole)
			resp.Diagnostics.Append(d...)
			plan.PlatformConsole = newPlatformConsole
		}
	}

	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

// requestedID composes the pool's ID from the configured components, for the
// state written before the API has returned an ID of its own.
func (r *StoragePoolResource) requestedID(data StoragePoolResourceModel) string {
	return armids.StoragePoolID{
		ResourceGroupID: armids.ResourceGroupID{SubscriptionID: r.clients.subscriptionID, ResourceGroupName: data.ResourceGroupName.ValueString()},
		StoragePoolName: data.Name.ValueString(),
	}.FullID()
}

// checkStateSubscription reports, on id, a state ID in another subscription than the
// provider's, and whether op may go on. Read, Update and Delete address the pool by
// resource_group_name and name in the provider's subscription, so only the state's id
// says which subscription the pool was created in. An empty id, as a failed create
// leaves, has no subscription to check.
func (r *StoragePoolResource) checkStateSubscription(op operation, id armids.IDValue, diags *diag.Diagnostics) bool {
	if id.ValueString() == "" {
		return true
	}
	pool, err := armids.ParseStoragePoolID(id.ValueString())
	if err != nil {
		diags.Append(diag.WithPath(path.Root("id"), op.fail(newInvalidStoredIDError(storedInState, idErrorCause(err)))))
		return false
	}
	if c := subscriptionMismatchCause(pool, kindStoragePool, r.clients.subscriptionID); c != nil {
		diags.Append(diag.WithPath(path.Root("id"), op.fail(*c)))
		return false
	}
	return true
}

func (r *StoragePoolResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	client, ok := r.storagePoolsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data StoragePoolResourceModel
	if resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	createTimeout, diags := data.Timeouts.Create(ctx, defaultStoragePoolCreateTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()

	pool, diags := toStoragePool(ctx, data)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	op := operation{action: actionCreate, kind: kindStoragePool, name: data.Name.ValueString(), id: r.requestedID(data)}
	existing, err := client.Get(ctx, data.ResourceGroupName.ValueString(), data.Name.ValueString(), nil)
	var existingState string
	if err == nil && existing.Properties != nil {
		existingState = provisioningStateName(existing.Properties.ProvisioningState)
	}
	if d := r.clients.existingObjectDiagnostic(op, "everpure_azure_storage_pool", existing.ID, existingState, err); d != nil {
		resp.Diagnostics.Append(d)
		return
	}

	poller, err := client.BeginCreate(ctx, data.ResourceGroupName.ValueString(), data.Name.ValueString(), pool, nil)
	if err != nil {
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	// Save minimal state as soon as the create is accepted, so a failed poll
	// leaves a tainted resource Terraform knows about instead of an orphan.
	// The id is set from the requested components so the tainted state carries an
	// id and an identity that agree; the final pair below comes from the API's id.
	data.ID = armids.IDValue{StringValue: types.StringValue(r.requestedID(data))}
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, data.ID.ValueString(), armids.ParseStoragePoolID)...)
	resp.Diagnostics.Append(setPartialState(ctx, &resp.State, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	result, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	resp.Diagnostics.Append(applyStoragePool(ctx, &data, result.StoragePool)...)
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, data.ID.ValueString(), armids.ParseStoragePoolID)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *StoragePoolResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	client, ok := r.storagePoolsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data StoragePoolResourceModel
	if resp.Diagnostics.Append(req.State.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	readTimeout, diags := data.Timeouts.Read(ctx, defaultStoragePoolReadTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	op := operation{action: actionRetrieve, kind: kindStoragePool, name: data.Name.ValueString(), id: r.requestedID(data)}
	if !r.checkStateSubscription(op, data.ID, &resp.Diagnostics) {
		return
	}
	result, err := client.Get(ctx, data.ResourceGroupName.ValueString(), data.Name.ValueString(), nil)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(setIdentityFromIDIfNull(ctx, resp.Identity, idOrFallback(data.ID, r.requestedID(data)), armids.ParseStoragePoolID)...)
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	// The identity comes from the id already in state, read before applyStoragePool
	// overwrites it with the API's spelling. The id is null in a tainted state from a
	// failed create; the API's id then becomes the state id, so the identity follows it.
	identityID := data.ID.ValueString()
	resp.Diagnostics.Append(applyStoragePoolRead(ctx, &data, result.StoragePool)...)
	if identityID == "" {
		identityID = data.ID.ValueString()
	}
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, identityID, armids.ParseStoragePoolID)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *StoragePoolResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data StoragePoolResourceModel
	if resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	var priorState StoragePoolResourceModel
	if resp.Diagnostics.Append(req.State.Get(ctx, &priorState)...); resp.Diagnostics.HasError() {
		return
	}

	// An in-place update never changes the id, so the identity is carried over
	// from state unchanged.
	resp.Diagnostics.Append(setIdentityFromID(ctx, resp.Identity, priorState.ID.ValueString(), armids.ParseStoragePoolID)...)

	// timeouts is provider-only config: the API has never heard of it, so a
	// plan that changes only it (or otherwise leaves every API-backed
	// attribute below unchanged from state) has nothing for a PATCH to do. The
	// PATCH body built from the plan would just restate the pool's current
	// values back at it — a no-op round trip — so skip building a client and
	// calling the API entirely and persist the plan as-is.
	if data.ProvisionedBandwidthInMbps.Equal(priorState.ProvisionedBandwidthInMbps) &&
		data.Tags.Equal(priorState.Tags) &&
		data.PlatformConsole.Equal(priorState.PlatformConsole) {
		// provisioned_iops carries no UseStateForUnknown modifier, since on a
		// real PATCH round trip it is always refreshed from the API response
		// instead (it is derived from provisioned_bandwidth_in_mbps server-side,
		// not computed locally); skipping that round trip here leaves it
		// unknown in the plan unless it's carried forward explicitly, which
		// Terraform Core rejects as an invalid post-apply value. Copying it
		// from state is safe here specifically because
		// provisioned_bandwidth_in_mbps, the only thing it is derived from,
		// was just confirmed unchanged above.
		data.ProvisionedIops = priorState.ProvisionedIops
		resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
		return
	}

	client, ok := r.storagePoolsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	updateTimeout, diags := data.Timeouts.Update(ctx, defaultStoragePoolUpdateTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()

	vnetInjection, diags := blockElements[VnetInjectionModel](ctx, data.VnetInjection)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}
	op := operation{action: actionUpdate, kind: kindStoragePool, name: data.Name.ValueString(), id: r.requestedID(data)}
	if !r.checkStateSubscription(op, priorState.ID, &resp.Diagnostics) {
		return
	}
	if len(vnetInjection) != 1 {
		resp.Diagnostics.Append(providerInternalDiagnostic(newProviderBugError("vnet_injection missing from the plan")))
		return
	}

	update, diags := toStoragePoolUpdate(ctx, data, priorState.PlatformConsole, vnetInjection[0].SubnetID.ValueString())
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	poller, err := client.BeginUpdate(ctx, data.ResourceGroupName.ValueString(), data.Name.ValueString(), update, nil)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(op.fail(newDeletedOutsideTerraformError(kindStoragePool, op.id)))
			return
		}
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	result, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(op.fail(newDeletedOutsideTerraformError(kindStoragePool, op.id)))
			return
		}
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	resp.Diagnostics.Append(applyStoragePool(ctx, &data, result.StoragePool)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *StoragePoolResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	client, ok := r.storagePoolsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data StoragePoolResourceModel
	if resp.Diagnostics.Append(req.State.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	deleteTimeout, diags := data.Timeouts.Delete(ctx, defaultStoragePoolDeleteTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()

	op := operation{action: actionDelete, kind: kindStoragePool, name: data.Name.ValueString(), id: r.requestedID(data)}
	if !r.checkStateSubscription(op, data.ID, &resp.Diagnostics) {
		return
	}
	addDeleteError := func(err error) {
		if isNotFound(err) {
			return
		}
		f := r.clients.apiFailure(err)
		switch classifyStoragePoolDelete(err) {
		case storagePoolDeleteHasChildren:
			resp.Diagnostics.Append(op.fail(newStoragePoolHasChildrenError(op.id, f)))
		case storagePoolDeleteContainsData:
			resp.Diagnostics.Append(op.fail(newStoragePoolContainsDataError(op.id, data.DataRetentionPeriodInSeconds.ValueInt64(), f)))
		default:
			resp.Diagnostics.Append(op.fail(newApiCallFailedError(op, f)))
		}
	}

	poller, err := client.BeginDelete(ctx, data.ResourceGroupName.ValueString(), data.Name.ValueString(), nil)
	if err != nil {
		addDeleteError(err)
		return
	}

	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		addDeleteError(err)
	}
}

func (r *StoragePoolResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if r.clients == nil {
		resp.Diagnostics.Append(operationConfigureProvider.fail(newProviderNotConfiguredError()))
		return
	}
	expectedSubscriptionID := r.clients.subscriptionID

	importID, diags := resolveImportID(ctx, req, kindStoragePool, armids.StoragePoolID.FullID, armids.ParseStoragePoolID)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}

	importOp := operation{action: actionImport, kind: kindStoragePool, id: importID, unnamed: true}

	parsed, idErr := parseIDInSubscription(armids.ParseStoragePoolID, importID, kindStoragePool, expectedSubscriptionID)
	if idErr != nil {
		resp.Diagnostics.Append(importOp.fail(*idErr))
		return
	}

	resp.Diagnostics.Append(resp.Identity.Set(ctx, parsed)...)
	resp.Diagnostics.Append(seedStoragePoolState(ctx, &resp.State, importID, parsed)...)
}

// MoveState takes over an azapi_resource of type PureStorage.Block/storagePools.
func (r *StoragePoolResource) MoveState(context.Context) []resource.StateMover {
	return resourceMove[armids.StoragePoolID]{
		kind: kindStoragePool, sources: []moveSource{azapiResourceSource}, parse: armids.ParseStoragePoolID, seed: seedStoragePoolState,
	}.stateMovers()
}
