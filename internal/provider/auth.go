// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/entrauth/aztfauth"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

// settingStatus is the outcome of resolving one provider attribute: a usable
// value, a definitive absence, or "don't know yet" (an unknown config value,
// which Configure must not treat as an error since it can mean a later
// Configure call will supply it).
type settingStatus int

const (
	settingResolved settingStatus = iota
	settingUnset
	settingUnknown
)

// resolveSetting applies the precedence shared by every string auth
// attribute (and subscription_id, in provider.go): explicit config, then
// each env var in envKeys in order. An empty config string counts as unset.
// lookupEnv is injected so this stays pure and unit-testable.
//
// The third return value names where the value came from: "config" for the
// provider block attribute, "env:<KEY>" for the env var that supplied it, or
// "" when nothing did (unset or unknown). It exists so authConfig.Sources
// (and, from there, the credential chain's log line) can tell an operator
// which of the block or the environment actually put a method in the chain -
// never the value itself.
func resolveSetting(config types.String, lookupEnv func(string) (string, bool), envKeys ...string) (string, settingStatus, string) {
	if config.IsUnknown() {
		return "", settingUnknown, ""
	}
	if !config.IsNull() && config.ValueString() != "" {
		return config.ValueString(), settingResolved, "config"
	}
	for _, key := range envKeys {
		if v, ok := lookupEnv(key); ok && v != "" {
			return v, settingResolved, "env:" + key
		}
	}
	return "", settingUnset, ""
}

// resolveBoolSetting is resolveSetting's bool twin, for the use_* flags.
// Bool env values follow azapi's own parsing: the literal string "true" is
// true, anything else is false.
//
// Its third return value follows resolveSetting's, with one difference: a
// bool always resolves to a definite value, even when unset, so the source
// is "default" rather than "" in that case.
func resolveBoolSetting(config types.Bool, lookupEnv func(string) (string, bool), def bool, envKey string) (bool, settingStatus, string) {
	if config.IsUnknown() {
		return false, settingUnknown, ""
	}
	if !config.IsNull() {
		return config.ValueBool(), settingResolved, "config"
	}
	if v, ok := lookupEnv(envKey); ok && v != "" {
		return v == "true", settingResolved, "env:" + envKey
	}
	return def, settingUnset, "default"
}

// settingResolver wraps resolveSetting/resolveBoolSetting for resolveAuth:
// it resolves an attribute and records where its value came from in one
// step, keyed by the attribute's tfsdk name, so no attribute can be resolved
// without its source being recorded. A string attribute that resolved to
// nothing gets no entry; a bool always does (it always resolves to a value,
// if only its default).
type settingResolver struct {
	lookupEnv func(string) (string, bool)
	sources   map[string]string
}

func (r settingResolver) str(attr string, config types.String, envKeys ...string) string {
	v, _, source := resolveSetting(config, r.lookupEnv, envKeys...)
	if source != "" {
		r.sources[attr] = source
	}
	return v
}

func (r settingResolver) boolean(attr string, config types.Bool, def bool, envKey string) bool {
	v, _, source := resolveBoolSetting(config, r.lookupEnv, def, envKey)
	r.sources[attr] = source
	return v
}

// resolveAuxiliaryTenantIDs resolves auxiliary_tenant_ids: the config list if
// set and non-empty, else ARM_AUXILIARY_TENANT_IDS split on ";" (that
// variable has no AZURE_* twin — it isn't one of the settings azidentity
// itself defines). A configured empty list is treated the same as an unset
// one and still falls through to the environment variable, matching
// azurerm's own behaviour for this attribute. A null element (as opposed to
// the whole list being null) is skipped rather than turned into an empty
// tenant ID.
func resolveAuxiliaryTenantIDs(config types.List, lookupEnv func(string) (string, bool)) []string {
	if !config.IsNull() && !config.IsUnknown() {
		elems := config.Elements()
		if len(elems) > 0 {
			ids := make([]string, 0, len(elems))
			for _, e := range elems {
				s, ok := e.(types.String)
				if !ok || s.IsNull() || s.IsUnknown() {
					continue
				}
				ids = append(ids, s.ValueString())
			}
			return ids
		}
	}
	if v, ok := lookupEnv("ARM_AUXILIARY_TENANT_IDS"); ok && v != "" {
		return strings.Split(v, ";")
	}
	return nil
}

