// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	atimeouts "github.com/hashicorp/terraform-plugin-framework-timeouts/action/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

var (
	_ action.Action              = &CreatePlatformConsoleSSHUserAction{}
	_ action.ActionWithConfigure = &CreatePlatformConsoleSSHUserAction{}
)

// defaultPlatformConsoleSSHUserInvokeTimeout matches the pool's read timeout. The
// service makes several round-trips to the pool's array to create the user and set its
// key and sets no deadline of its own on the call, so the provider's deadline is the
// only ceiling.
const defaultPlatformConsoleSSHUserInvokeTimeout = defaultStoragePoolReadTimeout

// CreatePlatformConsoleSSHUserActionModel is the Terraform-side representation of the
// everpure_azure_create_platform_console_ssh_user action's configuration.
type CreatePlatformConsoleSSHUserActionModel struct {
	StoragePoolID armids.IDValue  `tfsdk:"storage_pool_id"`
	Username      types.String    `tfsdk:"username"`
	Role          types.String    `tfsdk:"role"`
	PublicKey     types.String    `tfsdk:"public_key"`
	Timeouts      atimeouts.Value `tfsdk:"timeouts"`
}

// CreatePlatformConsoleSSHUserAction creates a user on a storage pool's platform console
// and sets its SSH public key. It is an action rather than a resource because the API
// can only create: it has no call to read the user back, change its key or role, or
// remove it, so a resource would have no state it could verify and nothing to destroy.
type CreatePlatformConsoleSSHUserAction struct {
	// actionClientsHolder supplies Configure and storagePoolsClient via promotion; see
	// its own doc comment.
	actionClientsHolder
}

func NewCreatePlatformConsoleSSHUserAction() action.Action {
	return &CreatePlatformConsoleSSHUserAction{}
}

func (a *CreatePlatformConsoleSSHUserAction) Metadata(_ context.Context, req action.MetadataRequest, resp *action.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_create_platform_console_ssh_user"
}

func (a *CreatePlatformConsoleSSHUserAction) Schema(ctx context.Context, _ action.SchemaRequest, resp *action.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Creates a user on the platform console of an existing Storage Pool and sets its " +
			"SSH public key, so the user can log in to the console over SSH. Needs Terraform 1.14 or later, and 1.16 " +
			"or later to refer to the triggering resource with `caller` in an `action_trigger`.\n\n" +
			"~> **This action can be run only once per user.** It creates a new user, and any later run for " +
			"the same `username` fails with a 409 \"already exists\" error. That includes users that were not " +
			"created by this action, such as users created in the platform console and its built-in users. " +
			"Changing or removing the configuration does not change or remove a user that already exists. To " +
			"set an SSH key for an existing user, or to change a user's keys or role, use the platform " +
			"console's GUI or REST API.\n\n" +
			"Logging in to the platform console as the user this action creates and logging in with temporary " +
			"credentials are independent ways in; use either or both. For the temporary credentials, see the " +
			"`everpure_azure_platform_console_activation_credentials` ephemeral resource.\n\n" +
			"The Storage Pool must have the platform console enabled.",
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
			"username": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the user to create. Must not already exist on the Storage Pool; " +
					"the service rejects an empty name and any name already in use.",
			},
			"role": schema.StringAttribute{
				Required: true,
				Validators: []validator.String{
					stringvalidator.OneOf(
						string(armpurestorageblock.PlatformConsoleRoleArrayAdmin),
						string(armpurestorageblock.PlatformConsoleRoleReadOnly),
					),
				},
				MarkdownDescription: "Role the user gets on the platform console: `array_admin` (full " +
					"administrative access) or `read_only`. There is no default, so that administrative access " +
					"is never granted implicitly.",
			},
			"public_key": schema.StringAttribute{
				Required: true,
				Validators: []validator.String{
					sshPublicKeyValidator{},
				},
				MarkdownDescription: "SSH public key of the user, in OpenSSH one-line format " +
					"(`<type> <base64 key> [comment]`). To give the user several keys, put one per line. The value " +
					"is sent to the service unchanged, so `file()` of a `.pub` file or of an `authorized_keys` file " +
					"works. The provider only rejects values that are plainly not public keys: empty values, " +
					"private keys and PEM blocks, and lines without both a key type and key data. The service " +
					"decides which key types it accepts.",
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": actionTimeoutsBlock(ctx, "creating the user", defaultPlatformConsoleSSHUserInvokeTimeout),
		},
	}
}

