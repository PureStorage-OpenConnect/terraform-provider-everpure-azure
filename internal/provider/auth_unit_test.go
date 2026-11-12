// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflogtest"
	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

func TestResolveSetting(t *testing.T) {
	tests := []struct {
		name       string
		config     types.String
		env        map[string]string
		envKeys    []string
		wantStatus settingStatus
		wantValue  string
		// wantSource is resolveSetting's third return value: "config", the
		// specific "env:<KEY>" that supplied the value, or "" when nothing
		// resolved. It backs authConfig.Sources, which the credential
		// chain's log line reports as method_sources - so every case here
		// checks it, not just the ones added for that purpose.
		wantSource string
	}{
		{
			name:       "prefers config over any env var",
			config:     types.StringValue("config-value"),
			env:        map[string]string{"ARM_X": "arm-value", "AZURE_X": "azure-value"},
			envKeys:    []string{"ARM_X", "AZURE_X"},
			wantStatus: settingResolved,
			wantValue:  "config-value",
			wantSource: "config",
		},
		{
			name:       "falls back to the first env key",
			config:     types.StringNull(),
			env:        map[string]string{"ARM_X": "arm-value", "AZURE_X": "azure-value"},
			envKeys:    []string{"ARM_X", "AZURE_X"},
			wantStatus: settingResolved,
			wantValue:  "arm-value",
			wantSource: "env:ARM_X",
		},
		{
			name:       "falls back to the second env key when the first is unset",
			config:     types.StringNull(),
			env:        map[string]string{"AZURE_X": "azure-value"},
			envKeys:    []string{"ARM_X", "AZURE_X"},
			wantStatus: settingResolved,
			wantValue:  "azure-value",
			wantSource: "env:AZURE_X",
		},
		{
			name:       "falls back through a third env key",
			config:     types.StringNull(),
			env:        map[string]string{"THIRD_X": "third-value"},
			envKeys:    []string{"ARM_X", "AZURE_X", "THIRD_X"},
			wantStatus: settingResolved,
			wantValue:  "third-value",
			wantSource: "env:THIRD_X",
		},
		{
			name:       "treats an empty config string as unset",
			config:     types.StringValue(""),
			env:        map[string]string{"ARM_X": "arm-value"},
			envKeys:    []string{"ARM_X"},
			wantStatus: settingResolved,
			wantValue:  "arm-value",
			wantSource: "env:ARM_X",
		},
		{
			name:       "reports unset with no config and no env",
			config:     types.StringNull(),
			envKeys:    []string{"ARM_X", "AZURE_X"},
			wantStatus: settingUnset,
			wantSource: "",
		},
		{
			name:       "reports unset with no env keys given",
			config:     types.StringNull(),
			wantStatus: settingUnset,
			wantSource: "",
		},
		{
			name:       "propagates an unknown config value without consulting the environment",
			config:     types.StringUnknown(),
			env:        map[string]string{"ARM_X": "arm-value"},
			envKeys:    []string{"ARM_X"},
			wantStatus: settingUnknown,
			wantSource: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, status, source := resolveSetting(tt.config, lookupEnvFrom(tt.env), tt.envKeys...)
			if status != tt.wantStatus {
				t.Fatalf("status = %v, want %v", status, tt.wantStatus)
			}
			if status == settingResolved && got != tt.wantValue {
				t.Fatalf("value = %q, want %q", got, tt.wantValue)
			}
			if source != tt.wantSource {
				t.Errorf("source = %q, want %q", source, tt.wantSource)
			}
		})
	}
}

func TestResolveBoolSetting(t *testing.T) {
	tests := []struct {
		name       string
		config     types.Bool
		env        map[string]string
		def        bool
		wantStatus settingStatus
		wantValue  bool
		// wantSource is resolveBoolSetting's third return value: "config",
		// "env:ARM_X", or "default" - a bool always resolves to something,
		// so unlike resolveSetting this is "" only when config is unknown.
		wantSource string
	}{
		{
			name:       "config true overrides env and default",
			config:     types.BoolValue(true),
			env:        map[string]string{"ARM_X": "false"},
			def:        false,
			wantStatus: settingResolved,
			wantValue:  true,
			wantSource: "config",
		},
		{
			name:       "config false overrides env and default",
			config:     types.BoolValue(false),
			env:        map[string]string{"ARM_X": "true"},
			def:        true,
			wantStatus: settingResolved,
			wantValue:  false,
			wantSource: "config",
		},
		{
			name:       "env literal true parses as true",
			config:     types.BoolNull(),
			env:        map[string]string{"ARM_X": "true"},
			def:        false,
			wantStatus: settingResolved,
			wantValue:  true,
			wantSource: "env:ARM_X",
		},
		{
			name:       "env TRUE (wrong case) parses as false, exactly azapi's parsing",
			config:     types.BoolNull(),
			env:        map[string]string{"ARM_X": "TRUE"},
			def:        true,
			wantStatus: settingResolved,
			wantValue:  false,
			wantSource: "env:ARM_X",
		},
		{
			name:       "env 1 parses as false, only the literal string true is true",
			config:     types.BoolNull(),
			env:        map[string]string{"ARM_X": "1"},
			def:        true,
			wantStatus: settingResolved,
			wantValue:  false,
			wantSource: "env:ARM_X",
		},
		{
			name:       "unset falls back to the default",
			config:     types.BoolNull(),
			def:        true,
			wantStatus: settingUnset,
			wantValue:  true,
			wantSource: "default",
		},
		{
			name:       "env present but empty falls back to the default, exactly azapi's parsing",
			config:     types.BoolNull(),
			env:        map[string]string{"ARM_X": ""},
			def:        true,
			wantStatus: settingUnset,
			wantValue:  true,
			wantSource: "default",
		},
		{
			name:       "unknown config propagates without consulting the environment",
			config:     types.BoolUnknown(),
			env:        map[string]string{"ARM_X": "true"},
			def:        false,
			wantStatus: settingUnknown,
			wantSource: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, status, source := resolveBoolSetting(tt.config, lookupEnvFrom(tt.env), tt.def, "ARM_X")
			if status != tt.wantStatus {
				t.Fatalf("status = %v, want %v", status, tt.wantStatus)
			}
			if got != tt.wantValue {
				t.Fatalf("value = %v, want %v", got, tt.wantValue)
			}
			if source != tt.wantSource {
				t.Errorf("source = %q, want %q", source, tt.wantSource)
			}
		})
	}
}

func stringList(t *testing.T, values ...string) types.List {
	t.Helper()
	l, diags := types.ListValueFrom(context.Background(), types.StringType, values)
	if diags.HasError() {
		t.Fatalf("unexpected error building list: %v", diags)
	}
	return l
}