// authConfig is every authentication-related provider attribute, resolved
// against its env fallbacks. It is the single input buildCredential needs to
// construct the run's credential.
type authConfig struct {
	TenantID string
	ClientID string

	ClientIDFilePath string

	ClientSecret         string
	ClientSecretFilePath string

	ClientCertificate         string
	ClientCertificatePath     string
	ClientCertificatePassword string

	UseOIDC           bool
	OIDCToken         string
	OIDCTokenFilePath string

	OIDCRequestToken             string
	OIDCRequestURL               string
	OIDCAzureServiceConnectionID string

	// AzurePipelinesOIDCRequestURI is SYSTEM_OIDCREQUESTURI, read from the
	// environment only - it has no provider attribute, since the operator
	// has no way to set it themselves. Azure Pipelines sets it in every
	// job's environment; azidentity.NewAzurePipelinesCredential requires it
	// and errors when it is absent, so validateAuth checks it here rather
	// than let that surface as an opaque credential-chain failure.
	AzurePipelinesOIDCRequestURI string

	UseCLI                 bool
	UseMSI                 bool
	UseAKSWorkloadIdentity bool

	AuxiliaryTenantIDs []string

	// Sources records, for every attribute above, where the resolved value
	// came from: "config", "env:<VAR>", or "default" for a bool that fell
	// back to its default.
	// Keyed by the attribute's own tfsdk name (e.g. "client_secret"), not
	// its Go field name. A string attribute that never resolved to a value
	// has no entry at all; a bool attribute always does, since it always
	// resolves to something. Never holds an attribute's actual value - only
	// where it came from - so it is safe to log.
	Sources map[string]string

	// AKSTenantMismatch records a tenant_id that disagrees with
	// AZURE_TENANT_ID while UseAKSWorkloadIdentity is true. resolveAuth only
	// detects this - it never adds errors itself - so validateAuth is where
	// it becomes a diagnostic.
	AKSTenantMismatch              bool
	AKSTenantMismatchAzureTenantID string
}

