// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azcorefake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	psbfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock/fake"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

var (
	regexpClientIDMissing = regexp.MustCompile(`(?s)client_id.*ARM_CLIENT_ID`)
	regexpIncompleteOIDC  = diagRegexp(operationConfigureProvider.fail(newOidcInputsNotSetError()))
)

// authConfigRecorder is the credentialFactory these tests inject: it stands
// in for buildCredential so a full terraform run through Configure can be
// asserted against, without ever exercising the real aztfauth/azidentity
// constructors (auth_unit_test.go already covers buildCredential itself in
// isolation). It always succeeds with a fake credential, so the resource
// lifecycle underneath proceeds through the fake ARM transport exactly as
// the other integration tests already do.
type authConfigRecorder struct {
	mu     sync.Mutex
	called bool
	cfg    authConfig
}

func (r *authConfigRecorder) factory() credentialFactory {
	return func(_ context.Context, cfg authConfig, _ *arm.ClientOptions) (azcore.TokenCredential, error) {
		r.mu.Lock()
		r.called = true
		r.cfg = cfg
		r.mu.Unlock()
		return &azcorefake.TokenCredential{}, nil
	}
}

func (r *authConfigRecorder) config(t *testing.T) authConfig {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.called {
		t.Fatal("expected the credential factory to have been called")
	}
	return r.cfg
}

// newAuthTestProviderFactories is newTestProviderFactories's twin for the
// auth tests below: same fresh fake ARM backing store, but wired to a
// caller-supplied credentialFactory instead of a fixed fake credential, so a
// test can inspect what Configure resolved.
func newAuthTestProviderFactories(factory credentialFactory) (map[string]func() (tfprotov6.ProviderServer, error), *fakeStoragePoolStore) {
	store := newFakeStoragePoolStore(testSubscriptionID)
	srv := store.newStoragePoolsServer()
	transport := psbfake.NewStoragePoolsServerTransport(&srv)

	opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: transport}}
	p := newProvider("test", factory, opts)()

	return map[string]func() (tfprotov6.ProviderServer, error){
		"everpure": providerserver.NewProtocol6WithError(p),
	}, store
}

// authEnvKeys is every environment variable resolveAuth ever consults.
// clearAuthEnv sets each to "" (resolveSetting treats an empty string as
// unset, same as an absent config attribute), so a test asserting on
// "nothing configured" isn't at the mercy of ARM_*/AZURE_* variables already
// present in whatever environment runs the test.
var authEnvKeys = []string{
	"ARM_TENANT_ID", "AZURE_TENANT_ID",
	"ARM_CLIENT_ID", "AZURE_CLIENT_ID",
	"ARM_CLIENT_ID_FILE_PATH",
	"ARM_CLIENT_SECRET", "AZURE_CLIENT_SECRET",
	"ARM_CLIENT_SECRET_FILE_PATH",
	"ARM_CLIENT_CERTIFICATE",
	"ARM_CLIENT_CERTIFICATE_PATH", "AZURE_CLIENT_CERTIFICATE_PATH",
	"ARM_CLIENT_CERTIFICATE_PASSWORD", "AZURE_CLIENT_CERTIFICATE_PASSWORD",
	"ARM_USE_OIDC",
	"ARM_OIDC_TOKEN",
	"ARM_OIDC_TOKEN_FILE_PATH", "AZURE_FEDERATED_TOKEN_FILE",
	"ARM_OIDC_REQUEST_TOKEN", "ACTIONS_ID_TOKEN_REQUEST_TOKEN", "SYSTEM_ACCESSTOKEN",
	"ARM_OIDC_REQUEST_URL", "ACTIONS_ID_TOKEN_REQUEST_URL",
	"ARM_ADO_PIPELINE_SERVICE_CONNECTION_ID", "ARM_OIDC_AZURE_SERVICE_CONNECTION_ID", "AZURESUBSCRIPTION_SERVICE_CONNECTION_ID",
	"ARM_USE_CLI",
	"ARM_USE_MSI",
	"ARM_USE_AKS_WORKLOAD_IDENTITY",
	"ARM_AUXILIARY_TENANT_IDS",
}