func TestResolveAuth(t *testing.T) {
	t.Run("mixed sources: attribute for one field, ARM_* for another, AZURE_* for a third", func(t *testing.T) {
		config := providerModel{
			ClientID: types.StringValue("client-from-config"),
		}
		env := map[string]string{
			"ARM_CLIENT_SECRET": "secret-from-arm",
			"AZURE_TENANT_ID":   "tenant-from-azure",
		}

		cfg := resolveAuth(config, lookupEnvFrom(env))

		if cfg.ClientID != "client-from-config" {
			t.Errorf("ClientID = %q, want %q", cfg.ClientID, "client-from-config")
		}
		if cfg.ClientSecret != "secret-from-arm" {
			t.Errorf("ClientSecret = %q, want %q", cfg.ClientSecret, "secret-from-arm")
		}
		if cfg.TenantID != "tenant-from-azure" {
			t.Errorf("TenantID = %q, want %q", cfg.TenantID, "tenant-from-azure")
		}
	})

	t.Run("defaults", func(t *testing.T) {
		cfg := resolveAuth(providerModel{}, lookupEnvFrom(nil))
		if !cfg.UseCLI {
			t.Error("UseCLI default should be true")
		}
		if cfg.UseMSI {
			t.Error("UseMSI default should be false")
		}
		if cfg.UseOIDC {
			t.Error("UseOIDC default should be false")
		}
		if cfg.UseAKSWorkloadIdentity {
			t.Error("UseAKSWorkloadIdentity default should be false")
		}
	})

	t.Run("AKS workload identity defaults client_id, tenant_id and oidc_token_file_path from the AZURE_* trio", func(t *testing.T) {
		config := providerModel{
			UseAKSWorkloadIdentity: types.BoolValue(true),
		}
		env := map[string]string{ //nolint:gosec // env var names, not credential values; AZURE_FEDERATED_TOKEN_FILE is a file path, not a token
			"AZURE_CLIENT_ID":            "aks-client",
			"AZURE_TENANT_ID":            "aks-tenant",
			"AZURE_FEDERATED_TOKEN_FILE": "/var/run/secrets/azure/federated-credential",
		}

		cfg := resolveAuth(config, lookupEnvFrom(env))

		if cfg.ClientID != "aks-client" {
			t.Errorf("ClientID = %q, want %q", cfg.ClientID, "aks-client")
		}
		if cfg.TenantID != "aks-tenant" {
			t.Errorf("TenantID = %q, want %q", cfg.TenantID, "aks-tenant")
		}
		if cfg.OIDCTokenFilePath != "/var/run/secrets/azure/federated-credential" {
			t.Errorf("OIDCTokenFilePath = %q, want %q", cfg.OIDCTokenFilePath, "/var/run/secrets/azure/federated-credential")
		}
		if cfg.AKSTenantMismatch {
			t.Error("AKSTenantMismatch should be false when tenant_id is not separately configured")
		}
	})

	t.Run("AZURE_FEDERATED_TOKEN_FILE is not consulted without use_aks_workload_identity", func(t *testing.T) {
		//nolint:gosec // env var name, not a credential value; AZURE_FEDERATED_TOKEN_FILE is a file path, not a token
		env := map[string]string{"AZURE_FEDERATED_TOKEN_FILE": "/var/run/secrets/azure/federated-credential"}
		cfg := resolveAuth(providerModel{}, lookupEnvFrom(env))
		if cfg.OIDCTokenFilePath != "" {
			t.Errorf("OIDCTokenFilePath = %q, want empty", cfg.OIDCTokenFilePath)
		}
	})

	t.Run("tenant_id disagreeing with AZURE_TENANT_ID under AKS workload identity is flagged, not silently overridden", func(t *testing.T) {
		config := providerModel{
			UseAKSWorkloadIdentity: types.BoolValue(true),
			TenantID:               types.StringValue("explicit-tenant"),
		}
		env := map[string]string{"AZURE_TENANT_ID": "aks-tenant"}

		cfg := resolveAuth(config, lookupEnvFrom(env))

		if !cfg.AKSTenantMismatch {
			t.Fatal("expected AKSTenantMismatch")
		}
		if cfg.AKSTenantMismatchAzureTenantID != "aks-tenant" {
			t.Errorf("AKSTenantMismatchAzureTenantID = %q, want %q", cfg.AKSTenantMismatchAzureTenantID, "aks-tenant")
		}
		if cfg.TenantID != "explicit-tenant" {
			t.Errorf("TenantID = %q, want it left as configured (%q)", cfg.TenantID, "explicit-tenant")
		}
	})

	t.Run("oidc_request_token: ARM_OIDC_REQUEST_TOKEN wins over the GitHub and ADO variables", func(t *testing.T) {
		env := map[string]string{
			"ARM_OIDC_REQUEST_TOKEN":         "arm-token",
			"ACTIONS_ID_TOKEN_REQUEST_TOKEN": "github-token",
			"SYSTEM_ACCESSTOKEN":             "ado-token",
		}
		cfg := resolveAuth(providerModel{}, lookupEnvFrom(env))
		if cfg.OIDCRequestToken != "arm-token" {
			t.Errorf("OIDCRequestToken = %q, want %q", cfg.OIDCRequestToken, "arm-token")
		}
	})

	t.Run("oidc_request_token: falls back to the GitHub Actions variable", func(t *testing.T) {
		env := map[string]string{
			"ACTIONS_ID_TOKEN_REQUEST_TOKEN": "github-token",
			"SYSTEM_ACCESSTOKEN":             "ado-token",
		}
		cfg := resolveAuth(providerModel{}, lookupEnvFrom(env))
		if cfg.OIDCRequestToken != "github-token" {
			t.Errorf("OIDCRequestToken = %q, want %q", cfg.OIDCRequestToken, "github-token")
		}
	})

	t.Run("oidc_request_token: falls back to the Azure Pipelines variable", func(t *testing.T) {
		env := map[string]string{"SYSTEM_ACCESSTOKEN": "ado-token"}
		cfg := resolveAuth(providerModel{}, lookupEnvFrom(env))
		if cfg.OIDCRequestToken != "ado-token" {
			t.Errorf("OIDCRequestToken = %q, want %q", cfg.OIDCRequestToken, "ado-token")
		}
	})

	t.Run("auxiliary_tenant_ids: config list wins over the environment", func(t *testing.T) {
		config := providerModel{AuxiliaryTenantIDs: stringList(t, "tenant-a", "tenant-b")}
		env := map[string]string{"ARM_AUXILIARY_TENANT_IDS": "tenant-c;tenant-d"}

		cfg := resolveAuth(config, lookupEnvFrom(env))

		want := []string{"tenant-a", "tenant-b"}
		if len(cfg.AuxiliaryTenantIDs) != len(want) {
			t.Fatalf("AuxiliaryTenantIDs = %v, want %v", cfg.AuxiliaryTenantIDs, want)
		}
		for i, v := range want {
			if cfg.AuxiliaryTenantIDs[i] != v {
				t.Errorf("AuxiliaryTenantIDs[%d] = %q, want %q", i, cfg.AuxiliaryTenantIDs[i], v)
			}
		}
	})

	t.Run("auxiliary_tenant_ids: ARM_AUXILIARY_TENANT_IDS splits on ;", func(t *testing.T) {
		env := map[string]string{"ARM_AUXILIARY_TENANT_IDS": "tenant-c;tenant-d;tenant-e"}
		cfg := resolveAuth(providerModel{}, lookupEnvFrom(env))
		want := []string{"tenant-c", "tenant-d", "tenant-e"}
		if len(cfg.AuxiliaryTenantIDs) != len(want) {
			t.Fatalf("AuxiliaryTenantIDs = %v, want %v", cfg.AuxiliaryTenantIDs, want)
		}
		for i, v := range want {
			if cfg.AuxiliaryTenantIDs[i] != v {
				t.Errorf("AuxiliaryTenantIDs[%d] = %q, want %q", i, cfg.AuxiliaryTenantIDs[i], v)
			}
		}
	})

	t.Run("auxiliary_tenant_ids: an empty configured list still falls through to the environment", func(t *testing.T) {
		config := providerModel{AuxiliaryTenantIDs: stringList(t)}
		env := map[string]string{"ARM_AUXILIARY_TENANT_IDS": "tenant-c;tenant-d"}

		cfg := resolveAuth(config, lookupEnvFrom(env))

		want := []string{"tenant-c", "tenant-d"}
		if len(cfg.AuxiliaryTenantIDs) != len(want) {
			t.Fatalf("AuxiliaryTenantIDs = %v, want %v", cfg.AuxiliaryTenantIDs, want)
		}
		for i, v := range want {
			if cfg.AuxiliaryTenantIDs[i] != v {
				t.Errorf("AuxiliaryTenantIDs[%d] = %q, want %q", i, cfg.AuxiliaryTenantIDs[i], v)
			}
		}
	})

	t.Run("auxiliary_tenant_ids: a null element is skipped rather than turned into an empty tenant ID", func(t *testing.T) {
		list, diags := types.ListValue(types.StringType, []attr.Value{
			types.StringValue("tenant-a"),
			types.StringNull(),
			types.StringValue("tenant-b"),
		})
		if diags.HasError() {
			t.Fatalf("unexpected error building list: %v", diags)
		}
		cfg := resolveAuth(providerModel{AuxiliaryTenantIDs: list}, lookupEnvFrom(nil))

		want := []string{"tenant-a", "tenant-b"}
		if len(cfg.AuxiliaryTenantIDs) != len(want) {
			t.Fatalf("AuxiliaryTenantIDs = %v, want %v", cfg.AuxiliaryTenantIDs, want)
		}
		for i, v := range want {
			if cfg.AuxiliaryTenantIDs[i] != v {
				t.Errorf("AuxiliaryTenantIDs[%d] = %q, want %q", i, cfg.AuxiliaryTenantIDs[i], v)
			}
		}
	})
}