// resolveAuth resolves every auth attribute in providerModel against its env
// fallbacks (see the provider schema for the exact list per attribute), then
// applies the two rules that only apply under AKS workload identity: the
// three extra defaults (client_id, tenant_id, oidc_token_file_path from the
// AZURE_* trio the AKS pod identity webhook sets) and the tenant_id/
// AZURE_TENANT_ID mismatch check. Callers must first confirm no field in
// config is unknown; resolveAuth does not itself return a not-yet-known
// status.
func resolveAuth(config providerModel, lookupEnv func(string) (string, bool)) authConfig {
	var cfg authConfig
	cfg.Sources = make(map[string]string)
	r := settingResolver{lookupEnv: lookupEnv, sources: cfg.Sources}

	cfg.TenantID = r.str("tenant_id", config.TenantID, "ARM_TENANT_ID", "AZURE_TENANT_ID")
	cfg.ClientID = r.str("client_id", config.ClientID, "ARM_CLIENT_ID", "AZURE_CLIENT_ID")
	cfg.ClientIDFilePath = r.str("client_id_file_path", config.ClientIDFilePath, "ARM_CLIENT_ID_FILE_PATH")
	cfg.ClientSecret = r.str("client_secret", config.ClientSecret, "ARM_CLIENT_SECRET", "AZURE_CLIENT_SECRET")
	cfg.ClientSecretFilePath = r.str("client_secret_file_path", config.ClientSecretFilePath, "ARM_CLIENT_SECRET_FILE_PATH")
	cfg.ClientCertificate = r.str("client_certificate", config.ClientCertificate, "ARM_CLIENT_CERTIFICATE")
	cfg.ClientCertificatePath = r.str("client_certificate_path", config.ClientCertificatePath,
		"ARM_CLIENT_CERTIFICATE_PATH", "AZURE_CLIENT_CERTIFICATE_PATH")
	cfg.ClientCertificatePassword = r.str("client_certificate_password", config.ClientCertificatePassword,
		"ARM_CLIENT_CERTIFICATE_PASSWORD", "AZURE_CLIENT_CERTIFICATE_PASSWORD")

	cfg.UseOIDC = r.boolean("use_oidc", config.UseOIDC, false, "ARM_USE_OIDC")
	cfg.OIDCToken = r.str("oidc_token", config.OIDCToken, "ARM_OIDC_TOKEN")
	cfg.OIDCRequestToken = r.str("oidc_request_token", config.OIDCRequestToken,
		"ARM_OIDC_REQUEST_TOKEN", "ACTIONS_ID_TOKEN_REQUEST_TOKEN", "SYSTEM_ACCESSTOKEN")
	cfg.OIDCRequestURL = r.str("oidc_request_url", config.OIDCRequestURL, "ARM_OIDC_REQUEST_URL", "ACTIONS_ID_TOKEN_REQUEST_URL")
	cfg.OIDCAzureServiceConnectionID = r.str("oidc_azure_service_connection_id", config.OIDCAzureServiceConnectionID,
		"ARM_ADO_PIPELINE_SERVICE_CONNECTION_ID", "ARM_OIDC_AZURE_SERVICE_CONNECTION_ID", "AZURESUBSCRIPTION_SERVICE_CONNECTION_ID")
	if v, ok := lookupEnv("SYSTEM_OIDCREQUESTURI"); ok {
		cfg.AzurePipelinesOIDCRequestURI = v
	}

	cfg.UseCLI = r.boolean("use_cli", config.UseCLI, true, "ARM_USE_CLI")
	cfg.UseMSI = r.boolean("use_msi", config.UseMSI, false, "ARM_USE_MSI")
	cfg.UseAKSWorkloadIdentity = r.boolean("use_aks_workload_identity", config.UseAKSWorkloadIdentity, false, "ARM_USE_AKS_WORKLOAD_IDENTITY")

	oidcTokenFileEnvKeys := []string{"ARM_OIDC_TOKEN_FILE_PATH"}
	if cfg.UseAKSWorkloadIdentity {
		// Only consulted under AKS workload identity: unlike ARM_TENANT_ID/
		// AZURE_TENANT_ID and ARM_CLIENT_ID/AZURE_CLIENT_ID above (which are
		// this attribute's fallback regardless of mode), AZURE_FEDERATED_TOKEN_FILE
		// is the path the AKS pod identity webhook actually mounts the token
		// at, so it's only a meaningful fallback when that webhook is in play.
		oidcTokenFileEnvKeys = append(oidcTokenFileEnvKeys, "AZURE_FEDERATED_TOKEN_FILE")
	}
	cfg.OIDCTokenFilePath = r.str("oidc_token_file_path", config.OIDCTokenFilePath, oidcTokenFileEnvKeys...)

	if cfg.UseAKSWorkloadIdentity {
		if v, ok := lookupEnv("AZURE_TENANT_ID"); ok && v != "" {
			switch {
			case cfg.TenantID == "":
				cfg.TenantID = v
				r.sources["tenant_id"] = "env:AZURE_TENANT_ID"
			case cfg.TenantID != v:
				cfg.AKSTenantMismatch = true
				cfg.AKSTenantMismatchAzureTenantID = v
			}
		}
	}

	cfg.AuxiliaryTenantIDs = resolveAuxiliaryTenantIDs(config.AuxiliaryTenantIDs, lookupEnv)

	return cfg
}