// clearAuthEnv blanks every env var resolveAuth ever consults, so a test
// asserting on "nothing configured" isn't at the mercy of ARM_*/AZURE_*
// variables already present in whatever environment runs the test.
// resolveSetting and resolveBoolSetting both treat a present-but-empty
// variable as unset, so t.Setenv(key, "") is enough - no need to actually
// remove the variable from the environment.
func clearAuthEnv(t *testing.T) {
	t.Helper()
	for _, key := range authEnvKeys {
		t.Setenv(key, "")
	}
}

// minimalPoolConfig is just enough HCL for a storage pool create to succeed
// against the fake ARM server; these tests care about what Configure
// resolved, not about the resource's own attributes.
func minimalPoolConfig(subnetID string) string {
	return fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }
}
`, testResourceGroup, testReservationID(), subnetID)
}

func TestAuthProviderBlockAttributes(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}
	clearAuthEnv(t)

	recorder := &authConfigRecorder{}
	factories, _ := newAuthTestProviderFactories(recorder.factory())

	config := fmt.Sprintf(`
provider "everpure" {
  subscription_id = %[1]q
  tenant_id       = "11111111-1111-1111-1111-111111111111"
  client_id       = "22222222-2222-2222-2222-222222222222"
  client_secret   = "shh-its-a-secret"
}
`, testSubscriptionID) + minimalPoolConfig(testSubnetID("snet1"))

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps:                    []resource.TestStep{{Config: config}},
	})

	got := recorder.config(t)
	if got.TenantID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("TenantID = %q, want the configured value", got.TenantID)
	}
	if got.ClientID != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("ClientID = %q, want the configured value", got.ClientID)
	}
	if got.ClientSecret != "shh-its-a-secret" {
		t.Errorf("ClientSecret = %q, want the configured value", got.ClientSecret)
	}
}

func TestAuthARMEnvVars(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	t.Run("resolves entirely from ARM_* environment variables when nothing is configured", func(t *testing.T) {
		clearAuthEnv(t)
		t.Setenv("ARM_TENANT_ID", "11111111-1111-1111-1111-111111111111")
		t.Setenv("ARM_CLIENT_ID", "22222222-2222-2222-2222-222222222222")
		t.Setenv("ARM_CLIENT_SECRET", "shh-its-a-secret")

		recorder := &authConfigRecorder{}
		factories, _ := newAuthTestProviderFactories(recorder.factory())
		config := providerBlock() + minimalPoolConfig(testSubnetID("snet1"))

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps:                    []resource.TestStep{{Config: config}},
		})

		got := recorder.config(t)
		if got.TenantID != "11111111-1111-1111-1111-111111111111" {
			t.Errorf("TenantID = %q, want the ARM_TENANT_ID value", got.TenantID)
		}
		if got.ClientID != "22222222-2222-2222-2222-222222222222" {
			t.Errorf("ClientID = %q, want the ARM_CLIENT_ID value", got.ClientID)
		}
		if got.ClientSecret != "shh-its-a-secret" {
			t.Errorf("ClientSecret = %q, want the ARM_CLIENT_SECRET value", got.ClientSecret)
		}
	})

	t.Run("a configured attribute wins for just that field, leaving the rest on their env fallback", func(t *testing.T) {
		clearAuthEnv(t)
		t.Setenv("ARM_TENANT_ID", "11111111-1111-1111-1111-111111111111")
		t.Setenv("ARM_CLIENT_ID", "22222222-2222-2222-2222-222222222222")
		t.Setenv("ARM_CLIENT_SECRET", "shh-its-a-secret")

		recorder := &authConfigRecorder{}
		factories, _ := newAuthTestProviderFactories(recorder.factory())
		config := fmt.Sprintf(`