func (a *CreatePlatformConsoleSSHUserAction) Invoke(ctx context.Context, req action.InvokeRequest, resp *action.InvokeResponse) {
	client, ok := a.storagePoolsClient(&resp.Diagnostics)
	if !ok {
		return
	}

	var data CreatePlatformConsoleSSHUserActionModel
	if resp.Diagnostics.Append(req.Config.Get(ctx, &data)...); resp.Diagnostics.HasError() {
		return
	}

	// The validator skips a key that is unknown at plan time (e.g. computed by another
	// resource in the same apply) and validators are not run again at invoke, so the key
	// is checked here too: otherwise a private key could reach the service unchecked.
	if keyErr := sshPublicKeyCause(data.PublicKey.ValueString()); keyErr != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("public_key"), invalidAttributeValueDiagnostic(*keyErr)))
		return
	}

	invokeTimeout, diags := data.Timeouts.Invoke(ctx, defaultPlatformConsoleSSHUserInvokeTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, invokeTimeout)
	defer cancel()

	poolID := data.StoragePoolID.ValueString()
	op := operation{action: actionCreate, kind: kindPlatformConsoleUser, name: data.Username.ValueString(), in: poolID}

	pool, idErr := parseIDInSubscription(armids.ParseStoragePoolID, poolID, kindStoragePool, a.clients.subscriptionID)
	if idErr != nil {
		resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(*idErr)))
		return
	}

	username := data.Username.ValueString()
	role := data.Role.ValueString()

	result, err := client.ConfigurePlatformConsoleAuth(
		withoutSDKRetries(ctx), pool.ResourceGroupName, pool.StoragePoolName,
		buildSSHPlatformConsoleAuthConfig(username, role, data.PublicKey.ValueString()), nil,
	)
	if err != nil {
		switch {
		case isNotFound(err):
			resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(newNotFoundError(kindStoragePool, poolID, nil, a.clients.apiFailure(err)))))
		case isPlatformConsoleDisabled(err):
			resp.Diagnostics.Append(diag.WithPath(path.Root("storage_pool_id"), op.fail(newPlatformConsoleDisabledError(poolID, a.clients.apiFailure(err)))))
		case isPlatformConsoleUserExists(err):
			resp.Diagnostics.Append(op.fail(newPlatformConsoleUserExistsError(username, a.clients.apiFailure(err))))
		default:
			resp.Diagnostics.Append(a.clients.apiCallError(op, err))
		}
		return
	}

	// The response echoes the request's username and role; it is read here only so the
	// progress message reports what the service confirmed. A response of another shape
	// falls back to the request's own values.
	createdUsername, createdRole := username, role
	if ssh, ok := result.PlatformConsoleAuthResultClassification.(*armpurestorageblock.SSHPlatformConsoleAuthResult); ok {
		if ssh.Username != nil {
			createdUsername = *ssh.Username
		}
		if ssh.Role != nil {
			createdRole = string(*ssh.Role)
		}
	}
	if resp.SendProgress != nil {
		resp.SendProgress(action.InvokeProgressEvent{
			Message: platformConsoleSSHUserProgressMessage(createdUsername, createdRole, poolID),
		})
	}
}

// buildSSHPlatformConsoleAuthConfig builds the request body. The SDK marshals the
// auth-type discriminator ("ssh") from the concrete type; the key is sent exactly as
// configured.
func buildSSHPlatformConsoleAuthConfig(username, role, publicKey string) *armpurestorageblock.SSHPlatformConsoleAuthConfig {
	return &armpurestorageblock.SSHPlatformConsoleAuthConfig{
		AuthType:  to.Ptr(armpurestorageblock.PlatformConsoleAuthTypeSSH),
		Username:  to.Ptr(username),
		Role:      to.Ptr(armpurestorageblock.PlatformConsoleRole(role)),
		PublicKey: to.Ptr(publicKey),
	}
}

func platformConsoleSSHUserProgressMessage(username, role, storagePoolID string) string {
	return fmt.Sprintf("Created platform console user %s (role %s) on storage pool %s", username, role, storagePoolID)
}

// withoutSDKRetries returns ctx with the SDK's retry policy switched off for the one
// call made with it (a negative MaxRetries means no retries; zero would mean the
// default of three). The call creates a user and is not idempotent: a retry after a
// lost success response, or after a server error that came after the user was
// created, would be answered "already exists" and hide what really happened, so one
// attempt must report the true outcome.
func withoutSDKRetries(ctx context.Context) context.Context {
	return policy.WithRetryOptions(ctx, policy.RetryOptions{MaxRetries: -1})
}

// sshPublicKeyValidator rejects only values that are obviously not OpenSSH public
// keys. It deliberately does not parse the key: the set of key types and options the
// platform console accepts is not documented, and refusing a key it would accept is
// worse than letting the service reject a bad one.
type sshPublicKeyValidator struct{}

var _ validator.String = sshPublicKeyValidator{}

func (sshPublicKeyValidator) Description(context.Context) string {
	return "value must be one or more OpenSSH public keys, one per line"
}

func (v sshPublicKeyValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (sshPublicKeyValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if keyErr := sshPublicKeyCause(req.ConfigValue.ValueString()); keyErr != nil {
		resp.Diagnostics.Append(diag.WithPath(req.Path, invalidAttributeValueDiagnostic(*keyErr)))
	}
}

// sshPublicKeyCause returns what is plainly wrong with value, or nil when nothing is.
// The cause never quotes the value: a rejected one may be a private key, so the
// diagnostic cannot end with "got: <value>" like a library validator's.
func sshPublicKeyCause(value string) *detailError {
	keys := 0
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "-----BEGIN") {
			c := newSshPublicKeyHasPEMBlockError()
			return &c
		}
		if len(strings.Fields(line)) < 2 {
			c := newSshPublicKeyLineWithoutKeyDataError()
			return &c
		}
		keys++
	}
	if keys == 0 {
		c := newSshPublicKeyMissingError()
		return &c
	}
	return nil
}