// validateAuth runs the checks that are knowable before any network call: a
// method's required companion attributes are present, referenced files
// exist and are readable, and a configured certificate actually parses.
// Everything else (a wrong secret, an expired certificate, an unreachable
// tenant) can only be discovered at token-request time and surfaces from the
// credential chain itself, naming the failing method.
func validateAuth(cfg authConfig) diag.Diagnostics {
	var diags diag.Diagnostics

	explicitCredentialInput := cfg.ClientSecret != "" || cfg.ClientSecretFilePath != "" ||
		cfg.ClientCertificate != "" || cfg.ClientCertificatePath != "" || cfg.UseOIDC || cfg.UseAKSWorkloadIdentity
	if explicitCredentialInput {
		if cfg.ClientID == "" && cfg.ClientIDFilePath == "" {
			diags.Append(operationConfigureProvider.fail(newClientIDNotSetError()))
		}
		if cfg.TenantID == "" {
			diags.Append(operationConfigureProvider.fail(newTenantIDNotSetError()))
		}
	}

	if cfg.UseOIDC {
		hasRequestPair := cfg.OIDCRequestURL != "" && cfg.OIDCRequestToken != ""
		switch {
		case cfg.OIDCAzureServiceConnectionID != "":
			// The Azure Pipelines OIDC flow calls azidentity.NewAzurePipelinesCredential,
			// which errors out when the system access token is empty and when
			// SYSTEM_OIDCREQUESTURI is absent from the environment - so the service
			// connection ID alone can never produce a working credential.
			if cfg.OIDCRequestToken == "" {
				diags.Append(operationConfigureProvider.fail(newAzurePipelinesRequestTokenNotSetError()))
			}
			if cfg.AzurePipelinesOIDCRequestURI == "" {
				diags.Append(operationConfigureProvider.fail(newNotInAzurePipelinesError()))
			}
		case cfg.OIDCToken == "" && cfg.OIDCTokenFilePath == "" && !hasRequestPair:
			diags.Append(operationConfigureProvider.fail(newOidcInputsNotSetError()))
		}
	}

	if cfg.UseAKSWorkloadIdentity && cfg.OIDCTokenFilePath == "" {
		diags.Append(operationConfigureProvider.fail(newAksTokenFileNotSetError()))
	}

	if cfg.AKSTenantMismatch {
		diags.Append(operationConfigureProvider.fail(newAksTenantMismatchError(cfg.TenantID, cfg.AKSTenantMismatchAzureTenantID)))
	}

	if len(configuredMethodNames(cfg)) == 0 {
		diags.Append(operationConfigureProvider.fail(newNoAuthMethodConfiguredError()))
	}

	checkFileReadable(&diags, "client_id_file_path", "ARM_CLIENT_ID_FILE_PATH", cfg.ClientIDFilePath)
	checkFileReadable(&diags, "client_secret_file_path", "ARM_CLIENT_SECRET_FILE_PATH", cfg.ClientSecretFilePath)
	certPathReadable := checkFileReadable(
		&diags, "client_certificate_path", "ARM_CLIENT_CERTIFICATE_PATH / AZURE_CLIENT_CERTIFICATE_PATH", cfg.ClientCertificatePath,
	)
	if cfg.UseOIDC || cfg.UseAKSWorkloadIdentity {
		checkFileReadable(&diags, "oidc_token_file_path", "ARM_OIDC_TOKEN_FILE_PATH / AZURE_FEDERATED_TOKEN_FILE", cfg.OIDCTokenFilePath)
	}

	if cfg.ClientCertificate != "" || (cfg.ClientCertificatePath != "" && certPathReadable) {
		validateClientCertificate(&diags, cfg)
	}

	return diags
}

// checkFileReadable reports whether path (when non-empty) can actually be
// opened, without reading a secret's contents into a diagnostic. attr and
// envDesc name the attribute and its env fallbacks in the error, matching
// every other validateAuth rule.
func checkFileReadable(diags *diag.Diagnostics, attr, envDesc, path string) bool {
	if path == "" {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		diags.Append(operationConfigureProvider.fail(newAuthFileUnreadableError(attr, envDesc, path, err)))
		return false
	}
	_ = f.Close()
	return true
}