provider "everpure" {
  subscription_id = %[1]q
  client_id       = "33333333-3333-3333-3333-333333333333"
}
`, testSubscriptionID) + minimalPoolConfig(testSubnetID("snet1"))

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps:                    []resource.TestStep{{Config: config}},
		})

		got := recorder.config(t)
		if got.ClientID != "33333333-3333-3333-3333-333333333333" {
			t.Errorf("ClientID = %q, want the attribute's value to win", got.ClientID)
		}
		if got.TenantID != "11111111-1111-1111-1111-111111111111" {
			t.Errorf("TenantID = %q, want the untouched ARM_TENANT_ID value", got.TenantID)
		}
		if got.ClientSecret != "shh-its-a-secret" {
			t.Errorf("ClientSecret = %q, want the untouched ARM_CLIENT_SECRET value", got.ClientSecret)
		}
	})
}

func TestAuthARMBeatsAzureEnvVar(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}
	clearAuthEnv(t)
	t.Setenv("ARM_CLIENT_ID", "arm-client-id")
	t.Setenv("AZURE_CLIENT_ID", "azure-client-id")

	recorder := &authConfigRecorder{}
	factories, _ := newAuthTestProviderFactories(recorder.factory())
	config := providerBlock() + minimalPoolConfig(testSubnetID("snet1"))

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps:                    []resource.TestStep{{Config: config}},
	})

	if got := recorder.config(t).ClientID; got != "arm-client-id" {
		t.Errorf("ClientID = %q, want ARM_CLIENT_ID to win over AZURE_CLIENT_ID", got)
	}
}

func TestAuthNothingConfiguredDefaultsToAzureCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}
	clearAuthEnv(t)

	recorder := &authConfigRecorder{}
	factories, _ := newAuthTestProviderFactories(recorder.factory())
	config := providerBlock() + minimalPoolConfig(testSubnetID("snet1"))

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps:                    []resource.TestStep{{Config: config}},
	})

	got := recorder.config(t)
	if got.TenantID != "" || got.ClientID != "" || got.ClientSecret != "" {
		t.Errorf("expected an empty authConfig, got %+v", got)
	}
	if !got.UseCLI {
		t.Error("expected use_cli to still default to true")
	}
	if methods := configuredMethodNames(got); len(methods) != 1 || methods[0] != "azure_cli" {
		t.Errorf("configuredMethodNames = %v, want [\"azure_cli\"]", methods)
	}
}

func TestAuthClientSecretWithoutClientIDIsRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}
	clearAuthEnv(t)

	factories, _ := newAuthTestProviderFactories((&authConfigRecorder{}).factory())
	config := fmt.Sprintf(`
provider "everpure" {
  subscription_id = %[1]q
  tenant_id       = "11111111-1111-1111-1111-111111111111"
  client_secret   = "shh-its-a-secret"
}
`, testSubscriptionID) + minimalPoolConfig(testSubnetID("snet1"))

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: regexpClientIDMissing,
			},
		},
	})
}

func TestAuthUseOIDCWithNoInputsIsRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}
	clearAuthEnv(t)

	factories, _ := newAuthTestProviderFactories((&authConfigRecorder{}).factory())
	config := fmt.Sprintf(`
provider "everpure" {
  subscription_id = %[1]q
  tenant_id       = "11111111-1111-1111-1111-111111111111"
  client_id       = "22222222-2222-2222-2222-222222222222"
  use_oidc        = true
}
`, testSubscriptionID) + minimalPoolConfig(testSubnetID("snet1"))

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: regexpIncompleteOIDC,
			},
		},
	})
}

func TestAuthAKSWorkloadIdentityDefaults(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}
	clearAuthEnv(t)

	// validateAuth actually opens oidc_token_file_path, so the fixture must
	// be a real, readable file - not the literal pod-mount path AKS uses in
	// production - for Configure to get past validation and reach the
	// credential factory this test inspects.
	tokenFile := filepath.Join(t.TempDir(), "azure-identity-token")
	if err := os.WriteFile(tokenFile, []byte("fake-token"), 0o600); err != nil {
		t.Fatalf("writing token fixture: %v", err)
	}

	t.Setenv("AZURE_CLIENT_ID", "aks-client-id")
	t.Setenv("AZURE_TENANT_ID", "aks-tenant-id")
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", tokenFile)

	recorder := &authConfigRecorder{}
	factories, _ := newAuthTestProviderFactories(recorder.factory())
	config := fmt.Sprintf(`