// generateSelfSignedCert returns a throwaway self-signed certificate and key
// for exercising validateClientCertificate; the identity it names is never
// checked against anything, only that the bundle parses.
func generateSelfSignedCert(t *testing.T) (*x509.Certificate, *rsa.PrivateKey) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "everpure-azure-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing certificate: %v", err)
	}
	return cert, key
}

func TestValidateAuth(t *testing.T) {
	t.Run("client_secret set but client_id missing", func(t *testing.T) {
		diags := validateAuth(authConfig{ClientSecret: "s", TenantID: "t"})
		assertDiagContains(t, diags, "client_id", "ARM_CLIENT_ID")
	})

	t.Run("client_secret_file_path set but client_id missing", func(t *testing.T) {
		diags := validateAuth(authConfig{ClientSecretFilePath: "/path", TenantID: "t"})
		assertDiagContains(t, diags, "client_id", "ARM_CLIENT_ID")
	})

	t.Run("client_id_file_path satisfies the client_id requirement", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "client-id")
		if err := os.WriteFile(path, []byte("client-id-value"), 0o600); err != nil {
			t.Fatalf("writing fixture: %v", err)
		}
		diags := validateAuth(authConfig{ClientIDFilePath: path, ClientSecret: "s", TenantID: "t"})
		if diags.HasError() {
			t.Fatalf("unexpected error: %v", diags)
		}
	})

	t.Run("client_certificate set but tenant_id missing", func(t *testing.T) {
		diags := validateAuth(authConfig{ClientCertificate: "s", ClientID: "c"})
		assertDiagContains(t, diags, "tenant_id", "ARM_TENANT_ID")
	})

	t.Run("use_oidc set but neither client_id nor tenant_id resolved", func(t *testing.T) {
		diags := validateAuth(authConfig{UseOIDC: true, OIDCToken: "tok"})
		assertDiagContains(t, diags, "client_id", "ARM_CLIENT_ID")
		assertDiagContains(t, diags, "tenant_id", "ARM_TENANT_ID")
	})

	t.Run("use_oidc with no OIDC input at all", func(t *testing.T) {
		diags := validateAuth(authConfig{UseOIDC: true, ClientID: "c", TenantID: "t"})
		assertDiagContains(t, diags, "use_oidc", "ARM_USE_OIDC")
	})

	t.Run("use_oidc satisfied by oidc_token alone", func(t *testing.T) {
		diags := validateAuth(authConfig{UseOIDC: true, ClientID: "c", TenantID: "t", OIDCToken: "tok"})
		if diags.HasError() {
			t.Fatalf("unexpected error: %v", diags)
		}
	})

	t.Run("use_oidc satisfied by the request URL and token pair", func(t *testing.T) {
		diags := validateAuth(authConfig{
			UseOIDC: true, ClientID: "c", TenantID: "t",
			OIDCRequestURL: "https://example/", OIDCRequestToken: "tok",
		})
		if diags.HasError() {
			t.Fatalf("unexpected error: %v", diags)
		}
	})

	t.Run("use_oidc with the Azure Pipelines service connection alone (no oidc_request_token) is rejected", func(t *testing.T) {
		diags := validateAuth(authConfig{
			UseOIDC: true, ClientID: "c", TenantID: "t",
			OIDCAzureServiceConnectionID: "conn-id",
		})
		assertDiagContains(t, diags, "oidc_request_token")
	})

	t.Run("use_oidc with the Azure Pipelines service connection and token but SYSTEM_OIDCREQUESTURI unset is rejected", func(t *testing.T) {
		diags := validateAuth(authConfig{
			UseOIDC: true, ClientID: "c", TenantID: "t",
			OIDCAzureServiceConnectionID: "conn-id", OIDCRequestToken: "tok",
		})
		assertDiagContains(t, diags, "SYSTEM_OIDCREQUESTURI")
	})

	t.Run("use_oidc satisfied by the Azure Pipelines service connection, request token and SYSTEM_OIDCREQUESTURI together", func(t *testing.T) {
		diags := validateAuth(authConfig{
			UseOIDC: true, ClientID: "c", TenantID: "t",
			OIDCAzureServiceConnectionID: "conn-id", OIDCRequestToken: "tok",
			AzurePipelinesOIDCRequestURI: "https://example.invalid/oidc",
		})
		if diags.HasError() {
			t.Fatalf("unexpected error: %v", diags)
		}
	})

	t.Run("use_aks_workload_identity with no token file resolvable", func(t *testing.T) {
		// client_id and tenant_id are given so this isolates the token-file
		// assertion from the separate missing-client_id/tenant_id checks
		// explicitCredentialInput now also runs for use_aks_workload_identity.
		diags := validateAuth(authConfig{UseAKSWorkloadIdentity: true, ClientID: "c", TenantID: "t"})
		assertDiagContains(t, diags, "use_aks_workload_identity", "ARM_USE_AKS_WORKLOAD_IDENTITY")
	})

	t.Run("use_aks_workload_identity with no client_id or tenant_id is rejected before the credential is ever built", func(t *testing.T) {
		diags := validateAuth(authConfig{UseAKSWorkloadIdentity: true, OIDCTokenFilePath: "/token"})
		assertDiagContains(t, diags, newClientIDNotSetError().Error())
		assertDiagContains(t, diags, newTenantIDNotSetError().Error())
	})

	t.Run("AKS tenant mismatch", func(t *testing.T) {
		diags := validateAuth(authConfig{
			UseAKSWorkloadIdentity:         true,
			OIDCTokenFilePath:              "/token",
			TenantID:                       "explicit-tenant",
			AKSTenantMismatch:              true,
			AKSTenantMismatchAzureTenantID: "aks-tenant",
		})
		assertDiagContains(t, diags, "AZURE_TENANT_ID")
	})

	t.Run("use_cli false with nothing else configured is rejected", func(t *testing.T) {
		diags := validateAuth(authConfig{UseCLI: false})
		assertDiagContains(t, diags, newNoAuthMethodConfiguredError().Error())
	})

	t.Run("use_cli true with nothing else configured passes", func(t *testing.T) {
		diags := validateAuth(authConfig{UseCLI: true})
		if diags.HasError() {
			t.Fatalf("unexpected error: %v", diags)
		}
	})

	t.Run("client_id_file_path not readable", func(t *testing.T) {
		diags := validateAuth(authConfig{ClientIDFilePath: filepath.Join(t.TempDir(), "missing")})
		assertDiagContains(t, diags, "client_id_file_path", "ARM_CLIENT_ID_FILE_PATH")
	})

	t.Run("client_secret_file_path not readable", func(t *testing.T) {
		diags := validateAuth(authConfig{ClientSecretFilePath: filepath.Join(t.TempDir(), "missing")})
		assertDiagContains(t, diags, "client_secret_file_path", "ARM_CLIENT_SECRET_FILE_PATH")
	})

	t.Run("client_certificate_path not readable", func(t *testing.T) {
		diags := validateAuth(authConfig{ClientCertificatePath: filepath.Join(t.TempDir(), "missing")})
		assertDiagContains(t, diags, "client_certificate_path", "ARM_CLIENT_CERTIFICATE_PATH")
	})

	t.Run("oidc_token_file_path not readable", func(t *testing.T) {
		diags := validateAuth(authConfig{
			UseOIDC: true, ClientID: "c", TenantID: "t",
			OIDCTokenFilePath: filepath.Join(t.TempDir(), "missing"),
		})
		assertDiagContains(t, diags, "oidc_token_file_path", "ARM_OIDC_TOKEN_FILE_PATH")
	})

	t.Run("oidc_token_file_path is not checked when neither use_oidc nor use_aks_workload_identity is set", func(t *testing.T) {
		diags := validateAuth(authConfig{
			ClientID: "c", TenantID: "t", ClientSecret: "s",
			OIDCTokenFilePath: filepath.Join(t.TempDir(), "missing"),
		})
		if diags.HasError() {
			t.Fatalf("unexpected error: %v", diags)
		}
	})

	t.Run("readable file path passes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "client-id")
		if err := os.WriteFile(path, []byte("client-id-value"), 0o600); err != nil {
			t.Fatalf("writing fixture: %v", err)
		}
		// UseCLI is set purely to isolate this test's assertion (a readable
		// client_id_file_path) from the separate no-authentication-method rule.
		diags := validateAuth(authConfig{ClientIDFilePath: path, UseCLI: true})
		if diags.HasError() {
			t.Fatalf("unexpected error: %v", diags)
		}
	})

	certCases := []struct {
		name   string
		encode func(cert *x509.Certificate, key *rsa.PrivateKey, password string) []byte
	}{
		{"legacy PKCS#12", func(cert *x509.Certificate, key *rsa.PrivateKey, password string) []byte {
			data, err := pkcs12.Legacy.Encode(key, cert, nil, password)
			if err != nil {
				t.Fatalf("encoding legacy PKCS#12: %v", err)
			}
			return data
		}},
		{"modern PKCS#12", func(cert *x509.Certificate, key *rsa.PrivateKey, password string) []byte {
			data, err := pkcs12.Modern.Encode(key, cert, nil, password)
			if err != nil {
				t.Fatalf("encoding modern PKCS#12: %v", err)
			}
			return data
		}},
	}

	for _, tc := range certCases {
		t.Run(tc.name+" inline base64 with the correct password parses", func(t *testing.T) {
			cert, key := generateSelfSignedCert(t)
			data := tc.encode(cert, key, "correct-password")
			diags := validateAuth(authConfig{
				ClientID:                  "client",
				TenantID:                  "tenant",
				ClientCertificate:         encodeBase64(data),
				ClientCertificatePassword: "correct-password",
			})
			if diags.HasError() {
				t.Fatalf("unexpected error: %v", diags)
			}
		})

		t.Run(tc.name+" from a file with the correct password parses", func(t *testing.T) {
			cert, key := generateSelfSignedCert(t)
			data := tc.encode(cert, key, "correct-password")
			path := filepath.Join(t.TempDir(), "cert.pfx")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatalf("writing fixture: %v", err)
			}
			diags := validateAuth(authConfig{
				ClientID:                  "client",
				TenantID:                  "tenant",
				ClientCertificatePath:     path,
				ClientCertificatePassword: "correct-password",
			})
			if diags.HasError() {
				t.Fatalf("unexpected error: %v", diags)
			}
		})

		t.Run(tc.name+" with the wrong password is rejected, naming the password", func(t *testing.T) {
			cert, key := generateSelfSignedCert(t)
			data := tc.encode(cert, key, "correct-password")
			diags := validateAuth(authConfig{
				ClientCertificate:         encodeBase64(data),
				ClientCertificatePassword: "wrong-password",
			})
			assertDiagContains(t, diags, newClientCertificateUnparseableError("client_certificate", true).Error())
		})
	}

	t.Run("PEM with an unencrypted private key parses, inline and from a file", func(t *testing.T) {
		cert, key := generateSelfSignedCert(t)
		data := append(
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}),
			pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})...,
		)
		path := filepath.Join(t.TempDir(), "cert.pem")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("writing fixture: %v", err)
		}
		for _, cfg := range []authConfig{
			{ClientID: "client", TenantID: "tenant", ClientCertificate: encodeBase64(data)},
			{ClientID: "client", TenantID: "tenant", ClientCertificatePath: path},
		} {
			if diags := validateAuth(cfg); diags.HasError() {
				t.Fatalf("unexpected error: %v", diags)
			}
		}
	})

	t.Run("password-protected certificate with no password given names client_certificate_password", func(t *testing.T) {
		cert, key := generateSelfSignedCert(t)
		data, err := pkcs12.Modern.Encode(key, cert, nil, "some-password")
		if err != nil {
			t.Fatalf("encoding: %v", err)
		}
		diags := validateAuth(authConfig{ClientCertificate: encodeBase64(data)})
		assertDiagContains(t, diags, newClientCertificateUnparseableError("client_certificate", false).Error())
	})

	t.Run("client_certificate that is not valid base64", func(t *testing.T) {
		diags := validateAuth(authConfig{ClientCertificate: "not-base64!!"})
		assertDiagContains(t, diags, "client_certificate is not valid base64")
	})
}

func encodeBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// assertDiagContains fails unless diags contains at least one error whose
// summary or detail contains every one of substrings - not necessarily all
// in the same diagnostic, so a rule that reports the attribute and its env
// var in two separate error diagnostics still passes.
func assertDiagContains(t *testing.T, diags diag.Diagnostics, substrings ...string) {
	t.Helper()
	if !diags.HasError() {
		t.Fatalf("expected an error, got none")
	}
	var all strings.Builder
	for _, d := range diags.Errors() {
		all.WriteString(d.Summary())
		all.WriteString(" ")
		all.WriteString(d.Detail())
		all.WriteString("\n")
	}
	for _, s := range substrings {
		if !strings.Contains(all.String(), s) {
			t.Errorf("diagnostics do not mention %q:\n%s", s, all.String())
		}
	}
}

func TestBuildCredential(t *testing.T) {
	tests := []struct {
		name string
		cfg  authConfig
	}{
		{"client secret", authConfig{ClientID: "client", TenantID: "tenant", ClientSecret: "secret"}},
		{"client certificate", func() authConfig {
			cert, key := generateSelfSignedCert(t)
			data, err := pkcs12.Modern.Encode(key, cert, nil, "pw")
			if err != nil {
				t.Fatalf("encoding: %v", err)
			}
			return authConfig{ClientID: "client", TenantID: "tenant", ClientCertificate: encodeBase64(data), ClientCertificatePassword: "pw"}
		}()},
		{"oidc token", authConfig{ClientID: "client", TenantID: "tenant", UseOIDC: true, OIDCToken: "tok"}},
		{"managed identity", authConfig{UseMSI: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			ctx := tflogtest.RootLogger(context.Background(), &buf)

			cred, err := buildCredential(ctx, tt.cfg, &arm.ClientOptions{})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, ok := cred.(*azidentity.ChainedTokenCredential); !ok {
				t.Fatalf("credential type = %T, want *azidentity.ChainedTokenCredential", cred)
			}

			entries, err := tflogtest.MultilineJSONDecode(&buf)
			if err != nil {
				t.Fatalf("decoding log output: %v", err)
			}
			assertHasInfoLine(t, entries, "everpure_azure credential chain")
		})
	}

	t.Run("client secret file path is an explicit method: methods lists client_secret", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "client-secret")
		if err := os.WriteFile(path, []byte("secret-value"), 0o600); err != nil {
			t.Fatalf("writing fixture: %v", err)
		}

		var buf bytes.Buffer
		ctx := tflogtest.RootLogger(context.Background(), &buf)

		cred, err := buildCredential(ctx, authConfig{ClientID: "client", TenantID: "tenant", ClientSecretFilePath: path}, &arm.ClientOptions{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := cred.(*azidentity.ChainedTokenCredential); !ok {
			t.Fatalf("credential type = %T, want *azidentity.ChainedTokenCredential", cred)
		}

		entries, err := tflogtest.MultilineJSONDecode(&buf)
		if err != nil {
			t.Fatalf("decoding log output: %v", err)
		}
		entry := assertHasInfoLine(t, entries, "everpure_azure credential chain")
		methods, _ := entry["methods"].([]interface{})
		if !containsString(methods, "client_secret") {
			t.Errorf("methods = %v, want it to contain %q", methods, "client_secret")
		}
	})

	t.Run("nothing configured chains Azure CLI only", func(t *testing.T) {
		var buf bytes.Buffer
		ctx := tflogtest.RootLogger(context.Background(), &buf)

		cred, err := buildCredential(ctx, authConfig{UseCLI: true}, &arm.ClientOptions{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := cred.(*azidentity.ChainedTokenCredential); !ok {
			t.Fatalf("credential type = %T, want *azidentity.ChainedTokenCredential", cred)
		}

		entries, err := tflogtest.MultilineJSONDecode(&buf)
		if err != nil {
			t.Fatalf("decoding log output: %v", err)
		}
		entry := assertHasInfoLine(t, entries, "everpure_azure credential chain")
		methods, _ := entry["methods"].([]interface{})
		if len(methods) != 1 {
			t.Fatalf("methods = %v, want exactly [\"azure_cli\"]", methods)
		}
		if s, ok := methods[0].(string); !ok || s != "azure_cli" {
			t.Errorf("methods = %v, want [\"azure_cli\"]", methods)
		}
	})

	t.Run("the library's own skip lines arrive at debug level, never above", func(t *testing.T) {
		var buf bytes.Buffer
		ctx := tflogtest.RootLogger(context.Background(), &buf)

		// Configuring only a client secret means aztfauth logs a "failed to
		// build ... cred option" line for every other method it was not
		// asked to use (OIDC, certificate, MSI): normal operation, not an
		// error, so it must never appear above debug.
		if _, err := buildCredential(ctx, authConfig{ClientID: "client", TenantID: "tenant", ClientSecret: "secret", UseCLI: true}, &arm.ClientOptions{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		entries, err := tflogtest.MultilineJSONDecode(&buf)
		if err != nil {
			t.Fatalf("decoding log output: %v", err)
		}
		for _, e := range entries {
			level, _ := e["@level"].(string)
			msg, _ := e["@message"].(string)
			if strings.Contains(msg, "cred option") && level != "debug" {
				t.Errorf("library log line %q arrived at level %q, want debug", msg, level)
			}
		}
	})
}

// TestBuildCredentialLogsMethodSourcesWithoutLeakingValues drives the
// resolveAuth -> buildCredential path (rather than building an authConfig by
// hand) with a mixed config - client_secret from the provider block,
// use_oidc and its token both from the environment - so the credential
// chain's log line can be checked against the actual source each method
// came from, not a hand-set field. It also confirms neither secret's value
// itself ever reaches the log: method_sources exists precisely so an
// operator never has to look at the values to tell block from environment.
func TestBuildCredentialLogsMethodSourcesWithoutLeakingValues(t *testing.T) {
	const secretValue = "config-supplied-client-secret"   //nolint:gosec // asserted absent from the log output below, not a real credential.
	const tokenValue = "env-supplied-oidc-token-value123" //nolint:gosec // asserted absent from the log output below, not a real credential.

	config := providerModel{
		ClientID:     types.StringValue("client"),
		TenantID:     types.StringValue("tenant"),
		ClientSecret: types.StringValue(secretValue),
	}
	env := map[string]string{
		"ARM_USE_OIDC":   "true",
		"ARM_OIDC_TOKEN": tokenValue,
	}

	cfg := resolveAuth(config, lookupEnvFrom(env))

	var buf bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &buf)
	if _, err := buildCredential(ctx, cfg, &arm.ClientOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entries, err := tflogtest.MultilineJSONDecode(&buf)
	if err != nil {
		t.Fatalf("decoding log output: %v", err)
	}
	entry := assertHasInfoLine(t, entries, "everpure_azure credential chain")

	sources, ok := entry["method_sources"].(map[string]interface{})
	if !ok {
		t.Fatalf("method_sources = %v (%T), want a map", entry["method_sources"], entry["method_sources"])
	}
	if got := sources["client_secret"]; got != "config" {
		t.Errorf("method_sources[client_secret] = %v, want %q", got, "config")
	}
	if got := sources["oidc_token"]; got != "env:ARM_OIDC_TOKEN" {
		t.Errorf("method_sources[oidc_token] = %v, want %q", got, "env:ARM_OIDC_TOKEN")
	}

	if strings.Contains(buf.String(), secretValue) {
		t.Error("log output contains the client secret's own value, not just where it came from")
	}
	if strings.Contains(buf.String(), tokenValue) {
		t.Error("log output contains the OIDC token's own value, not just where it came from")
	}
}

func assertHasInfoLine(t *testing.T, entries []map[string]interface{}, message string) map[string]interface{} {
	t.Helper()
	for _, e := range entries {
		msg, _ := e["@message"].(string)
		level, _ := e["@level"].(string)
		if msg == message {
			if level != "info" {
				t.Errorf("log line %q arrived at level %q, want info", message, level)
			}
			return e
		}
	}
	t.Fatalf("no log line %q found in %v", message, entries)
	return nil
}

// methodCase describes one authentication method's minimal valid
// configuration. It backs TestAuthMethodsAgainstLibrary,
// TestAuthMethodsCrossUseCLI and TestAuthMethodsBrokenInputsAreRejectedBeforeBuild,
// so every one of those exercises the exact same set of methods.
type methodCase struct {
	name string
	// buildCfg constructs a minimal valid authConfig for this method. It
	// takes t so it can allocate temp files and scoped environment
	// variables (t.Setenv) for the calling subtest.
	buildCfg func(t *testing.T) authConfig
	// expectMethod is the name configuredMethodNames is expected to report
	// for this config.
	expectMethod string
	// failureSubstrings are the exact aztfauth/entrauth log message
	// fragments that only appear when THIS method's own credential option
	// failed to build or its credential failed to construct (see
	// aztfauth.go's "failed to build ... cred option" lines and
	// entrauth.go's "failed to new ... credential" lines). A method the
	// config did not configure logs its own failure line every run - that's
	// expected - so tests must only look for these specific fragments.
	failureSubstrings []string
	// successSubstring is the exact entrauth.go "new ... credential
	// succeeded" log line for this method. A test that only checks the
	// absence of failureSubstrings would pass even if the library renamed
	// or dropped that failure message entirely; asserting this line is
	// actually present catches that silently.
	successSubstring string
	// mutations each remove exactly one input this method needs. They back
	// TestAuthMethodsBrokenInputsAreRejectedBeforeBuild's check that
	// validateAuth actually rejects every config it must reject - not just
	// the ones it happens to catch today - before buildCredential ever
	// silently drops the method from the chain.
	mutations []methodMutation
}

// methodMutation is one way to break a methodCase's otherwise-valid config.
type methodMutation struct {
	name string
	// apply removes one required input from cfg. It takes t so it can
	// change environment variables (t.Setenv) alongside the field, for
	// methods that also read the environment directly (e.g. Azure Pipelines
	// via SYSTEM_OIDCREQUESTURI).
	apply func(t *testing.T, cfg *authConfig)
}

// methodCases returns one entry per credential method aztfauth supports,
// each already valid enough that buildCredential should attach it to the
// chain without error.
func methodCases(t *testing.T) []methodCase {
	t.Helper()

	secretFilePath := filepath.Join(t.TempDir(), "client-secret")
	if err := os.WriteFile(secretFilePath, []byte("secret-value"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	oidcTokenFilePath := filepath.Join(t.TempDir(), "oidc-token")
	if err := os.WriteFile(oidcTokenFilePath, []byte("token-value"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	aksTokenFilePath := filepath.Join(t.TempDir(), "aks-token")
	if err := os.WriteFile(aksTokenFilePath, []byte("token-value"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	cert, key := generateSelfSignedCert(t)
	certData, err := pkcs12.Modern.Encode(key, cert, nil, "pw")
	if err != nil {
		t.Fatalf("encoding certificate: %v", err)
	}

	return []methodCase{
		{
			name: "client secret",
			buildCfg: func(*testing.T) authConfig {
				return authConfig{ClientID: "client", TenantID: "tenant", ClientSecret: "secret"}
			},
			expectMethod:      "client_secret",
			failureSubstrings: []string{"failed to build client secret cred option", "failed to new client secret credential"},
			successSubstring:  "new client secret credential succeeded",
			mutations: []methodMutation{
				{"drop ClientID", func(_ *testing.T, cfg *authConfig) { cfg.ClientID = "" }},
				{"drop TenantID", func(_ *testing.T, cfg *authConfig) { cfg.TenantID = "" }},
			},
		},
		{
			name: "client secret file",
			buildCfg: func(*testing.T) authConfig {
				return authConfig{ClientID: "client", TenantID: "tenant", ClientSecretFilePath: secretFilePath}
			},
			expectMethod:      "client_secret",
			failureSubstrings: []string{"failed to build client secret cred option", "failed to new client secret credential"},
			successSubstring:  "new client secret credential succeeded",
		},
		{
			name: "client certificate",
			buildCfg: func(*testing.T) authConfig {
				return authConfig{
					ClientID: "client", TenantID: "tenant",
					ClientCertificate: encodeBase64(certData), ClientCertificatePassword: "pw",
				}
			},
			expectMethod:      "client_certificate",
			failureSubstrings: []string{"failed to build client certificate cred option", "failed to new client certificate credential"},
			successSubstring:  "new client certificate credential succeeded",
			mutations: []methodMutation{
				{"drop TenantID", func(_ *testing.T, cfg *authConfig) { cfg.TenantID = "" }},
				{"drop password", func(_ *testing.T, cfg *authConfig) { cfg.ClientCertificatePassword = "" }},
			},
		},
		{
			name: "oidc token",
			buildCfg: func(*testing.T) authConfig {
				return authConfig{ClientID: "client", TenantID: "tenant", UseOIDC: true, OIDCToken: "tok"}
			},
			expectMethod:      "oidc_token",
			failureSubstrings: []string{"failed to build oidc token cred option", "failed to new plain assertion credential"},
			successSubstring:  "new plain assertion credential succeeded",
			mutations: []methodMutation{
				{"drop ClientID", func(_ *testing.T, cfg *authConfig) { cfg.ClientID = "" }},
				{"drop TenantID", func(_ *testing.T, cfg *authConfig) { cfg.TenantID = "" }},
			},
		},
		{
			name: "oidc token file",
			buildCfg: func(*testing.T) authConfig {
				return authConfig{ClientID: "client", TenantID: "tenant", UseOIDC: true, OIDCTokenFilePath: oidcTokenFilePath}
			},
			expectMethod: "oidc_token_file",
			failureSubstrings: []string{
				"failed to build oidc token file cred option", "failed to new file (based) assertion credential",
			},
			successSubstring: "new file (based) assertion credential succeeded",
			mutations: []methodMutation{
				{"drop TenantID", func(_ *testing.T, cfg *authConfig) { cfg.TenantID = "" }},
				{"point OIDCTokenFilePath at a nonexistent path", func(t *testing.T, cfg *authConfig) {
					cfg.OIDCTokenFilePath = filepath.Join(t.TempDir(), "missing")
				}},
			},
		},
		{
			name: "oidc request, GitHub Actions variant",
			buildCfg: func(*testing.T) authConfig {
				return authConfig{
					ClientID: "client", TenantID: "tenant", UseOIDC: true,
					OIDCRequestURL: "https://example.invalid/token", OIDCRequestToken: "tok",
				}
			},
			expectMethod: "oidc_token_request",
			failureSubstrings: []string{
				"failed to build oidc token request cred option",
				"failed to new request (based) assertion for Github credential",
			},
			successSubstring: "new request (based) assertion for Github credential succeeded",
			mutations: []methodMutation{
				{"drop OIDCRequestURL", func(_ *testing.T, cfg *authConfig) { cfg.OIDCRequestURL = "" }},
				{"drop OIDCRequestToken", func(_ *testing.T, cfg *authConfig) { cfg.OIDCRequestToken = "" }},
				{"drop TenantID", func(_ *testing.T, cfg *authConfig) { cfg.TenantID = "" }},
			},
		},
		{
			name: "oidc request, Azure Pipelines variant",
			buildCfg: func(t *testing.T) authConfig {
				// azidentity.NewAzurePipelinesCredential reads its OIDC
				// request endpoint from this environment variable directly;
				// it isn't one of the provider's own attributes.
				t.Setenv("SYSTEM_OIDCREQUESTURI", "https://example.invalid/oidc")
				return authConfig{
					ClientID: "client", TenantID: "tenant", UseOIDC: true,
					OIDCAzureServiceConnectionID: "conn-id", OIDCRequestToken: "tok",
					AzurePipelinesOIDCRequestURI: "https://example.invalid/oidc",
				}
			},
			expectMethod: "oidc_token_request",
			failureSubstrings: []string{
				"failed to build oidc token request cred option",
				"failed to new request (based) assertion for AzureDevOps credential",
			},
			successSubstring: "new request (based) assertion for AzureDevOps credential succeeded",
			mutations: []methodMutation{
				{"drop OIDCRequestToken", func(_ *testing.T, cfg *authConfig) { cfg.OIDCRequestToken = "" }},
				{"drop TenantID", func(_ *testing.T, cfg *authConfig) { cfg.TenantID = "" }},
				{"SYSTEM_OIDCREQUESTURI unset", func(t *testing.T, cfg *authConfig) {
					t.Setenv("SYSTEM_OIDCREQUESTURI", "")
					cfg.AzurePipelinesOIDCRequestURI = ""
				}},
			},
		},
		{
			name: "managed identity",
			buildCfg: func(*testing.T) authConfig {
				return authConfig{UseMSI: true}
			},
			expectMethod:      "managed_identity",
			failureSubstrings: []string{"failed to build MSI cred option", "failed to new managed identity credential"},
			successSubstring:  "new managed identity credential succeeded",
			mutations: []methodMutation{
				// A user-assigned identity's client_id is optional: dropping
				// it (it's already unset here) still selects the VM/pod's
				// system-assigned identity, so this must still build.
				{"drop ClientID", func(_ *testing.T, cfg *authConfig) { cfg.ClientID = "" }},
			},
		},
		{
			name: "AKS workload identity",
			buildCfg: func(*testing.T) authConfig {
				return authConfig{
					UseAKSWorkloadIdentity: true, ClientID: "aks-client", TenantID: "aks-tenant",
					OIDCTokenFilePath: aksTokenFilePath,
				}
			},
			// AKS workload identity is wired through the same OIDC token
			// file mechanism as use_oidc + oidc_token_file_path (see
			// buildCredential's UseOIDCTokenFile), so it reports the same
			// method name and the same failure/success fragments.
			expectMethod: "oidc_token_file",
			failureSubstrings: []string{
				"failed to build oidc token file cred option", "failed to new file (based) assertion credential",
			},
			successSubstring: "new file (based) assertion credential succeeded",
			mutations: []methodMutation{
				{"drop ClientID", func(_ *testing.T, cfg *authConfig) { cfg.ClientID = "" }},
				{"drop TenantID", func(_ *testing.T, cfg *authConfig) { cfg.TenantID = "" }},
				{"point OIDCTokenFilePath at a nonexistent path", func(t *testing.T, cfg *authConfig) {
					cfg.OIDCTokenFilePath = filepath.Join(t.TempDir(), "missing")
				}},
			},
		},
	}
}

// containsString reports whether items (as decoded from a JSON log field by
// tflogtest, i.e. []interface{} of strings) contains want.
func containsString(items []interface{}, want string) bool {
	for _, it := range items {
		if s, ok := it.(string); ok && s == want {
			return true
		}
	}
	return false
}

// hasLogLineContaining reports whether any decoded log entry's message
// contains substr.
func hasLogLineContaining(entries []map[string]interface{}, substr string) bool {
	for _, e := range entries {
		if msg, _ := e["@message"].(string); strings.Contains(msg, substr) {
			return true
		}
	}
	return false
}

// TestAuthMethodsAgainstLibrary is G1: for every supported credential
// method's minimal valid configuration, validateAuth must accept it,
// buildCredential must construct a credential without error, no debug log
// line may report that the target method itself failed to build or
// construct, and the "everpure_azure credential chain" info line must name
// it as configured.
func TestAuthMethodsAgainstLibrary(t *testing.T) {
	for _, tc := range methodCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.buildCfg(t)

			if diags := validateAuth(cfg); diags.HasError() {
				t.Fatalf("validateAuth: unexpected error: %v", diags)
			}

			var buf bytes.Buffer
			ctx := tflogtest.RootLogger(context.Background(), &buf)
			cred, err := buildCredential(ctx, cfg, &arm.ClientOptions{})
			if err != nil {
				t.Fatalf("buildCredential: unexpected error: %v", err)
			}
			if cred == nil {
				t.Fatal("buildCredential: returned a nil credential")
			}

			entries, err := tflogtest.MultilineJSONDecode(&buf)
			if err != nil {
				t.Fatalf("decoding log output: %v", err)
			}

			for _, e := range entries {
				msg, _ := e["@message"].(string)
				if !strings.Contains(msg, "failed") {
					continue
				}
				for _, kw := range tc.failureSubstrings {
					if strings.Contains(msg, kw) {
						t.Errorf("log line reports a failure building/constructing the %s method's own credential: %q", tc.name, msg)
					}
				}
			}

			// Checking only the absence of a failure line would still pass if
			// entrauth ever renamed or dropped that message - a presence
			// check on its "succeeded" counterpart fails loudly instead.
			if !hasLogLineContaining(entries, tc.successSubstring) {
				t.Errorf("no log line contains %q: the %s method's credential may never actually have been constructed", tc.successSubstring, tc.name)
			}

			entry := assertHasInfoLine(t, entries, "everpure_azure credential chain")
			methods, _ := entry["methods"].([]interface{})
			if !containsString(methods, tc.expectMethod) {
				t.Errorf("methods = %v, want it to contain %q", methods, tc.expectMethod)
			}
		})
	}
}

// TestAuthMethodsCrossUseCLI is G2: every method from methodCases, crossed
// with use_cli true/false. Every combination must chain to a
// *azidentity.ChainedTokenCredential with the method present, and azure_cli
// must appear in the chain's methods precisely when use_cli is true.
func TestAuthMethodsCrossUseCLI(t *testing.T) {
	for _, tc := range methodCases(t) {
		for _, useCLI := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/use_cli=%v", tc.name, useCLI), func(t *testing.T) {
				cfg := tc.buildCfg(t)
				cfg.UseCLI = useCLI

				var buf bytes.Buffer
				ctx := tflogtest.RootLogger(context.Background(), &buf)
				cred, err := buildCredential(ctx, cfg, &arm.ClientOptions{})
				if err != nil {
					t.Fatalf("buildCredential: unexpected error: %v", err)
				}
				if _, ok := cred.(*azidentity.ChainedTokenCredential); !ok {
					t.Fatalf("credential type = %T, want *azidentity.ChainedTokenCredential", cred)
				}

				entries, err := tflogtest.MultilineJSONDecode(&buf)
				if err != nil {
					t.Fatalf("decoding log output: %v", err)
				}
				entry := assertHasInfoLine(t, entries, "everpure_azure credential chain")
				methods, _ := entry["methods"].([]interface{})
				if len(methods) == 0 {
					t.Error("methods is empty, want at least the configured method")
				}
				if !containsString(methods, tc.expectMethod) {
					t.Errorf("methods = %v, want it to contain %q", methods, tc.expectMethod)
				}
				if got := containsString(methods, "azure_cli"); got != useCLI {
					t.Errorf("methods = %v, azure_cli present = %v, want %v", methods, got, useCLI)
				}
			})
		}
	}
}

// TestAuthMethodsBrokenInputsAreRejectedBeforeBuild is G1b: the property
// under test is "if validateAuth accepts a config that still counts as an
// explicit method, the library must actually build that method." Each
// mutation removes exactly one input a method needs. If the method is no
// longer configured at all after the mutation, the property has nothing to
// say (skipped). Otherwise: if validateAuth rejects the broken config, the
// property holds trivially. If validateAuth accepts it, buildCredential must
// still produce a working credential for it - checked the same way G1 does,
// via the method's success line and the absence of its failure lines -
// because a config validateAuth waves through but aztfauth silently drops
// fails exactly as badly as a hard error, just later and less legibly.
func TestAuthMethodsBrokenInputsAreRejectedBeforeBuild(t *testing.T) {
	for _, tc := range methodCases(t) {
		for _, mut := range tc.mutations {
			t.Run(tc.name+"/"+mut.name, func(t *testing.T) {
				cfg := tc.buildCfg(t)
				mut.apply(t, &cfg)

				stillConfigured := false
				for _, m := range configuredMethodNames(cfg) {
					if m == tc.expectMethod {
						stillConfigured = true
						break
					}
				}
				if !stillConfigured {
					t.Skip("mutation removed the method entirely; the property does not apply")
				}

				if diags := validateAuth(cfg); diags.HasError() {
					return // validateAuth correctly rejected the broken config.
				}

				var buf bytes.Buffer
				ctx := tflogtest.RootLogger(context.Background(), &buf)
				if _, err := buildCredential(ctx, cfg, &arm.ClientOptions{}); err != nil {
					t.Fatalf("buildCredential: unexpected error: %v", err)
				}

				entries, err := tflogtest.MultilineJSONDecode(&buf)
				if err != nil {
					t.Fatalf("decoding log output: %v", err)
				}

				if !hasLogLineContaining(entries, tc.successSubstring) {
					t.Errorf(
						"validateAuth accepted a broken %s config (mutation %q) but no log line contains %q: "+
							"the credential was never actually built",
						tc.name, mut.name, tc.successSubstring,
					)
				}
				for _, e := range entries {
					msg, _ := e["@message"].(string)
					if !strings.Contains(msg, "failed") {
						continue
					}
					for _, kw := range tc.failureSubstrings {
						if strings.Contains(msg, kw) {
							t.Errorf(
								"validateAuth accepted a broken %s config (mutation %q) and the credential silently failed to build: %q",
								tc.name, mut.name, msg,
							)
						}
					}
				}
			})
		}
	}
}

// TestResolveAuthNoisyOIDCEnvironment is G3: a client-secret provider
// configuration must still pass validateAuth even when every OIDC-related
// environment variable is set to noise, none of it meaningful because
// use_oidc was never turned on. This specifically exercises the
// oidc_token_file_path check, gated in validateAuth to run only when
// use_oidc or use_aks_workload_identity is set: ARM_OIDC_TOKEN_FILE_PATH
// here points at a path that does not exist.
func TestResolveAuthNoisyOIDCEnvironment(t *testing.T) {
	config := providerModel{
		ClientID:     types.StringValue("client"),
		TenantID:     types.StringValue("tenant"),
		ClientSecret: types.StringValue("secret"),
	}
	env := map[string]string{
		"ARM_OIDC_TOKEN_FILE_PATH":               "/nonexistent/path",
		"ARM_OIDC_TOKEN":                         "garbage",
		"ARM_OIDC_REQUEST_TOKEN":                 "garbage",
		"ARM_OIDC_REQUEST_URL":                   "garbage",
		"ARM_ADO_PIPELINE_SERVICE_CONNECTION_ID": "garbage",
		"AZURE_FEDERATED_TOKEN_FILE":             "garbage",
	}

	cfg := resolveAuth(config, lookupEnvFrom(env))
	if diags := validateAuth(cfg); diags.HasError() {
		t.Fatalf("unexpected error: %v", diags)
	}
}