// validateClientCertificate pre-parses a configured certificate with the same
// two parsers aztfauth itself falls back between (azidentity's, then
// go-pkcs12's for the modern PKCS#12 files azidentity's own parser rejects),
// so a malformed bundle is reported at Configure time by name rather than
// surfacing later as an opaque credential-chain failure.
func validateClientCertificate(diags *diag.Diagnostics, cfg authConfig) {
	data, source, err := clientCertificateBytes(cfg)
	if err != nil {
		diags.Append(operationConfigureProvider.fail(newClientCertificateUnreadableError(err)))
		return
	}

	password := []byte(cfg.ClientCertificatePassword)
	if _, _, err := azidentity.ParseCertificates(data, password); err == nil {
		return
	}
	if _, _, _, err := pkcs12.DecodeChain(data, cfg.ClientCertificatePassword); err == nil {
		return
	}

	if cfg.ClientCertificatePassword == "" {
		diags.Append(operationConfigureProvider.fail(newClientCertificateUnparseableError(source, false)))
		return
	}
	diags.Append(operationConfigureProvider.fail(newClientCertificateUnparseableError(source, true)))
}

func clientCertificateBytes(cfg authConfig) (data []byte, source string, err error) {
	if cfg.ClientCertificate != "" {
		b, err := base64.StdEncoding.DecodeString(cfg.ClientCertificate)
		if err != nil {
			return nil, "", fmt.Errorf("client_certificate is not valid base64: %w", err)
		}
		return b, "client_certificate", nil
	}
	b, err := os.ReadFile(cfg.ClientCertificatePath)
	if err != nil {
		return nil, "", fmt.Errorf("client_certificate_path %q could not be read: %w", cfg.ClientCertificatePath, err)
	}
	return b, fmt.Sprintf("client_certificate_path %q", cfg.ClientCertificatePath), nil
}

// configuredMethodNames lists the credential methods buildCredential asked
// aztfauth to attempt, in its own try order. It is best-effort: aztfauth
// silently skips a method that fails to construct (logged at debug via
// tflogWriter, not returned as an error), so this names what was configured,
// not a confirmed-working method.
func configuredMethodNames(cfg authConfig) []string {
	var methods []string
	if cfg.UseOIDC && cfg.OIDCToken != "" {
		methods = append(methods, "oidc_token")
	}
	if (cfg.UseOIDC || cfg.UseAKSWorkloadIdentity) && cfg.OIDCTokenFilePath != "" {
		methods = append(methods, "oidc_token_file")
	}
	if cfg.UseOIDC && cfg.OIDCRequestToken != "" {
		methods = append(methods, "oidc_token_request")
	}
	if cfg.ClientSecret != "" || cfg.ClientSecretFilePath != "" {
		methods = append(methods, "client_secret")
	}
	if cfg.ClientCertificate != "" || cfg.ClientCertificatePath != "" {
		methods = append(methods, "client_certificate")
	}
	if cfg.UseMSI {
		methods = append(methods, "managed_identity")
	}
	if cfg.UseCLI {
		methods = append(methods, "azure_cli")
	}
	return methods
}

