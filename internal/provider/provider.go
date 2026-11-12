// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azlog "github.com/Azure/azure-sdk-for-go/sdk/azcore/log"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/list"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

var (
	_ provider.Provider                  = &EverpureAzureProvider{}
	_ provider.ProviderWithListResources = &EverpureAzureProvider{}

	_ provider.ProviderWithEphemeralResources = &EverpureAzureProvider{}
	_ provider.ProviderWithActions            = &EverpureAzureProvider{}
)

type clientOptionsHook func(*arm.ClientOptions)

var clientOptionsHooks []clientOptionsHook

// credentialFactory builds this run's Azure credential from its resolved
// auth attributes. Production always uses buildCredential (auth.go); tests
// inject a factory that returns a fake credential, ignoring authConfig, so
// they can drive the fake ARM transport without a real credential chain.
type credentialFactory func(ctx context.Context, cfg authConfig, opts *arm.ClientOptions) (azcore.TokenCredential, error)

type EverpureAzureProvider struct {
	version string

	// correlationID identifies every ARM call this provider instance makes,
	// across create/read/update/delete and every LRO poll, so a customer can
	// quote the one value from a failed run's error output and support finds
	// every server-side log line for that run, not just its last call.
	correlationID string

	// credentialFactory is nil in production, where Configure falls back to
	// buildCredential; tests set it directly.
	credentialFactory credentialFactory
	// clientOptionsOverride is a test-only override, wired to
	// armpurestorageblock/fake's in-memory transport. Left nil in
	// production, where Configure starts from a bare ClientOptions.
	clientOptionsOverride *arm.ClientOptions
}

type providerModel struct {
	SubscriptionID types.String `tfsdk:"subscription_id"`

	TenantID                     types.String `tfsdk:"tenant_id"`
	ClientID                     types.String `tfsdk:"client_id"`
	ClientIDFilePath             types.String `tfsdk:"client_id_file_path"`
	ClientSecret                 types.String `tfsdk:"client_secret"`
	ClientSecretFilePath         types.String `tfsdk:"client_secret_file_path"`
	ClientCertificate            types.String `tfsdk:"client_certificate"`
	ClientCertificatePath        types.String `tfsdk:"client_certificate_path"`
	ClientCertificatePassword    types.String `tfsdk:"client_certificate_password"`
	UseOIDC                      types.Bool   `tfsdk:"use_oidc"`
	OIDCToken                    types.String `tfsdk:"oidc_token"`
	OIDCTokenFilePath            types.String `tfsdk:"oidc_token_file_path"`
	OIDCRequestToken             types.String `tfsdk:"oidc_request_token"`
	OIDCRequestURL               types.String `tfsdk:"oidc_request_url"`
	OIDCAzureServiceConnectionID types.String `tfsdk:"oidc_azure_service_connection_id"`
	UseCLI                       types.Bool   `tfsdk:"use_cli"`
	UseMSI                       types.Bool   `tfsdk:"use_msi"`
	UseAKSWorkloadIdentity       types.Bool   `tfsdk:"use_aks_workload_identity"`
	AuxiliaryTenantIDs           types.List   `tfsdk:"auxiliary_tenant_ids"`
}