provider "everpure" {
  subscription_id           = %[1]q
  use_aks_workload_identity = true
}
`, testSubscriptionID) + minimalPoolConfig(testSubnetID("snet1"))

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps:                    []resource.TestStep{{Config: config}},
	})

	got := recorder.config(t)
	if got.ClientID != "aks-client-id" {
		t.Errorf("ClientID = %q, want the AZURE_CLIENT_ID value", got.ClientID)
	}
	if got.TenantID != "aks-tenant-id" {
		t.Errorf("TenantID = %q, want the AZURE_TENANT_ID value", got.TenantID)
	}
	if got.OIDCTokenFilePath != tokenFile {
		t.Errorf("OIDCTokenFilePath = %q, want the AZURE_FEDERATED_TOKEN_FILE value %q", got.OIDCTokenFilePath, tokenFile)
	}
}

// TestAuthAuxiliaryTenantsHeader confirms auxiliary_tenant_ids reaches the ARM
// pipeline, not just the credential: ARM only honours a cross-tenant request
// when it carries an x-ms-authorization-auxiliary header holding one bearer
// token per auxiliary tenant, and the SDK's bearer policy only adds that header
// when arm.ClientOptions.AuxiliaryTenants is set.
func TestAuthAuxiliaryTenantsHeader(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	run := func(t *testing.T, providerExtra string) []capturedRequest {
		t.Helper()
		clearAuthEnv(t)

		store := newFakeStoragePoolStore(testSubscriptionID)
		srv := store.newStoragePoolsServer()
		capture := &capturingTransport{inner: psbfake.NewStoragePoolsServerTransport(&srv)}
		opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: capture}}
		p := newProvider("test", fixedCredentialFactory(&azcorefake.TokenCredential{}), opts)()

		config := fmt.Sprintf(`
provider "everpure" {
  subscription_id = %[1]q
  %[2]s
}
`, testSubscriptionID, providerExtra) + minimalPoolConfig(testSubnetID("snet1"))

		resource.Test(t, resource.TestCase{
			IsUnitTest: true,
			ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){
				"everpure": providerserver.NewProtocol6WithError(p),
			},
			Steps: []resource.TestStep{{Config: config}},
		})

		capture.mu.Lock()
		defer capture.mu.Unlock()
		if len(capture.requests) == 0 {
			t.Fatal("expected at least one ARM request")
		}
		return append([]capturedRequest(nil), capture.requests...)
	}

	t.Run("every request carries one auxiliary token per configured tenant", func(t *testing.T) {
		requests := run(t, `auxiliary_tenant_ids = ["33333333-3333-3333-3333-333333333333", "44444444-4444-4444-4444-444444444444"]`)
		for i, r := range requests {
			if got := strings.Count(r.auxiliaryAuthorization, "Bearer "); got != 2 {
				t.Fatalf("request %d (%s %s) x-ms-authorization-auxiliary = %q, want 2 bearer tokens",
					i, r.method, r.path, r.auxiliaryAuthorization)
			}
		}
	})

	t.Run("no header when auxiliary_tenant_ids is unset", func(t *testing.T) {
		for i, r := range run(t, "") {
			if r.auxiliaryAuthorization != "" {
				t.Fatalf("request %d (%s %s) unexpectedly carried x-ms-authorization-auxiliary = %q",
					i, r.method, r.path, r.auxiliaryAuthorization)
			}
		}
	})
}

func TestAuthImportStateWithAuthAttributes(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}
	clearAuthEnv(t)

	factories, _ := newAuthTestProviderFactories((&authConfigRecorder{}).factory())
	subnetID := testSubnetID("snet1")
	config := fmt.Sprintf(`
provider "everpure" {
  subscription_id = %[1]q
  tenant_id       = "11111111-1111-1111-1111-111111111111"
  client_id       = "22222222-2222-2222-2222-222222222222"
  client_secret   = "shh-its-a-secret"
}
`, testSubscriptionID) + minimalPoolConfig(subnetID)

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{Config: config},
			{
				Config:            config,
				ResourceName:      "everpure_azure_storage_pool.test",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateId: fmt.Sprintf(
					"/subscriptions/%s/resourceGroups/%s/providers/PureStorage.Block/storagePools/pool1",
					testSubscriptionID, testResourceGroup,
				),
			},
		},
	})
}
