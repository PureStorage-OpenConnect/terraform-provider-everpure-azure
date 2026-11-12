// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/config"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
)

// The round-trip tests are the guarantee behind bulk import: objects created through
// one Terraform working directory are listed from another, `terraform query
// -generate-config-out` writes config for them, and the first plan on that config must
// import every object with nothing to add, change or destroy. They run the real
// terraform binary by hand (see terraformCLI) because terraform-plugin-testing's Query
// steps cannot pass -generate-config-out.

// includeResource is the list block line every round trip uses, except where a test
// says otherwise.
const includeResource = "  include_resource = true"

const requiredProvidersBlock = `
terraform {
  required_providers {
    everpure = {
      source = "hashicorp/everpure"
    }
  }
}
`

// roundTrip creates objects by applying createConfig in one directory (with env as
// TF_VAR_* inputs), then, from a second directory with no state, queries listBlock with
// -generate-config-out, plans the generated config and checks that it imports wantCount
// objects and changes nothing; applying it must then leave a clean plan.
func roundTrip(
	t *testing.T, factories map[string]func() (tfprotov6.ProviderServer, error),
	createConfig string, env map[string]string, listType, includeResource, listBody string, wantCount int,
) {
	t.Helper()

	creator := newTerraformCLI(t, factories)
	for name, value := range env {
		creator.setEnv("TF_VAR_"+name, value)
	}
	creator.write("main.tf", requiredProvidersBlock+providerBlock()+createConfig)
	creator.mustRun("init", "-no-color")
	creator.mustRun("apply", "-auto-approve", "-no-color")

	importer := creator.fork()
	importer.write("main.tf", requiredProvidersBlock+providerBlock())
	importer.write("discover.tfquery.hcl", fmt.Sprintf(`
list %q "all" {
  provider = everpure
%s
%s
}
`, listType, includeResource, listBody))
	importer.mustRun("init", "-no-color")
	importer.mustRun("query", "-no-color", "-generate-config-out=generated.tf")

	generated := importer.read("generated.tf")
	if got := strings.Count(generated, "\nimport {"); got != wantCount {
		t.Fatalf("generated config has %d import blocks, want %d:\n%s", got, wantCount, generated)
	}

	plan := importer.mustRun("plan", "-no-color")
	want := fmt.Sprintf("Plan: %d to import, 0 to add, 0 to change, 0 to destroy.", wantCount)
	if !strings.Contains(plan, want) {
		t.Fatalf("plan does not say %q:\n%s\n\ngenerated config:\n%s", want, plan, generated)
	}

	importer.mustRun("apply", "-auto-approve", "-no-color")
	if out := importer.mustRun("plan", "-no-color"); !strings.Contains(out, "No changes.") {
		t.Fatalf("plan after importing is not empty:\n%s\n\ngenerated config:\n%s", out, generated)
	}
}

func TestStoragePoolListResourceRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	factories, _ := newTestProviderFactories()
	createConfig := fmt.Sprintf(`
variable "console" {
  type = list(object({
    gui = bool
    api = bool
    cli = bool
  }))
  default = []
}

resource "everpure_azure_storage_pool" "full" {
  name                          = "pool-full"
  resource_group_name           = %[1]q
  location                      = "eastus2"
  zone                          = "1"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[3]q
  }

  dynamic "platform_console" {
    for_each = var.console
    content {
      gui_enabled = platform_console.value.gui
      api_enabled = platform_console.value.api
      cli_enabled = platform_console.value.cli
    }
  }

  tags = {
    environment = "test"
    team        = "storage"
  }
}

resource "everpure_azure_storage_pool" "plain" {
  name                          = "pool-plain"
  resource_group_name           = "rg-other"
  location                      = "eastus2"
  zone                          = "2"
  reservation_id                = %[2]q
  provisioned_bandwidth_in_mbps = 2048

  vnet_injection {
    subnet_id = %[3]q
  }
}
`, testResourceGroup, testReservationID(), testSubnetID("snet1"))

	env := map[string]string{"console": `[{ gui = true, api = true, cli = false }]`}

	t.Run("the whole subscription", func(t *testing.T) {
		roundTrip(t, factories, createConfig, env, "everpure_azure_storage_pool", includeResource, "", 2)
	})

	t.Run("one resource group", func(t *testing.T) {
		factories, _ := newTestProviderFactories()
		roundTrip(t, factories, createConfig, env, "everpure_azure_storage_pool", includeResource,
			fmt.Sprintf("  config {\n    resource_group_name = %q\n  }", testResourceGroup), 1)
	})
}

func TestReservationListResourceRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	// offer comes from a variable that is unknown at validate time, the nested-attribute
	// counterpart of the dynamic-block case: it must not break creating the objects the
	// list then finds.
	createConfig := fmt.Sprintf(`
variable "offer" {
  type = object({
    offer_id  = string
    plan_id   = string
    term_unit = string
  })
}

resource "everpure_azure_reservation" "full" {
  name                = "res-full"
  resource_group_name = %[1]q
  location            = "eastus2"
  offer               = var.offer

  user = {
    first_name    = "Ada"
    last_name     = "Lovelace"
    email_address = "ada@example.com"
  }

  tags = {
    environment = "test"
    team        = "storage"
  }
}

resource "everpure_azure_reservation" "plain" {
  name                = "res-plain"
  resource_group_name = "rg-other"
  location            = "westus2"
  offer               = var.offer
}
`, testResourceGroup)

	env := map[string]string{"offer": `{ offer_id = "offer_a", plan_id = "plan_a", term_unit = "P1M" }`}

	t.Run("the whole subscription", func(t *testing.T) {
		factories, _ := newReservationTestProviderFactories()
		roundTrip(t, factories, createConfig, env, "everpure_azure_reservation", includeResource, "", 2)
	})

	t.Run("one resource group", func(t *testing.T) {
		factories, _ := newReservationTestProviderFactories()
		roundTrip(t, factories, createConfig, env, "everpure_azure_reservation", includeResource,
			fmt.Sprintf("  config {\n    resource_group_name = %q\n  }", testResourceGroup), 1)
	})

	t.Run("without include_resource", func(t *testing.T) {
		factories, _ := newReservationTestProviderFactories()
		roundTrip(t, factories, createConfig, env, "everpure_azure_reservation", "", "", 2)
	})
}

// Terraform asks the provider for the full resource object when it is generating
// config, whatever the list block says, so omitting include_resource must not change
// what -generate-config-out writes.
func TestVolumeListResourceRoundTripWithoutIncludeResource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	factories, _ := newTestVolumeProviderFactories()
	createConfig := fmt.Sprintf(`
resource "everpure_azure_volume" "plain" {
  name                    = "vol-plain"
  volume_group_id         = %q
  provisioned_size_in_gib = 5
  deletion_protection     = false
}
`, testVolumeGroupID("vg1"))

	roundTrip(t, factories, createConfig, nil, "everpure_azure_volume", "",
		fmt.Sprintf("  config {\n    volume_group_id = %q\n  }", testVolumeGroupID("vg1")), 1)
}

func TestVolumeGroupListResourceRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	factories, _, _ := newTestVolumeGroupProviderFactories()
	createConfig := fmt.Sprintf(`
variable "policies" {
  type = list(object({
    frequency = string
    retention = string
  }))
  default = []
}

resource "everpure_azure_volume_group" "protected" {
  name                    = "vg-protected"
  storage_pool_id         = %[1]q
  deletion_protection     = false
  bandwidth_limit_in_mbps = 500
  iops_limit              = 10000

  dynamic "snapshot_protection_policy" {
    for_each = var.policies
    content {
      frequency = snapshot_protection_policy.value.frequency
      retention = snapshot_protection_policy.value.retention
    }
  }

  tags = {
    environment = "test"
  }
}

resource "everpure_azure_volume_group" "plain" {
  name                = "vg-plain"
  storage_pool_id     = %[1]q
  deletion_protection = false
}
`, testStoragePoolIDForVG())

	roundTrip(t, factories, createConfig,
		map[string]string{"policies": `[{ frequency = "PT1H", retention = "P7D" }]`},
		"everpure_azure_volume_group", includeResource, fmt.Sprintf("  config {\n    storage_pool_id = %q\n  }", testStoragePoolIDForVG()), 2)
}

func TestVolumeListResourceRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	factories, _ := newTestVolumeProviderFactories()
	createConfig := fmt.Sprintf(`
variable "snapshots" {
  type = list(object({
    id          = string
    volume_name = string
  }))
  default = []
}

resource "everpure_azure_volume" "plain" {
  name                    = "vol-plain"
  volume_group_id         = %[1]q
  provisioned_size_in_gib = 5
  deletion_protection     = false
}

resource "everpure_azure_volume" "restored" {
  name                    = "vol-restored"
  volume_group_id         = %[1]q
  deletion_protection     = false
  source_type             = "snapshot"

  dynamic "source_snapshot" {
    for_each = var.snapshots
    content {
      id          = source_snapshot.value.id
      volume_name = source_snapshot.value.volume_name
    }
  }
}
`, testVolumeGroupID("vg1"))

	roundTrip(t, factories, createConfig,
		map[string]string{"snapshots": fmt.Sprintf(`[{ id = %q, volume_name = "member-vol" }]`, testVolumeGroupSnapshotID("vg-source", "snap1"))},
		"everpure_azure_volume", includeResource, fmt.Sprintf("  config {\n    volume_group_id = %q\n  }", testVolumeGroupID("vg1")), 2)
}

// The generated config is what most practitioners will import from, but it is also
// common to move a generated resource into a module that builds its nested blocks with
// dynamic. These tests import listed objects into such configs, with the block contents
// supplied through ConfigVariables, and require the import to plan empty.

func TestVolumeGroupListResultImportsIntoDynamicBlockConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	factories, _, vgStore := newTestVolumeGroupProviderFactories()
	vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg-a", "PT1H", "P7D")

	cfg := providerBlock() + fmt.Sprintf(`
variable "deletion_protection" {
  type    = bool
  default = true
}

variable "policies" {
  type = list(object({
    frequency = string
    retention = string
  }))
  default = []
}

import {
  to = everpure_azure_volume_group.test
  identity = {
    subscription_id     = %[2]q
    resource_group_name = %[3]q
    storage_pool_name   = %[4]q
    name                = "vg-a"
  }
}

resource "everpure_azure_volume_group" "test" {
  name                = "vg-a"
  storage_pool_id     = %[1]q
  deletion_protection = var.deletion_protection

  dynamic "snapshot_protection_policy" {
    for_each = var.policies
    content {
      frequency = snapshot_protection_policy.value.frequency
      retention = snapshot_protection_policy.value.retention
    }
  }
}
`, testStoragePoolIDForVG(), testSubscriptionID, testResourceGroup, testStoragePoolName)

	variables := config.Variables{
		"policies": config.ListVariable(config.ObjectVariable(map[string]config.Variable{
			"frequency": config.StringVariable("PT1H"),
			"retention": config.StringVariable("P7D"),
		})),
	}

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		TerraformVersionChecks:   []tfversion.TerraformVersionCheck{tfversion.SkipBelow(tfversion.Version1_14_0)},
		Steps: []resource.TestStep{
			{
				// The import itself: the listed object, written out with the default
				// deletion protection an import assumes, must plan as a pure import.
				Config:          cfg,
				ConfigVariables: variables,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("everpure_azure_volume_group.test", plancheck.ResourceActionNoop),
				}},
				Check: resource.TestCheckResourceAttr("everpure_azure_volume_group.test", "snapshot_protection_policy.#", "1"),
			},
			{
				// Lifts the protection so the harness can destroy what it imported.
				Config: cfg,
				ConfigVariables: config.Variables{
					"deletion_protection": config.BoolVariable(false),
					"policies":            variables["policies"],
				},
			},
		},
	})
}