// hasUnknownAuthAttribute reports whether any authentication attribute is
// still unknown (e.g. it depends on a resource not yet applied). Configure
// must treat that as "not resolvable yet" and defer to a later call, exactly
// as it already does for subscription_id via resolveSetting's own unknown
// status. auxiliary_tenant_ids needs its own element-by-element check on top
// of the list-level one: the list itself can be known while one of its
// elements still isn't.
func (m providerModel) hasUnknownAuthAttribute() bool {
	if m.TenantID.IsUnknown() ||
		m.ClientID.IsUnknown() ||
		m.ClientIDFilePath.IsUnknown() ||
		m.ClientSecret.IsUnknown() ||
		m.ClientSecretFilePath.IsUnknown() ||
		m.ClientCertificate.IsUnknown() ||
		m.ClientCertificatePath.IsUnknown() ||
		m.ClientCertificatePassword.IsUnknown() ||
		m.UseOIDC.IsUnknown() ||
		m.OIDCToken.IsUnknown() ||
		m.OIDCTokenFilePath.IsUnknown() ||
		m.OIDCRequestToken.IsUnknown() ||
		m.OIDCRequestURL.IsUnknown() ||
		m.OIDCAzureServiceConnectionID.IsUnknown() ||
		m.UseCLI.IsUnknown() ||
		m.UseMSI.IsUnknown() ||
		m.UseAKSWorkloadIdentity.IsUnknown() ||
		m.AuxiliaryTenantIDs.IsUnknown() {
		return true
	}
	for _, e := range m.AuxiliaryTenantIDs.Elements() {
		if s, ok := e.(types.String); ok && s.IsUnknown() {
			return true
		}
	}
	return false
}

func New(version string) func() provider.Provider {
	return newProvider(version, nil, nil)
}

// newProvider is the test seam: it lets tests inject a credentialFactory and
// arm.ClientOptions (typically wired to armpurestorageblock/fake) instead of
// the production credential chain. Production code always goes through New,
// which passes nil for both; a nil factory means Configure uses
// buildCredential.
func newProvider(version string, factory credentialFactory, opts *arm.ClientOptions) func() provider.Provider {
	return func() provider.Provider {
		return &EverpureAzureProvider{
			version:               version,
			correlationID:         uuid.NewString(),
			credentialFactory:     factory,
			clientOptionsOverride: opts,
		}
	}
}

func (p *EverpureAzureProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "everpure_azure"
	resp.Version = p.version
}

