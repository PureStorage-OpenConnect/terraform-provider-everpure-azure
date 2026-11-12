// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/echoprovider"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

const testEchoAddr = "echo.test"

// withEchoProvider adds terraform-plugin-testing's echo provider to factories. An
// ephemeral value is never stored, so the only way a test can look at one is to feed
// it through the echo provider's configuration into an `echo` resource's state.
func withEchoProvider(factories map[string]func() (tfprotov6.ProviderServer, error)) map[string]func() (tfprotov6.ProviderServer, error) {
	factories["echo"] = echoprovider.NewProviderServer()
	return factories
}

// echoOf returns a configuration that echoes the whole ephemeral resource into state.
func echoOf(ephemeralAddr string) string {
	return fmt.Sprintf(`
provider "echo" {
  data = %[1]s
}

resource "echo" "test" {}
`, ephemeralAddr)
}

// checkNoPasswordInState fails when any resource other than the echo resource
// (which exists only to surface the ephemeral value) holds a password the fake has issued
// anywhere in its attributes, however deeply nested.
func checkNoPasswordInState(s *terraform.State) error {
	for _, m := range s.Modules {
		for addr, rs := range m.Resources {
			if rs.Type == "echo" {
				continue
			}
			raw, err := json.Marshal(rs.Primary.Attributes)
			if err != nil {
				return err
			}
			if strings.Contains(string(raw), "fake-activation-code-") {
				return fmt.Errorf("%s holds a password in its state: %s", addr, raw)
			}
		}
	}
	return nil
}

// expectNoPasswordInPlan fails when the plan, as `terraform show -json` renders it,
// carries a password the fake has issued anywhere: planned values, prior state, resource
// changes or configuration. Unlike checkNoPasswordInState it also covers the echo
// resource, whose planned value must stay unknown until apply.
type expectNoPasswordInPlan struct{}

func (expectNoPasswordInPlan) CheckPlan(_ context.Context, req plancheck.CheckPlanRequest, resp *plancheck.CheckPlanResponse) {
	raw, err := json.Marshal(req.Plan)
	if err != nil {
		resp.Error = err
		return
	}
	if strings.Contains(string(raw), "fake-activation-code-") {
		resp.Error = fmt.Errorf("the plan holds a password: %s", raw)
	}
}

