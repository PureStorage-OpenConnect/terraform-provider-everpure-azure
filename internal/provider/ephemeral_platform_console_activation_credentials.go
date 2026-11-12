// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	etimeouts "github.com/hashicorp/terraform-plugin-framework-timeouts/ephemeral/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

var (
	_ ephemeral.EphemeralResource              = &PlatformConsoleActivationCredentialsEphemeralResource{}
	_ ephemeral.EphemeralResourceWithConfigure = &PlatformConsoleActivationCredentialsEphemeralResource{}
)

// defaultPlatformConsoleActivationCredentialsOpenTimeout matches the pool's read timeout. The
// service sets no deadline of its own on this call and makes several round-trips to
// the pool's array to mint the code, so the provider's deadline, which also bounds the
// SDK's retries, is the only ceiling.
const defaultPlatformConsoleActivationCredentialsOpenTimeout = defaultStoragePoolReadTimeout

// PlatformConsoleActivationCredentialsEphemeralResourceModel is the Terraform-side
// representation of the everpure_azure_platform_console_activation_credentials ephemeral
// resource.
type PlatformConsoleActivationCredentialsEphemeralResourceModel struct {
	StoragePoolID armids.IDValue  `tfsdk:"storage_pool_id"`
	Password      types.String    `tfsdk:"password"`
	Username      types.String    `tfsdk:"username"`
	ExpiresAt     types.String    `tfsdk:"expires_at"`
	Timeouts      etimeouts.Value `tfsdk:"timeouts"`
}

// PlatformConsoleActivationCredentialsEphemeralResource requests activation credentials for a
// storage pool's platform console. It only implements Open: the credentials cannot be
// renewed, and the API has no call to revoke them, so there is nothing for Renew or Close to do.
type PlatformConsoleActivationCredentialsEphemeralResource struct {
	// ephemeralClientsHolder supplies Configure and storagePoolsClient via promotion;
	// see its own doc comment.
	ephemeralClientsHolder
}

func NewPlatformConsoleActivationCredentialsEphemeralResource() ephemeral.EphemeralResource {
	return &PlatformConsoleActivationCredentialsEphemeralResource{}
}

func (r *PlatformConsoleActivationCredentialsEphemeralResource) Metadata(_ context.Context, req ephemeral.MetadataRequest, resp *ephemeral.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_platform_console_activation_credentials"
}

func (r *PlatformConsoleActivationCredentialsEphemeralResource) Schema(ctx context.Context, _ ephemeral.SchemaRequest, resp *ephemeral.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Requests temporary credentials for the platform console of a Storage Pool: a " +
			"`username` and an expirable `password`. Log in with them, create your own users, and " +
			"use those from then on. The Storage Pool must have the platform console enabled.\n\n" +
			"-> **This is meant for the initial setup of the platform console.** Use it only when you need " +
			"credentials: remove the block once the setup is done, keep it in a separate configuration, or use `count` " +
			"to skip it in other runs.\n\n" +
			"Every `terraform plan` and `terraform apply` that evaluates this resource requests new credentials and " +
			"invalidates the previous password, including plan-only and refresh runs. " +
			"The password is valid until `expires_at`. " +
			"Users you created yourself are never affected.\n\n" +
			"Requires Terraform 1.10 or later, and 1.11 or later to pass the password to a write-only attribute.",
		Attributes: map[string]schema.Attribute{
			"storage_pool_id": schema.StringAttribute{
				Required:   true,
				CustomType: armids.IDType{},
				Validators: []validator.String{
					armids.TypeStoragePool.Validator(),
				},
				MarkdownDescription: "Full Azure Resource Manager ID of the Storage Pool. Must be in the " +
					"provider's own `subscription_id`.",
			},
			"password": schema.StringAttribute{
				Computed:            true,
				Sensitive:           true,
				MarkdownDescription: "Expirable password for `username`, valid until `expires_at`.",
			},
			"username": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Name of the console user to log in as with the password.",
			},
			"expires_at": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "When the password expires, in UTC, formatted as RFC 3339 " +
					"(for example `2026-10-02T14:30:00Z`).",
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": ephemeralTimeoutsBlock(ctx, "the activation credentials", defaultPlatformConsoleActivationCredentialsOpenTimeout),
		},
	}
}

func (r *PlatformConsoleActivationCredentialsEphemeralResource) Open(ctx context.Context, req ephemeral.OpenRequest, resp *ephemeral.OpenResponse) {
	client, ok := r.storagePoolsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data PlatformConsoleActivationCredentialsEphemeralResourceModel
	if resp.Diagnostics.Append(req.Config.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	openTimeout, diags := data.Timeouts.Open(ctx, defaultPlatformConsoleActivationCredentialsOpenTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()

	poolID := data.StoragePoolID.ValueString()
	op := operation{action: actionRetrieve, kind: kindPlatformConsoleActivationCode, in: poolID, unnamed: true}

	pool, idErr := parseIDInSubscription(armids.ParseStoragePoolID, poolID, kindStoragePool, r.clients.subscriptionID)
	if idErr != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(*idErr)))
		return
	}

	code, err := client.ListPlatformConsoleActivationCode(ctx, pool.ResourceGroupName, pool.StoragePoolName, nil)
	if err != nil {
		if isNotFound(err) {
			resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(newNotFoundError(kindStoragePool, poolID, nil, r.clients.apiFailure(err)))))
			return
		}
		if isPlatformConsoleDisabled(err) {
			resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(newPlatformConsoleDisabledError(poolID, r.clients.apiFailure(err)))))
			return
		}
		resp.Diagnostics.Append(r.clients.apiCallError(op, err))
		return
	}

	applyPlatformConsoleActivationCredentials(&data, code.PlatformConsoleActivationCode)

	// storage_pool_id is left untouched from config: Terraform core rejects a result
	// that changes a configured, non-Computed value.
	resp.Diagnostics.Append(resp.Result.Set(ctx, &data)...)
}

// applyPlatformConsoleActivationCredentials maps the API response onto every computed field of
// data. Nil pointers map to null values, and the expiry is rendered in UTC so the
// value does not depend on the offset the service happened to answer with.
func applyPlatformConsoleActivationCredentials(data *PlatformConsoleActivationCredentialsEphemeralResourceModel, code armpurestorageblock.PlatformConsoleActivationCode) {
	data.Password = types.StringPointerValue(code.ActivationCode)
	data.Username = types.StringPointerValue(code.Username)
	if code.ExpiresAt == nil {
		data.ExpiresAt = types.StringNull()
		return
	}
	data.ExpiresAt = types.StringValue(code.ExpiresAt.UTC().Format(time.RFC3339))
}