func (p *EverpureAzureProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages Everpure resources on Azure through the " +
			"`PureStorage.Block` Azure Resource Manager API.",
		Attributes: map[string]schema.Attribute{
			"subscription_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "ID of the subscription resources are managed in. Falls back to the " +
					"`ARM_SUBSCRIPTION_ID` then `AZURE_SUBSCRIPTION_ID` environment variables when unset. " +
					"One of the three must resolve to a value.",
			},
			"tenant_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The Tenant ID which should be used. This can also be sourced from the " +
					"`ARM_TENANT_ID` or `AZURE_TENANT_ID` environment variables.",
			},
			"client_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The Client ID which should be used. This can also be sourced from the " +
					"`ARM_CLIENT_ID` or `AZURE_CLIENT_ID` environment variables. When `use_msi` is `true`, this is " +
					"the client ID of the user-assigned identity to use, rather than the VM's system-assigned one.",
			},
			"client_id_file_path": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Path to a file containing the Client ID which should be used. This can also " +
					"be sourced from the `ARM_CLIENT_ID_FILE_PATH` environment variable.",
			},
			"client_secret": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "The Client Secret which should be used, for authenticating as a service " +
					"principal. This can also be sourced from the `ARM_CLIENT_SECRET` or `AZURE_CLIENT_SECRET` " +
					"environment variables.",
			},
			"client_secret_file_path": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Path to a file containing the Client Secret which should be used. This can " +
					"also be sourced from the `ARM_CLIENT_SECRET_FILE_PATH` environment variable.",
			},
			"client_certificate": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Base64-encoded client certificate, for authenticating as a service " +
					"principal: either a PKCS#12 (.pfx/.p12) bundle, or a PEM file holding the certificate and its " +
					"unencrypted private key. This can also be sourced from the `ARM_CLIENT_CERTIFICATE` environment " +
					"variable.",
			},
			"client_certificate_path": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Path to a client certificate: either a PKCS#12 (.pfx/.p12) bundle, or a PEM " +
					"file holding the certificate and its unencrypted private key. This can also be sourced from the " +
					"`ARM_CLIENT_CERTIFICATE_PATH` or `AZURE_CLIENT_CERTIFICATE_PATH` environment variables.",
			},
			"client_certificate_password": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Password for a PKCS#12 bundle given in `client_certificate` or " +
					"`client_certificate_path`; a PEM private key must be unencrypted. This can also be sourced from the " +
					"`ARM_CLIENT_CERTIFICATE_PASSWORD` or `AZURE_CLIENT_CERTIFICATE_PASSWORD` environment " +
					"variables.",
			},
			"use_oidc": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Allow OpenID Connect to be used for authentication. This can also be " +
					"sourced from the `ARM_USE_OIDC` environment variable. Defaults to `false`.",
			},
			"oidc_token": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "The OIDC token to use for authentication. This can also be sourced from " +
					"the `ARM_OIDC_TOKEN` environment variable.",
			},
			"oidc_token_file_path": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Path to a file containing an OIDC token to use for authentication. This " +
					"can also be sourced from the `ARM_OIDC_TOKEN_FILE_PATH` environment variable, or from " +
					"`AZURE_FEDERATED_TOKEN_FILE` when `use_aks_workload_identity` is `true`.",
			},
			"oidc_request_token": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "The bearer token to present when requesting an OIDC token, for GitHub " +
					"Actions or Azure Pipelines. This can also be sourced from the `ARM_OIDC_REQUEST_TOKEN`, " +
					"`ACTIONS_ID_TOKEN_REQUEST_TOKEN` or `SYSTEM_ACCESSTOKEN` environment variables.",
			},
			"oidc_request_url": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The URL to request an OIDC token from, for GitHub Actions. This can also " +
					"be sourced from the `ARM_OIDC_REQUEST_URL` or `ACTIONS_ID_TOKEN_REQUEST_URL` environment " +
					"variables.",
			},
			"oidc_azure_service_connection_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The Azure Pipelines service connection ID to request an OIDC token " +
					"through. This can also be sourced from the `ARM_ADO_PIPELINE_SERVICE_CONNECTION_ID`, " +
					"`ARM_OIDC_AZURE_SERVICE_CONNECTION_ID` or `AZURESUBSCRIPTION_SERVICE_CONNECTION_ID` " +
					"environment variables. Its presence selects the Azure Pipelines OIDC flow over the GitHub " +
					"Actions one. `oidc_request_token` (or `ARM_OIDC_REQUEST_TOKEN` / `SYSTEM_ACCESSTOKEN`) must " +
					"also resolve to a value; the service connection ID alone is not enough.",
			},
			"use_cli": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Allow Azure CLI to be used for authentication. This can also be sourced " +
					"from the `ARM_USE_CLI` environment variable. Defaults to `true`.",
			},
			"use_msi": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Allow Managed Identity to be used for authentication. This can also be " +
					"sourced from the `ARM_USE_MSI` environment variable. Defaults to `false`.",
			},
			"use_aks_workload_identity": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Allow Azure AKS Workload Identity to be used for authentication. This can " +
					"also be sourced from the `ARM_USE_AKS_WORKLOAD_IDENTITY` environment variable. Defaults to " +
					"`false`. When `true`, `client_id`, `tenant_id` and `oidc_token_file_path` default from the " +
					"`AZURE_CLIENT_ID`, `AZURE_TENANT_ID` and `AZURE_FEDERATED_TOKEN_FILE` environment variables " +
					"the AKS pod identity webhook sets, and a `tenant_id` that disagrees with `AZURE_TENANT_ID` " +
					"is an error.",
			},
			"auxiliary_tenant_ids": schema.ListAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "A list of the Tenant IDs of Microsoft Entra ID tenants other than the " +
					"one specified in `tenant_id`, for multi-tenancy. This can also be sourced from the " +
					"`ARM_AUXILIARY_TENANT_IDS` environment variable as a `;`-separated list. An empty list is " +
					"treated the same as omitting the attribute and still falls back to the environment variable.",
			},
		},
	}
}