func TestPlatformConsoleActivationCredentialsEphemeralResource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	const ephemeralAddr = "ephemeral.everpure_azure_platform_console_activation_credentials.test"

	activationConfig := func(poolID, extra string) string {
		return providerBlock() + fmt.Sprintf(`
ephemeral "everpure_azure_platform_console_activation_credentials" "test" {
  storage_pool_id = %[1]q
%[2]s}
`, poolID, extra) + echoOf(ephemeralAddr)
	}

	// Ephemeral resources need Terraform 1.10; the echo provider needs it for the same
	// reason.
	versionChecks := []tfversion.TerraformVersionCheck{tfversion.RequireAbove(tfversion.Version1_10_0)}

	t.Run("happy path: username, UTC expiry and a code come back, and no code reaches state", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		seedTestStoragePool(store)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: withEchoProvider(factories),
			TerraformVersionChecks:   versionChecks,
			Steps: []resource.TestStep{
				{
					// The pool is looked up through its data source, as the registry
					// examples do, so state holds a provider-written object besides the
					// echo resource for checkNoPasswordInState to scan.
					Config: fmt.Sprintf(`
data "everpure_azure_storage_pool" "test" {
  name                = %[1]q
  resource_group_name = %[2]q
}
`, testStoragePoolName, testResourceGroup) + activationConfig("${data.everpure_azure_storage_pool.test.id}", ""),
					ConfigStateChecks: []statecheck.StateCheck{
						statecheck.ExpectKnownValue(testEchoAddr, tfjsonpath.New("data").AtMapKey("storage_pool_id"), knownvalue.StringExact(testStoragePoolIDForVG())),
						statecheck.ExpectKnownValue(testEchoAddr, tfjsonpath.New("data").AtMapKey("username"), knownvalue.StringExact(fakeActivationCodeUsername)),
						statecheck.ExpectKnownValue(testEchoAddr, tfjsonpath.New("data").AtMapKey("expires_at"), knownvalue.StringExact("2026-10-02T14:30:00Z")),
						statecheck.ExpectKnownValue(testEchoAddr, tfjsonpath.New("data").AtMapKey("password"), knownvalue.StringRegexp(regexp.MustCompile(`^fake-activation-code-[0-9]+$`))),
					},
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{expectNoPasswordInPlan{}},
					},
					Check: resource.ComposeAggregateTestCheckFunc(
						checkNoPasswordInState,
						func(*terraform.State) error {
							if store.activationCodeCallCount() < 1 {
								return errors.New("the fake issued no activation code")
							}
							return nil
						},
					),
				},
			},
		})
	})

	t.Run("an explicit open timeout is accepted", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		seedTestStoragePool(store)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: withEchoProvider(factories),
			TerraformVersionChecks:   versionChecks,
			Steps: []resource.TestStep{
				{
					Config: activationConfig(testStoragePoolIDForVG(), "  timeouts {\n    open = \"2m\"\n  }\n"),
					ConfigStateChecks: []statecheck.StateCheck{
						statecheck.ExpectKnownValue(testEchoAddr, tfjsonpath.New("data").AtMapKey("username"), knownvalue.StringExact(fakeActivationCodeUsername)),
					},
				},
			},
		})
	})

	t.Run("a malformed open timeout is rejected", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		seedTestStoragePool(store)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: withEchoProvider(factories),
			TerraformVersionChecks:   versionChecks,
			Steps: []resource.TestStep{
				{
					Config:      activationConfig(testStoragePoolIDForVG(), "  timeouts {\n    open = \"soon\"\n  }\n"),
					ExpectError: regexp.MustCompile(`(?s)Invalid Attribute Value Time\s+Duration`),
				},
			},
		})

		if n := store.activationCodeCallCount(); n != 0 {
			t.Errorf("a malformed timeout must not issue a code, got %d", n)
		}
	})

	t.Run("chained from a storage pool resource in the same configuration (storage_pool_id unknown at plan)", func(t *testing.T) {
		factories, store := newTestProviderFactories()

		config := providerBlock() + fmt.Sprintf(`
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

ephemeral "everpure_azure_platform_console_activation_credentials" "test" {
  storage_pool_id = everpure_azure_storage_pool.test.id
}
`, testResourceGroup, testReservationID(), testSubnetID("snet1")) + echoOf(ephemeralAddr)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: withEchoProvider(factories),
			TerraformVersionChecks:   versionChecks,
			Steps: []resource.TestStep{
				{
					Config: config,
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{expectNoPasswordInPlan{}},
					},
					ConfigStateChecks: []statecheck.StateCheck{
						statecheck.ExpectKnownValue(testEchoAddr, tfjsonpath.New("data").AtMapKey("username"), knownvalue.StringExact(fakeActivationCodeUsername)),
						statecheck.ExpectKnownValue(testEchoAddr, tfjsonpath.New("data").AtMapKey("password"), knownvalue.StringRegexp(regexp.MustCompile(`^fake-activation-code-[0-9]+$`))),
					},
					// The pool is in state, so this also proves the code did not leak into
					// the one resource that sits next to the ephemeral one.
					Check: resource.ComposeAggregateTestCheckFunc(
						checkNoPasswordInState,
						func(*terraform.State) error {
							// The pool does not exist until apply, so the plan cannot open the
							// ephemeral resource; apply opens it exactly once.
							if n := store.activationCodeCallCount(); n != 1 {
								return fmt.Errorf("the fake issued %d activation codes during apply, want 1", n)
							}
							return nil
						},
					),
				},
			},
		})
	})

	t.Run("errors with the ID-based wording when the pool doesn't exist", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		seedTestStoragePool(store)

		missingID := armids.StoragePoolID{
			ResourceGroupID: armids.ResourceGroupID{SubscriptionID: testSubscriptionID, ResourceGroupName: testResourceGroup},
			StoragePoolName: "does-not-exist",
		}.FullID()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: withEchoProvider(factories),
			TerraformVersionChecks:   versionChecks,
			Steps: []resource.TestStep{
				{
					Config:      activationConfig(missingID, ""),
					ExpectError: diagRegexp(operation{action: actionRetrieve, kind: kindPlatformConsoleActivationCode, unnamed: true}.fail(newNotFoundError(kindStoragePool, missingID, nil, apiFailure{}))),
				},
			},
		})

		if n := store.activationCodeCallCount(); n != 0 {
			t.Errorf("a missing pool must not issue a code, got %d", n)
		}
	})

	t.Run("errors with the enable-the-console hint when the pool's platform console is off", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		seedTestStoragePool(store)
		store.disablePlatformConsole(testResourceGroup, testStoragePoolName)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: withEchoProvider(factories),
			TerraformVersionChecks:   versionChecks,
			Steps: []resource.TestStep{
				{
					Config: activationConfig(testStoragePoolIDForVG(), ""),
					ExpectError: regexp.MustCompile(diagRegexp(diag.WithPath(path.Root("storage_pool_id"), operation{action: actionRetrieve, kind: kindPlatformConsoleActivationCode, unnamed: true}.fail(newPlatformConsoleDisabledError(testStoragePoolIDForVG(), apiFailure{})))).String() +
						`.*Platform Console is not\s+enabled.*x-ms-correlation-request-id`),
				},
			},
		})

		if n := store.activationCodeCallCount(); n != 0 {
			t.Errorf("a pool without the console must not issue a code, got %d", n)
		}
	})

	t.Run("errors when storage_pool_id names another subscription", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		seedTestStoragePool(store)

		otherSubscriptionID := armids.StoragePoolID{
			ResourceGroupID: armids.ResourceGroupID{
				SubscriptionID:    "22222222-2222-2222-2222-222222222222",
				ResourceGroupName: testResourceGroup,
			},
			StoragePoolName: testStoragePoolName,
		}.FullID()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: withEchoProvider(factories),
			TerraformVersionChecks:   versionChecks,
			Steps: []resource.TestStep{
				{
					Config:      activationConfig(otherSubscriptionID, ""),
					ExpectError: subscriptionMismatchRegexp(kindStoragePool, otherSubscriptionID),
				},
			},
		})

		if n := store.activationCodeCallCount(); n != 0 {
			t.Errorf("a pool in another subscription must not issue a code, got %d", n)
		}
	})

	t.Run("errors at validation when storage_pool_id is not a storage pool ID", func(t *testing.T) {
		factories, _ := newTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: withEchoProvider(factories),
			TerraformVersionChecks:   versionChecks,
			Steps: []resource.TestStep{
				{
					Config:      activationConfig("not-an-id", ""),
					ExpectError: regexp.MustCompile(`(?s)must be the full Azure Resource Manager ID of a\s+storage\s+pool`),
				},
			},
		})
	})
}