func TestVolumeListResultImportsIntoDynamicBlockConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	factories, store := newTestVolumeProviderFactories()
	store.seedVolume(testResourceGroup, testStoragePoolName, "vg1", "vol-a", bytesPerGiB)

	cfg := providerBlock() + fmt.Sprintf(`
variable "deletion_protection" {
  type    = bool
  default = true
}

variable "snapshots" {
  type = list(object({
    id          = string
    volume_name = string
  }))
  default = []
}

import {
  to = everpure_azure_volume.test
  identity = {
    subscription_id     = %[2]q
    resource_group_name = %[3]q
    storage_pool_name   = %[4]q
    volume_group_name   = "vg1"
    name                = "vol-a"
  }
}

resource "everpure_azure_volume" "test" {
  name                      = "vol-a"
  volume_group_id           = %[1]q
  provisioned_size_in_bytes = %[5]d
  deletion_protection       = var.deletion_protection

  dynamic "source_snapshot" {
    for_each = var.snapshots
    content {
      id          = source_snapshot.value.id
      volume_name = source_snapshot.value.volume_name
    }
  }
}
`, testVolumeGroupID("vg1"), testSubscriptionID, testResourceGroup, testStoragePoolName, int64(bytesPerGiB))

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		TerraformVersionChecks:   []tfversion.TerraformVersionCheck{tfversion.SkipBelow(tfversion.Version1_14_0)},
		Steps: []resource.TestStep{
			{
				// The import itself: the listed object, written out with the default
				// deletion protection an import assumes, must plan as a pure import.
				Config: cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("everpure_azure_volume.test", plancheck.ResourceActionNoop),
				}},
				Check: resource.TestCheckResourceAttr("everpure_azure_volume.test", "source_snapshot.#", "0"),
			},
			{
				// Lifts the protection so the harness can destroy what it imported.
				Config: cfg,
				ConfigVariables: config.Variables{
					"deletion_protection": config.BoolVariable(false),
				},
			},
		},
	})
}

// The volume group holds a schedule-created snapshot and a destroyed one besides the two
// created here, so the round trip also proves the exclusions: importing either would
// fail, and the plan would not be "2 to import".
func seedSnapshotListDecoys(store *fakeSnapshotStore) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "20260901000000", at, true)
	store.seedDestroyedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap-old-20260901000000", "snap-old", at, false)
}

func TestSnapshotListResourceRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	factories, store := newTestSnapshotProviderFactories()
	seedSnapshotListDecoys(store)
	createConfig := fmt.Sprintf(`
resource "everpure_azure_snapshot" "first" {
  name                = "snap-first"
  volume_group_id     = %[1]q
  deletion_protection = false
}

resource "everpure_azure_snapshot" "second" {
  name                = "snap-second"
  volume_group_id     = %[1]q
  deletion_protection = false
}
`, testVolumeGroupID("vg1"))

	roundTrip(t, factories, createConfig, nil, "everpure_azure_snapshot", includeResource,
		fmt.Sprintf("  config {\n    volume_group_id = %q\n  }", testVolumeGroupID("vg1")), 2)
}

// Generating config makes Terraform request the resource object whatever the list block
// says. The list response carries no member volumes, and the provider must not issue a
// GET per result to fetch them: volume_snapshots is read-only, so the generated config
// never contains it and the import's own Read fills it.
func TestSnapshotListResourceGenerateConfigMakesNoGets(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	factories, store := newTestSnapshotProviderFactories()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap-a", at, false)
	store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap-b", at, false)
	seedSnapshotListDecoys(store)

	cli := newTerraformCLI(t, factories)
	cli.write("main.tf", requiredProvidersBlock+providerBlock())
	cli.write("discover.tfquery.hcl", fmt.Sprintf(`
list "everpure_azure_snapshot" "all" {
  provider = everpure

  config {
    volume_group_id = %q
  }
}
`, testVolumeGroupID("vg1")))
	cli.mustRun("init", "-no-color")

	store.mu.Lock()
	store.getCallCount = 0
	store.mu.Unlock()
	cli.mustRun("query", "-no-color")
	store.mu.Lock()
	withoutGenerate := store.getCallCount
	store.getCallCount = 0
	store.mu.Unlock()
	if withoutGenerate != 0 {
		t.Errorf("plain query made %d GETs, want none", withoutGenerate)
	}

	cli.mustRun("query", "-no-color", "-generate-config-out=generated.tf")
	store.mu.Lock()
	withGenerate := store.getCallCount
	store.mu.Unlock()
	if withGenerate != 0 {
		t.Errorf("-generate-config-out made %d GETs, want none", withGenerate)
	}
}