// Clients hands out ARM clients to resources, scoped to the subscription
// resolved at Configure time. Credentials come from resolveAuth and
// buildCredential, unless a test overrides them through newProvider.
type Clients struct {
	cred           azcore.TokenCredential
	opts           *arm.ClientOptions
	subscriptionID string

	// correlationID is this run's value, mirrored from the provider so the
	// error helpers in errors.go can quote it without a separate lookup.
	correlationID string
}

// correlationRequestIDPolicy stamps every outgoing ARM request with the
// run's correlation ID. It runs ahead of the retry policy in the pipeline,
// so a retried call and every poll of a single LRO carry the same value as
// the original request, and error output can quote it before any response
// (or none, on a transport failure) comes back.
type correlationRequestIDPolicy struct {
	id string
}

func (p *correlationRequestIDPolicy) Do(req *policy.Request) (*http.Response, error) {
	req.Raw().Header.Set("X-Ms-Correlation-Request-Id", p.id)
	return req.Next()
}

// userAgentPolicy puts the provider's identification in front of the
// User-Agent the request already carries.
//
// azcore's pipeline runs its telemetry policy before PerCallPolicies. The
// telemetry policy sets User-Agent to "azsdk-go-<module>/<ver> (<go>; <os>)",
// so when this policy runs the header already holds that SDK string, and
// prepending the provider's own string keeps the SDK string after it. The
// HasPrefix check makes a second pass through this policy a no-op.
// ClientOptions.Telemetry.ApplicationID is not used: azcore truncates it to 24
// characters, too short for a dev version string, and places it after the SDK
// string rather than first.
type userAgentPolicy struct {
	prefix string
}

// userAgentAppendEnv names the environment variable whose value is appended to
// the provider's User-Agent string. A CI job sets it to something that
// identifies the run, such as a pipeline name, so the run's requests can be
// told apart in Azure's request logs. azurerm honours the same variable.
const userAgentAppendEnv = "TF_APPEND_USER_AGENT"

// userAgentAppend returns the trimmed value of TF_APPEND_USER_AGENT. It is the
// only place the variable is read; the policy receives the value as a parameter.
func userAgentAppend() string {
	return strings.TrimSpace(os.Getenv(userAgentAppendEnv))
}

// newUserAgentPolicy builds the policy's prefix:
// "terraform-provider-everpure-azure/<v> Terraform/<tf> <appended>", where the
// Terraform and appended parts are left out when empty.
func newUserAgentPolicy(providerVersion, terraformVersion, appended string) *userAgentPolicy {
	prefix := "terraform-provider-everpure-azure/" + providerVersion
	if terraformVersion != "" {
		prefix += " Terraform/" + terraformVersion
	}
	if appended != "" {
		prefix += " " + appended
	}
	return &userAgentPolicy{prefix: prefix}
}

func (p *userAgentPolicy) Do(req *policy.Request) (*http.Response, error) {
	h := req.Raw().Header
	ua := h.Get("User-Agent")
	switch {
	case strings.HasPrefix(ua, p.prefix):
	case ua == "":
		h.Set("User-Agent", p.prefix)
	default:
		h.Set("User-Agent", p.prefix+" "+ua)
	}
	return req.Next()
}

var (
	sdkLogOnce sync.Once
	// sdkLogCtx is the context of the latest Configure call. The azcore
	// listener is installed once but must log through a live context, so it
	// reads the context from here.
	sdkLogCtx atomic.Pointer[context.Context]
)