// methodSources maps each method name configuredMethodNames reports to
// where its defining attribute's value came from ("config", "env:<VAR>", or
// "default" for use_cli/use_msi when neither was set) - so a customer with a
// wrong secret and an inherited ARM_CLIENT_SECRET can see from TF_LOG=INFO
// alone that their provider block was never the one tried, rather than
// reading the log for a wrong-identity failure and assuming a bug. A method
// with no recorded source (config.Sources has no entry for its defining
// attribute) is left out of the result - this only happens for a method
// configuredMethodNames itself could not have reported, so in practice it
// never does.
func methodSources(cfg authConfig) map[string]string {
	sources := make(map[string]string)
	for _, method := range configuredMethodNames(cfg) {
		var attr string
		switch method {
		case "oidc_token":
			attr = "oidc_token"
		case "oidc_token_file":
			attr = "oidc_token_file_path"
		case "oidc_token_request":
			if cfg.OIDCAzureServiceConnectionID != "" {
				attr = "oidc_azure_service_connection_id"
			} else {
				attr = "oidc_request_token"
			}
		case "client_secret":
			if cfg.ClientSecret != "" {
				attr = "client_secret"
			} else {
				attr = "client_secret_file_path"
			}
		case "client_certificate":
			if cfg.ClientCertificate != "" {
				attr = "client_certificate"
			} else {
				attr = "client_certificate_path"
			}
		case "managed_identity":
			attr = "use_msi"
		case "azure_cli":
			attr = "use_cli"
		}
		if src, ok := cfg.Sources[attr]; ok {
			sources[method] = src
		}
	}
	return sources
}

// tflogWriter adapts an io.Writer to tflog.Debug, so it can back the
// *log.Logger handed to aztfauth.Option.Logger. aztfauth logs one "failed to
// build ... cred option" line for every method it was not asked to use, in
// addition to any it was asked to use but couldn't - both land here, at
// debug level, so a customer sees them only under TF_LOG=DEBUG and they are
// never mistaken for the provider's own errors.
type tflogWriter struct {
	ctx context.Context //nolint:containedctx // adapts to the io.Writer interface, which carries no context parameter of its own.
}

func (w tflogWriter) Write(p []byte) (int, error) {
	tflog.Debug(w.ctx, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// buildCredential is the production credentialFactory: it turns a resolved
// authConfig into aztfauth's credential chain. aztfauth.Option is filled
// exactly as azapi fills it, so the method order and fall-through behaviour
// among configured methods stays identical to azapi.
func buildCredential(ctx context.Context, cfg authConfig, opts *arm.ClientOptions) (azcore.TokenCredential, error) {
	logger := log.New(tflogWriter{ctx: ctx}, "", 0)

	opt := aztfauth.Option{
		Logger: logger,

		TenantId:     cfg.TenantID,
		ClientId:     cfg.ClientID,
		ClientIdFile: cfg.ClientIDFilePath,

		UseClientSecret:  true,
		ClientSecret:     cfg.ClientSecret,
		ClientSecretFile: cfg.ClientSecretFilePath,

		UseClientCert:      true,
		ClientCertBase64:   cfg.ClientCertificate,
		ClientCertPfxFile:  cfg.ClientCertificatePath,
		ClientCertPassword: []byte(cfg.ClientCertificatePassword),

		UseOIDCToken:     cfg.UseOIDC,
		OIDCToken:        cfg.OIDCToken,
		UseOIDCTokenFile: cfg.UseOIDC || cfg.UseAKSWorkloadIdentity,
		OIDCTokenFile:    cfg.OIDCTokenFilePath,

		UseOIDCTokenRequest:    cfg.UseOIDC,
		OIDCRequestToken:       cfg.OIDCRequestToken,
		OIDCRequestURL:         cfg.OIDCRequestURL,
		ADOServiceConnectionId: cfg.OIDCAzureServiceConnectionID,

		UseMSI:      cfg.UseMSI,
		UseAzureCLI: cfg.UseCLI,

		ClientOptions:              opts.ClientOptions,
		AdditionallyAllowedTenants: cfg.AuxiliaryTenantIDs,
	}

	methods := configuredMethodNames(cfg)
	sources := methodSources(cfg)

	libCred, libErr := aztfauth.NewCredential(opt)
	if libErr != nil {
		return nil, fmt.Errorf("building Azure credential: %w", libErr)
	}
	tflog.Info(ctx, "everpure_azure credential chain", map[string]any{"methods": methods, "method_sources": sources})
	return libCred, nil
}