// installSDKLogging forwards azcore's HTTP log events to tflog at debug level.
//
// azcore keeps one process-wide listener and writes it without any locking,
// neither in SetListener nor when events are emitted, so replacing it from a
// second Configure while requests are in flight would be a data race. The
// listener is therefore installed exactly once; later Configure calls only
// swap the context it logs through.
func installSDKLogging(ctx context.Context) {
	sdkLogCtx.Store(&ctx)
	sdkLogOnce.Do(func() {
		azlog.SetEvents(azlog.EventRequest, azlog.EventResponse, azlog.EventResponseError,
			azlog.EventRetryPolicy, azlog.EventLRO)
		azlog.SetListener(func(ev azlog.Event, msg string) {
			if c := sdkLogCtx.Load(); c != nil {
				tflog.Debug(*c, strings.TrimRight(msg, "\n"), map[string]any{"azure_sdk_event": string(ev)})
			}
		})
	})
}

func (c *Clients) StoragePools() (*armpurestorageblock.StoragePoolsClient, error) {
	return armpurestorageblock.NewStoragePoolsClient(c.subscriptionID, c.cred, c.opts)
}

func (c *Clients) Reservations() (*armpurestorageblock.ReservationsClient, error) {
	return armpurestorageblock.NewReservationsClient(c.subscriptionID, c.cred, c.opts)
}

func (c *Clients) VolumeGroups() (*armpurestorageblock.VolumeGroupsClient, error) {
	return armpurestorageblock.NewVolumeGroupsClient(c.subscriptionID, c.cred, c.opts)
}

func (c *Clients) RecoverableVolumeGroups() (*armpurestorageblock.RecoverableVolumeGroupsClient, error) {
	return armpurestorageblock.NewRecoverableVolumeGroupsClient(c.subscriptionID, c.cred, c.opts)
}

func (c *Clients) Volumes() (*armpurestorageblock.VolumesClient, error) {
	return armpurestorageblock.NewVolumesClient(c.subscriptionID, c.cred, c.opts)
}

func (c *Clients) VolumeGroupSnapshots() (*armpurestorageblock.VolumeGroupSnapshotsClient, error) {
	return armpurestorageblock.NewVolumeGroupSnapshotsClient(c.subscriptionID, c.cred, c.opts)
}

// virtualMachines and virtualMachineExtensions are the Microsoft.Compute
// counterparts of the accessors above, for the iSCSI mount VM extension: unlike every
// other client here, the VM (and so its extensions) may live in a different
// subscription than the one this provider instance is configured for, so the caller
// supplies subscriptionID explicitly — parsed from the VM's own full ARM ID — instead
// of it coming from c.subscriptionID. They still share this Clients' credential and
// arm.ClientOptions (correlation-ID policy included, and whatever transport a test
// injects), so the same fake-server harness that backs every other client here backs
// these too.
func (c *Clients) virtualMachines(subscriptionID string) (*armcompute.VirtualMachinesClient, error) {
	return armcompute.NewVirtualMachinesClient(subscriptionID, c.cred, c.opts)
}

func (c *Clients) virtualMachineExtensions(subscriptionID string) (*armcompute.VirtualMachineExtensionsClient, error) {
	return armcompute.NewVirtualMachineExtensionsClient(subscriptionID, c.cred, c.opts)
}

func (p *EverpureAzureProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config providerModel
	if resp.Diagnostics.Append(req.Config.Get(ctx, &config)...); resp.Diagnostics.HasError() {
		return
	}

	if config.hasUnknownAuthAttribute() {
		// Not resolvable yet (e.g. a partial plan); defer to a later Configure call.
		return
	}

	subscriptionID, status, _ := resolveSetting(config.SubscriptionID, os.LookupEnv, "ARM_SUBSCRIPTION_ID", "AZURE_SUBSCRIPTION_ID")
	switch status {
	case settingUnknown:
		// Not resolvable yet (e.g. a partial plan); defer to a later Configure call.
		return
	case settingUnset:
		resp.Diagnostics.Append(operationConfigureProvider.fail(newSubscriptionIDNotSetError()))
		return
	case settingResolved:
		// fall through
	}

	authCfg := resolveAuth(config, os.LookupEnv)
	if resp.Diagnostics.Append(validateAuth(authCfg)...); resp.Diagnostics.HasError() {
		return
	}

	opts := &arm.ClientOptions{}
	if p.clientOptionsOverride != nil {
		*opts = *p.clientOptionsOverride
	}
	opts.PerCallPolicies = append(opts.PerCallPolicies, &correlationRequestIDPolicy{id: p.correlationID})
	opts.PerCallPolicies = append(opts.PerCallPolicies, newUserAgentPolicy(p.version, req.TerraformVersion, userAgentAppend()))
	// Bodies stay out of the debug log: the activation-credentials ephemeral
	// resource receives a password in a response body, which a log must never
	// carry. The correlation ID header is added to azcore's default allow-list
	// so the value every error message quotes also shows in the log.
	opts.Logging.IncludeBody = false
	opts.Logging.AllowedHeaders = append(opts.Logging.AllowedHeaders, "x-ms-correlation-request-id")
	installSDKLogging(ctx)
	// The credential is also told about these tenants (AdditionallyAllowedTenants
	// in buildCredential), but that only permits it to issue their tokens; ARM
	// only sees them when the SDK's bearer policy sends them in the
	// x-ms-authorization-auxiliary header, which it does only for tenants listed
	// here.
	opts.AuxiliaryTenants = authCfg.AuxiliaryTenantIDs
	for _, hook := range clientOptionsHooks {
		hook(opts)
	}

	factory := p.credentialFactory
	if factory == nil {
		factory = buildCredential
	}
	cred, err := factory(ctx, authCfg, opts)
	if err != nil {
		resp.Diagnostics.Append(operationConfigureProvider.fail(newAzureCredentialFailedError(err)))
		return
	}

	// Logged even on a run that never errors, so TF_LOG output alone (no
	// failure needed) is still enough to locate this run's server-side logs.
	tflog.Info(ctx, "everpure_azure correlation id", map[string]any{"correlation_id": p.correlationID})

	clients := &Clients{cred: cred, opts: opts, subscriptionID: subscriptionID, correlationID: p.correlationID}
	resp.ResourceData = clients
	resp.DataSourceData = clients
	resp.ListResourceData = clients
	resp.EphemeralResourceData = clients
	resp.ActionData = clients
}

func (p *EverpureAzureProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewReservationResource,
		NewStoragePoolResource,
		NewVolumeGroupResource,
		NewVolumeResource,
		NewSnapshotResource,
		NewVirtualMachineConnectionResource,
	}
}

func (p *EverpureAzureProvider) ListResources(_ context.Context) []func() list.ListResource {
	return []func() list.ListResource{
		NewReservationListResource,
		NewStoragePoolListResource,
		NewVolumeGroupListResource,
		NewVolumeListResource,
		NewSnapshotListResource,
	}
}

func (p *EverpureAzureProvider) EphemeralResources(_ context.Context) []func() ephemeral.EphemeralResource {
	return []func() ephemeral.EphemeralResource{
		NewPlatformConsoleActivationCredentialsEphemeralResource,
	}
}

func (p *EverpureAzureProvider) Actions(_ context.Context) []func() action.Action {
	return []func() action.Action{
		NewCreatePlatformConsoleSSHUserAction,
	}
}

func (p *EverpureAzureProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewReservationDataSource,
		NewStoragePoolDataSource,
		NewVolumeGroupDataSource,
		NewVolumeDataSource,
		NewSnapshotDataSource,
		NewReservationsDataSource,
		NewStoragePoolsDataSource,
		NewVolumeGroupsDataSource,
		NewVolumesDataSource,
		NewSnapshotsDataSource,
		NewRecoverableVolumeGroupsDataSource,
		NewVolumeGroupConnectionParametersDataSource,
		NewStoragePoolStatusDataSource,
		NewVolumeGroupStatusDataSource,
	}
}
