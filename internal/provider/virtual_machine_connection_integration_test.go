// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azcorefake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	psbfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock/fake"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/config"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

const testVMConnResourceAddr = "everpure_azure_virtual_machine_connection.test"

// newTestVMConnectionTransport builds fresh fake stores for storage pools, volume
// groups and compute behind one combined transport, with a storage pool pre-seeded
// so a volume group can be created against it.
func newTestVMConnectionTransport() (*fakeStoragePoolStore, *fakeVolumeGroupStore, *fakeComputeStore, *arm.ClientOptions) {
	poolStore := newFakeStoragePoolStore(testSubscriptionID)
	vgStore := newFakeVolumeGroupStore(testSubscriptionID)
	computeStore := newFakeComputeStore(testSubscriptionID)
	seedTestStoragePool(poolStore)

	poolsServer := poolStore.newStoragePoolsServer()
	vgServer := vgStore.newVolumeGroupsServer()
	recoverableServer := vgStore.newRecoverableVolumeGroupsServer()

	psbTransport := psbfake.NewServerFactoryTransport(&psbfake.ServerFactory{
		StoragePoolsServer:            poolsServer,
		VolumeGroupsServer:            vgServer,
		RecoverableVolumeGroupsServer: recoverableServer,
	})

	transport := &multiModuleTransport{
		purestorageblock: psbTransport,
		compute:          computeStore.newSelfHealingComputeTransport(),
	}

	return poolStore, vgStore, computeStore, &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: transport}}
}

// newTestVMConnectionProviderFactories wraps newTestVMConnectionTransport's fakes in
// an isolated provider instance, for tests driving the resource through terraform.
func newTestVMConnectionProviderFactories() (
	map[string]func() (tfprotov6.ProviderServer, error), *fakeStoragePoolStore, *fakeVolumeGroupStore, *fakeComputeStore,
) {
	poolStore, vgStore, computeStore, opts := newTestVMConnectionTransport()
	p := newProvider("test", fixedCredentialFactory(&azcorefake.TokenCredential{}), opts)()

	factories := map[string]func() (tfprotov6.ProviderServer, error){
		"everpure": providerserver.NewProtocol6WithError(p),
	}

	return factories, poolStore, vgStore, computeStore
}

// testExtensionID composes the full ARM ID of an extension named extensionName on the
// virtual machine named vmName under testSubscriptionID/testResourceGroup, for import
// and identity assertions.
func testExtensionID(vmName, extensionName string) string {
	return armids.VirtualMachineExtensionID{
		ResourceGroupID: armids.ResourceGroupID{
			SubscriptionID:    testSubscriptionID,
			ResourceGroupName: testResourceGroup,
		},
		VirtualMachineName: vmName,
		ExtensionName:      extensionName,
	}.FullID()
}

// pinnedSettings decodes a recorded PUT's settings body into the typed shape,
// failing the test immediately on a decode error.
func pinnedSettings(t *testing.T, entry computeLogEntry) iscsiExtensionSettings {
	t.Helper()
	if entry.Body == nil || entry.Body.Properties == nil {
		t.Fatalf("recorded PUT entry has no body/properties: %+v", entry)
	}
	settings, err := iscsiSettingsFromProperties(entry.Body.Properties)
	if err != nil {
		t.Fatalf("iscsiSettingsFromProperties: %v", err)
	}
	return settings
}

// oneVGConfig builds a minimal config with one virtual machine and one volume group,
// both referenced by literal ARM ID rather than created in the config.
func oneVGConfig(vmID, vgID, extraAttrs string) string {
	return providerBlock() + fmt.Sprintf(`
resource "everpure_azure_virtual_machine_connection" "test" {
  virtual_machine_id = %[1]q
  volume_group_ids    = [%[2]q]
%[3]s
}
`, vmID, vgID, extraAttrs)
}

func twoVGConfig(vmID, vg1, vg2, extraAttrs string) string {
	return providerBlock() + fmt.Sprintf(`
resource "everpure_azure_virtual_machine_connection" "test" {
  virtual_machine_id = %[1]q
  volume_group_ids    = [%[2]q, %[3]q]
%[4]s
}
`, vmID, vg1, vg2, extraAttrs)
}

// TestVirtualMachineConnectionResource covers create, update, replace, import, drift
// and destroy for the resource. The dynamic-block/unknown-ID regression is a separate
// test function below.
func TestVirtualMachineConnectionResource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	t.Run("scenario 1: Linux create minimal pins the PUT body and populates resolved_targets", func(t *testing.T) {
		factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
		computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
		vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			endpoint("10.0.1.5", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
		})

		vmID := testVMID(testSubscriptionID, "vm1")
		vgID := testVGID("pool1", "vg1")
		cfg := oneVGConfig(vmID, vgID, "")

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: cfg,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(testVMConnResourceAddr, "extension_type", "MountIscsiLinux"),
						resource.TestCheckResourceAttr(testVMConnResourceAddr, "extension_name", "PureMountIscsi"),
						resource.TestCheckResourceAttr(testVMConnResourceAddr, "iscsi_session_count", "2"),
						resource.TestCheckResourceAttr(testVMConnResourceAddr, "resolved_targets.#", "2"),
						resource.TestCheckResourceAttr(testVMConnResourceAddr, "reboot_allowed", "false"),
						resource.TestCheckNoResourceAttr(testVMConnResourceAddr, "initiator_name"),
					),
				},
			},
		})

		log := computeStore.operationLog()
		if len(log) == 0 || log[0].Op != "PUT" {
			t.Fatalf("expected the create's PUT to be the first recorded op, got %+v", log)
		}
		body := log[0].Body
		if body.Location == nil || *body.Location != "eastus2" {
			t.Fatalf("expected the PUT body's Location to carry the parent virtual machine's own location, got %+v", body.Location)
		}
		if body.Properties == nil || body.Properties.Publisher == nil || *body.Properties.Publisher != extensionPublisher {
			t.Fatalf("unexpected publisher: %+v", body.Properties)
		}
		if body.Properties.Type == nil || *body.Properties.Type != extensionTypeLinux {
			t.Fatalf("unexpected type: %+v", body.Properties.Type)
		}
		if body.Properties.TypeHandlerVersion == nil || *body.Properties.TypeHandlerVersion != extensionTypeHandlerVersion {
			t.Fatalf("unexpected typeHandlerVersion: %+v", body.Properties.TypeHandlerVersion)
		}
		if body.Properties.AutoUpgradeMinorVersion == nil || !*body.Properties.AutoUpgradeMinorVersion {
			t.Fatalf("expected autoUpgradeMinorVersion true, got %+v", body.Properties.AutoUpgradeMinorVersion)
		}
		settings := pinnedSettings(t, log[0])
		if !settings.ReplaceTargets || settings.SessionCount != 2 || len(settings.Targets) != 2 {
			t.Fatalf("unexpected pinned settings: %+v", settings)
		}
		if settings.InitiatorName != "" {
			t.Fatalf("expected no initiator name in the PUT body, got %q", settings.InitiatorName)
		}
	})

	t.Run("scenario 2: Windows create full waits through rebooting to success", func(t *testing.T) {
		origInterval := instanceViewPollInterval
		instanceViewPollInterval = 10 * time.Millisecond
		defer func() { instanceViewPollInterval = origInterval }()

		factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
		computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesWindows, "eastus2")
		vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
		})

		vmID := testVMID(testSubscriptionID, "vm1")
		vgID := testVGID("pool1", "vg1")
		cfg := oneVGConfig(vmID, vgID, `
  reboot_allowed      = true
  initiator_name      = "iqn.2026-09.com.contoso:app01"
  iscsi_session_count = 4

  reapply_on_boot {
    enabled                   = true
    reboot_on_reapply_allowed = true
  }

  windows_settings {
    disk_timeout_in_seconds   = 240
    link_down_time_in_seconds = 180
  }
`)

		// The PUT's own LRO succeeds before the extension finishes configuring, so
		// the instance view is polled afterwards: it reports "rebooting" once before
		// settling on "completed successfully".
		computeStore.setInstanceViewSequence(testResourceGroup, "vm1", defaultExtensionName(),
			&armcompute.InstanceViewStatus{
				Code: to.Ptr("ProvisioningState/succeeded"), Level: to.Ptr(armcompute.StatusLevelTypesInfo),
				Message: to.Ptr("Enable succeeded: Rebooting as requested..."),
			},
			&armcompute.InstanceViewStatus{
				Code: to.Ptr("ProvisioningState/succeeded"), Level: to.Ptr(armcompute.StatusLevelTypesInfo),
				Message: to.Ptr("Enable succeeded: iSCSI configuration completed successfully"),
			},
		)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: cfg,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(testVMConnResourceAddr, "extension_type", "MountIscsiWindows"),
						resource.TestCheckResourceAttr(testVMConnResourceAddr, "reboot_allowed", "true"),
						resource.TestCheckResourceAttr(testVMConnResourceAddr, "iscsi_session_count", "4"),
						resource.TestCheckResourceAttr(testVMConnResourceAddr, "initiator_name", "iqn.2026-09.com.contoso:app01"),
						resource.TestCheckResourceAttr(testVMConnResourceAddr, "reapply_on_boot.0.enabled", "true"),
						resource.TestCheckResourceAttr(testVMConnResourceAddr, "windows_settings.0.disk_timeout_in_seconds", "240"),
						resource.TestCheckResourceAttr(testVMConnResourceAddr, "windows_settings.0.link_down_time_in_seconds", "180"),
						// Confirms apply itself (not the post-apply refresh that runs after
						// this Check) consumed both scripted statuses: exactly one
						// $expand=instanceView GET per status.
						func(*terraform.State) error {
							extName := defaultExtensionName()
							if got := computeStore.expandedInstanceViewGetCount(testResourceGroup, "vm1", extName); got != 2 {
								return fmt.Errorf(
									"expected exactly 2 $expand=instanceView GETs consumed by apply (one per scripted status), got %d",
									got,
								)
							}
							return nil
						},
					),
				},
			},
		})
	})

	// The warning text itself can't be asserted through terraform-plugin-testing;
	// only its effect is checked: apply succeeds without tainting the resource, and
	// a subsequent plan stays empty.
	t.Run("scenario 3: Windows default reboot_allowed reports reboot-required as a warning, not an error", func(t *testing.T) {
		factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
		computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesWindows, "eastus2")
		vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
		})

		vmID := testVMID(testSubscriptionID, "vm1")
		vgID := testVGID("pool1", "vg1")
		cfg := oneVGConfig(vmID, vgID, "")

		computeStore.queueLROFailure(testResourceGroup, "vm1", defaultExtensionName(), &armcompute.InstanceViewStatus{
			Code: to.Ptr("ProvisioningState/failed/0"), Level: to.Ptr(armcompute.StatusLevelTypesError),
			Message: to.Ptr(handlerMessageRebootRequired),
		})

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: cfg,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttrSet(testVMConnResourceAddr, "id"),
						resource.TestCheckResourceAttr(testVMConnResourceAddr, "reboot_allowed", "false"),
					),
				},
				{
					// The fake keeps repeating reboot-required, so refresh raises the
					// warning again, but the resource isn't tainted and this plans no
					// changes.
					Config:   cfg,
					PlanOnly: true,
				},
			},
		})
	})

	// initiator_name is absent from every PUT below since it's never set in these
	// steps.
	t.Run("scenario 4: in-place updates for volume_group_ids, iscsi_session_count and blocks", func(t *testing.T) {
		factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
		computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
		vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
		})
		vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg2", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg2", 3260),
		})

		vmID := testVMID(testSubscriptionID, "vm1")
		vg1 := testVGID("pool1", "vg1")
		vg2 := testVGID("pool1", "vg2")

		cfgOneVG := oneVGConfig(vmID, vg1, "")
		cfgTwoVGs := twoVGConfig(vmID, vg1, vg2, "")
		cfgTwoVGsSessionCount := twoVGConfig(vmID, vg1, vg2, "  iscsi_session_count = 4\n")
		cfgWithReapply := twoVGConfig(vmID, vg1, vg2, `  iscsi_session_count = 4

  reapply_on_boot {
    enabled                   = true
    reboot_on_reapply_allowed = true
  }
`)
		cfgWithWindowsSettings := twoVGConfig(vmID, vg1, vg2, `  iscsi_session_count = 4

  windows_settings {
    disk_timeout_in_seconds   = 120
    link_down_time_in_seconds = 60
  }
`)
		cfgWithWindowsSettingsChanged := twoVGConfig(vmID, vg1, vg2, `  iscsi_session_count = 4

  windows_settings {
    disk_timeout_in_seconds   = 300
    link_down_time_in_seconds = 90
  }
`)

		updateCheck := resource.ConfigPlanChecks{
			PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(testVMConnResourceAddr, plancheck.ResourceActionUpdate),
			},
		}

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: cfgOneVG}, // create: PUT #0
				{
					Config:           cfgTwoVGs, // add vg2: PUT #1
					ConfigPlanChecks: updateCheck,
					Check:            resource.TestCheckResourceAttr(testVMConnResourceAddr, "resolved_targets.#", "2"),
				},
				{
					Config:           cfgTwoVGsSessionCount, // session count 2 -> 4: PUT #2
					ConfigPlanChecks: updateCheck,
					Check:            resource.TestCheckResourceAttr(testVMConnResourceAddr, "iscsi_session_count", "4"),
				},
				{
					Config:           cfgWithReapply, // add reapply_on_boot: PUT #3
					ConfigPlanChecks: updateCheck,
					Check:            resource.TestCheckResourceAttr(testVMConnResourceAddr, "reapply_on_boot.0.enabled", "true"),
				},
				{
					Config:           cfgTwoVGsSessionCount, // remove reapply_on_boot: PUT #4
					ConfigPlanChecks: updateCheck,
					Check:            resource.TestCheckResourceAttr(testVMConnResourceAddr, "reapply_on_boot.#", "0"),
				},
				{
					Config:           cfgWithWindowsSettings, // add windows_settings: PUT #5
					ConfigPlanChecks: updateCheck,
					Check:            resource.TestCheckResourceAttr(testVMConnResourceAddr, "windows_settings.0.disk_timeout_in_seconds", "120"),
				},
				{
					Config:           cfgWithWindowsSettingsChanged, // change windows_settings' values: PUT #6
					ConfigPlanChecks: updateCheck,
					Check:            resource.TestCheckResourceAttr(testVMConnResourceAddr, "windows_settings.0.disk_timeout_in_seconds", "300"),
				},
				{
					Config:           cfgTwoVGsSessionCount, // remove windows_settings: PUT #7
					ConfigPlanChecks: updateCheck,
					Check:            resource.TestCheckResourceAttr(testVMConnResourceAddr, "windows_settings.#", "0"),
				},
				{
					Config:           cfgOneVG, // remove vg2, session count back to 2: PUT #8
					ConfigPlanChecks: updateCheck,
					Check:            resource.TestCheckResourceAttr(testVMConnResourceAddr, "resolved_targets.#", "1"),
				},
			},
		})

		log := computeStore.operationLog()
		if len(log) < 9 {
			t.Fatalf("expected at least 9 recorded PUTs, got %+v", log)
		}
		for i, want := range []struct {
			replaceTargets bool
			targets        int
			sessionCount   int64
			reapplyEnabled bool
			diskTimeout    int64
			linkDownTime   int64
		}{
			{true, 1, 2, false, 0, 0},    // create
			{true, 2, 2, false, 0, 0},    // add vg2
			{true, 2, 4, false, 0, 0},    // session count 4
			{true, 2, 4, true, 0, 0},     // add reapply_on_boot
			{true, 2, 4, false, 0, 0},    // remove reapply_on_boot
			{true, 2, 4, false, 120, 60}, // add windows_settings
			{true, 2, 4, false, 300, 90}, // change windows_settings' values
			{true, 2, 4, false, 0, 0},    // remove windows_settings
			{true, 1, 2, false, 0, 0},    // remove vg2, session count back to 2
		} {
			if log[i].Op != "PUT" {
				t.Fatalf("op #%d: expected PUT, got %s", i, log[i].Op)
			}
			settings := pinnedSettings(t, log[i])
			if settings.ReplaceTargets != want.replaceTargets {
				t.Fatalf("op #%d: replaceTargets = %v, want %v", i, settings.ReplaceTargets, want.replaceTargets)
			}
			if len(settings.Targets) != want.targets {
				t.Fatalf("op #%d: len(targets) = %d, want %d", i, len(settings.Targets), want.targets)
			}
			if settings.SessionCount != want.sessionCount {
				t.Fatalf("op #%d: sessionCount = %d, want %d", i, settings.SessionCount, want.sessionCount)
			}
			if settings.AutoUpdateConfiguration.Enabled != want.reapplyEnabled {
				t.Fatalf("op #%d: autoUpdateConfiguration.enabled = %v, want %v", i, settings.AutoUpdateConfiguration.Enabled, want.reapplyEnabled)
			}
			if settings.AdvancedWindowsSettings.DiskTimeout != want.diskTimeout || settings.AdvancedWindowsSettings.LinkDownTime != want.linkDownTime {
				t.Fatalf("op #%d: advancedWindowsSettings = %+v, want {%d, %d}", i, settings.AdvancedWindowsSettings, want.diskTimeout, want.linkDownTime)
			}
			if settings.InitiatorName != "" {
				t.Fatalf("op #%d: expected no initiatorName key, got %q", i, settings.InitiatorName)
			}
		}
	})

	t.Run("scenario 5: virtual_machine_id change replaces the resource", func(t *testing.T) {
		factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
		computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
		computeStore.seedVirtualMachine(testResourceGroup, "vm2", armcompute.OperatingSystemTypesLinux, "eastus2")
		vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
		})

		vg1 := testVGID("pool1", "vg1")
		vm1ID := testVMID(testSubscriptionID, "vm1")
		vm2ID := testVMID(testSubscriptionID, "vm2")

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: oneVGConfig(vm1ID, vg1, "")},
				{
					Config: oneVGConfig(vm2ID, vg1, ""),
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction(testVMConnResourceAddr, plancheck.ResourceActionReplace),
						},
					},
					Check: resource.TestCheckResourceAttr(testVMConnResourceAddr, "virtual_machine_id", vm2ID),
				},
			},
		})

		log := computeStore.operationLog()
		var vm1Keys, vm2Keys int
		for _, e := range log {
			switch {
			case e.Key == computeExtensionKey(testResourceGroup, "vm1", defaultExtensionName()):
				vm1Keys++
			case e.Key == computeExtensionKey(testResourceGroup, "vm2", defaultExtensionName()):
				vm2Keys++
			}
		}
		if vm1Keys == 0 || vm2Keys == 0 {
			t.Fatalf("expected operations against both the old and the new virtual machine's extension, got %+v", log)
		}
	})

	t.Run("scenario 6: a volume group's endpoints changing in the fake shows an in-place update on the next plan", func(t *testing.T) {
		factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
		computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
		vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
		})

		vmID := testVMID(testSubscriptionID, "vm1")
		vgID := testVGID("pool1", "vg1")
		cfg := oneVGConfig(vmID, vgID, "")

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: cfg},
				{
					PreConfig: func() {
						// A second portal is added for vg1 out of band; resolveVMExtensionTargets
						// picks it up on the next plan or apply with no config change.
						vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
							endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
							endpoint("10.0.1.5", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
						})
					},
					Config: cfg,
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction(testVMConnResourceAddr, plancheck.ResourceActionUpdate),
						},
					},
					Check: resource.TestCheckResourceAttr(testVMConnResourceAddr, "resolved_targets.#", "2"),
				},
			},
		})
	})

	// windows_settings values are sent as-is even on Linux; stripping them would show
	// a diff on every plan for a config shared across a mixed Linux/Windows for_each.
	t.Run("scenario 7: windows_settings on a Linux VM warns at plan and still sends the values", func(t *testing.T) {
		factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
		computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
		vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
		})

		vmID := testVMID(testSubscriptionID, "vm1")
		vgID := testVGID("pool1", "vg1")
		cfg := oneVGConfig(vmID, vgID, `
  reboot_allowed = true

  windows_settings {
    disk_timeout_in_seconds   = 240
    link_down_time_in_seconds = 180
  }
`)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: cfg,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(testVMConnResourceAddr, "extension_type", "MountIscsiLinux"),
						resource.TestCheckResourceAttr(testVMConnResourceAddr, "reboot_allowed", "true"),
						resource.TestCheckResourceAttr(testVMConnResourceAddr, "windows_settings.0.disk_timeout_in_seconds", "240"),
					),
				},
			},
		})

		log := computeStore.operationLog()
		if len(log) == 0 {
			t.Fatal("expected at least one recorded PUT")
		}
		settings := pinnedSettings(t, log[0])
		if !settings.Reboot {
			t.Fatal("expected reboot: true to still be sent on a Linux VM")
		}
		if settings.AdvancedWindowsSettings.DiskTimeout != 240 || settings.AdvancedWindowsSettings.LinkDownTime != 180 {
			t.Fatalf("expected windows_settings to still be sent on a Linux VM, got %+v", settings.AdvancedWindowsSettings)
		}
	})

	t.Run("scenario 9: import", func(t *testing.T) {
		t.Run("ImportStateVerify ignoring volume_group_ids", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			vmID := testVMID(testSubscriptionID, "vm1")
			vgID := testVGID("pool1", "vg1")
			cfg := oneVGConfig(vmID, vgID, "")
			extID := testExtensionID("vm1", defaultExtensionName())

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: cfg},
					{
						ResourceName:            testVMConnResourceAddr,
						ImportState:             true,
						ImportStateId:           extID,
						ImportStateVerify:       true,
						ImportStateVerifyIgnore: []string{"volume_group_ids"},
					},
				},
			})
			// Import here is discarded (no ImportStatePersist); this step only checks
			// ImportStateVerify's attribute comparison against the state the create left.
		})

		t.Run("an import-persisted extension whose settings already match this provider's own shape applies without a reconciling PUT", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			extName := defaultExtensionName()
			// Matches exactly what the config below would produce (session count 2,
			// reboot false, no blocks), so Update finds nothing to reconcile.
			computeStore.seedExtension(testResourceGroup, "vm1", extName, extensionPublisher, extensionTypeLinux, map[string]any{
				"targets": []map[string]any{
					{"ip": "10.0.1.4", "port": float64(3260), "iqn": "iqn.2010-01.com.purestorage.psc:aaa-vg1"},
				},
				"replaceTargets":          true,
				"sessionCount":            float64(2),
				"reboot":                  false,
				"autoUpdateConfiguration": map[string]any{"enabled": false, "allowRebootOnAutoUpdate": false},
				"advancedWindowsSettings": map[string]any{"diskTimeout": float64(0), "linkDownTime": float64(0)},
			})

			vmID := testVMID(testSubscriptionID, "vm1")
			vgID := testVGID("pool1", "vg1")
			cfg := oneVGConfig(vmID, vgID, "")
			extID := testExtensionID("vm1", extName)

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config:             cfg,
						ResourceName:       testVMConnResourceAddr,
						ImportState:        true,
						ImportStateId:      extID,
						ImportStatePersist: true,
						ImportStateVerify:  false,
					},
					{
						// volume_group_ids is unset right after import, so this plan shows
						// an update, not an empty plan; the point is that Update sends no
						// PUT because the seeded settings already match.
						Config: cfg,
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{
								plancheck.ExpectResourceAction(testVMConnResourceAddr, plancheck.ResourceActionUpdate),
							},
						},
					},
				},
			})

			// Teardown destroys the resource after Test returns (clearing PUT then
			// DELETE); Update itself must not have sent a PUT, so exactly those two
			// ops are expected, in that order.
			log := computeStore.operationLog()
			if len(log) != 2 || log[0].Op != "PUT" || log[1].Op != "DELETE" {
				t.Fatalf("expected only [teardown's clearing PUT, DELETE] with no reconciling PUT, got %+v", log)
			}
			clearing := pinnedSettings(t, log[0])
			if !clearing.ReplaceTargets || len(clearing.Targets) != 0 {
				t.Fatalf("expected log[0] to be the teardown's clearing PUT, got %+v", clearing)
			}
		})

		t.Run("a Portal-shaped extension (no replaceTargets) reconciles on the next apply", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			extName := defaultExtensionName()
			// The Portal's settings shape has sessionCount and targets but no
			// replaceTargets key; this provider always sends one, so that's the only
			// wire difference driving the reconciling PUT below.
			computeStore.seedExtension(testResourceGroup, "vm1", extName, extensionPublisher, extensionTypeLinux, map[string]any{
				"targets": []map[string]any{
					{"ip": "10.0.1.4", "port": float64(3260), "iqn": "iqn.2010-01.com.purestorage.psc:aaa-vg1"},
				},
				"sessionCount": float64(2),
			})

			vmID := testVMID(testSubscriptionID, "vm1")
			vgID := testVGID("pool1", "vg1")
			cfg := oneVGConfig(vmID, vgID, "")
			extID := testExtensionID("vm1", extName)

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config:             cfg,
						ResourceName:       testVMConnResourceAddr,
						ImportState:        true,
						ImportStateId:      extID,
						ImportStatePersist: true,
						ImportStateVerify:  false,
					},
					{
						Config: cfg,
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{
								plancheck.ExpectResourceAction(testVMConnResourceAddr, plancheck.ResourceActionUpdate),
							},
						},
					},
				},
			})

			// log[0] is the reconciling PUT this subtest is about; teardown adds the
			// clearing PUT and DELETE.
			log := computeStore.operationLog()
			if len(log) != 3 || log[0].Op != "PUT" || log[1].Op != "PUT" || log[2].Op != "DELETE" {
				t.Fatalf("expected [reconciling PUT, teardown's clearing PUT, DELETE], got %+v", log)
			}
			settings := pinnedSettings(t, log[0])
			if !settings.ReplaceTargets {
				t.Fatal("expected the reconciling PUT to carry replaceTargets: true")
			}
		})

		t.Run("an extension of another publisher or type is rejected", func(t *testing.T) {
			factories, _, _, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			computeStore.seedExtension(testResourceGroup, "vm1", "CustomScript", "Microsoft.Azure.Extensions", "CustomScript", map[string]any{})

			vmID := testVMID(testSubscriptionID, "vm1")
			cfg := oneVGConfig(vmID, testVGID("pool1", "vg1"), "")
			extID := testExtensionID("vm1", "CustomScript")

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config:        cfg,
						ResourceName:  testVMConnResourceAddr,
						ImportState:   true,
						ImportStateId: extID,
						ExpectError:   diagRegexp((operation{action: actionRetrieve, kind: kindVirtualMachineConnection, id: extID, unnamed: true}).fail(newExtensionReplacedOutOfBandError(extID))),
					},
				},
			})
		})
	})

	t.Run("scenario 10: drift", func(t *testing.T) {
		t.Run("settings edited out of band show a non-empty plan", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			vmID := testVMID(testSubscriptionID, "vm1")
			vgID := testVGID("pool1", "vg1")
			cfg := oneVGConfig(vmID, vgID, "")

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: cfg},
					{
						PreConfig: func() {
							computeStore.mutateExtensionSettings(testResourceGroup, "vm1", defaultExtensionName(), map[string]any{
								"targets": []any{}, "replaceTargets": true, "sessionCount": float64(9),
							})
						},
						Config:   cfg,
						PlanOnly: true,
						// A plan without a refresh diffs the config against the cached
						// state and can't see drift in the live backend; only the
						// refresh-including PostApplyPostRefresh check observes it.
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PostApplyPostRefresh: []plancheck.PlanCheck{
								plancheck.ExpectResourceAction(testVMConnResourceAddr, plancheck.ResourceActionUpdate),
							},
						},
						ExpectNonEmptyPlan: true,
					},
				},
			})
		})

		for _, tc := range []struct {
			state   string
			wantTag bool
		}{
			{"Failed", true},
			{"Succeeded", false},
		} {
			t.Run("updating a "+tc.state+" extension: forceUpdateTag sent = "+strconv.FormatBool(tc.wantTag), func(t *testing.T) {
				factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
				computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
				vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
					endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
				})

				cfg := oneVGConfig(testVMID(testSubscriptionID, "vm1"), testVGID("pool1", "vg1"), "")

				resource.Test(t, resource.TestCase{
					IsUnitTest:               true,
					ProtoV6ProviderFactories: factories,
					Steps: []resource.TestStep{
						{Config: cfg},
						{
							PreConfig: func() {
								computeStore.mutateExtensionSettings(testResourceGroup, "vm1", defaultExtensionName(), map[string]any{
									"targets": []any{}, "replaceTargets": true, "sessionCount": float64(9),
								})
								computeStore.setExtensionProvisioningState(testResourceGroup, "vm1", defaultExtensionName(), tc.state)
							},
							Config: cfg,
							Check: func(*terraform.State) error {
								tag := computeStore.extensionForceUpdateTag(testResourceGroup, "vm1", defaultExtensionName())
								if tc.wantTag && (tag == nil || *tag == "") {
									return fmt.Errorf("the PUT sent no forceUpdateTag for a %s extension", tc.state)
								}
								if !tc.wantTag && tag != nil {
									return fmt.Errorf("the PUT sent forceUpdateTag %q for a %s extension", *tag, tc.state)
								}
								return nil
							},
						},
					},
				})
			})
		}

		t.Run("an extension removed out of band plans a recreate", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			vmID := testVMID(testSubscriptionID, "vm1")
			vgID := testVGID("pool1", "vg1")
			cfg := oneVGConfig(vmID, vgID, "")

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: cfg},
					{
						PreConfig: func() {
							computeStore.removeExtensionOutOfBand(testResourceGroup, "vm1", defaultExtensionName())
						},
						Config:   cfg,
						PlanOnly: true,
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PostApplyPostRefresh: []plancheck.PlanCheck{
								plancheck.ExpectResourceAction(testVMConnResourceAddr, plancheck.ResourceActionCreate),
							},
						},
						ExpectNonEmptyPlan: true,
					},
				},
			})
		})
	})

	t.Run("scenario 11: destroy", func(t *testing.T) {
		t.Run("clearing PUT (body pinned) is observed before DELETE", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			vmID := testVMID(testSubscriptionID, "vm1")
			vgID := testVGID("pool1", "vg1")
			cfg := oneVGConfig(vmID, vgID, "")

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: cfg},
				},
			})
			// Teardown destroys the resource after the last step, recording the
			// clearing PUT and DELETE before Test returns.

			log := computeStore.operationLog()
			if len(log) != 3 || log[0].Op != "PUT" || log[1].Op != "PUT" || log[2].Op != "DELETE" {
				t.Fatalf("expected [create PUT, clearing PUT, DELETE], got %+v", log)
			}
			raw, ok := log[1].Body.Properties.Settings.(map[string]any)
			if !ok {
				t.Fatalf("expected the clearing PUT's settings to decode as a map, got %T", log[1].Body.Properties.Settings)
			}
			if len(raw) != 2 {
				t.Fatalf("expected exactly {targets, replaceTargets} on the clearing PUT, got %+v", raw)
			}
			targets, _ := raw["targets"].([]any)
			if len(targets) != 0 {
				t.Fatalf("expected an empty targets array on the clearing PUT, got %+v", raw["targets"])
			}
			if replaceTargets, _ := raw["replaceTargets"].(bool); !replaceTargets {
				t.Fatalf("expected replaceTargets: true on the clearing PUT, got %+v", raw["replaceTargets"])
			}
		})

		t.Run("a failed clearing PUT blocks destroy with the start-the-VM/state-rm message", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			vmID := testVMID(testSubscriptionID, "vm1")
			vgID := testVGID("pool1", "vg1")
			cfg := oneVGConfig(vmID, vgID, "")

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config: cfg,
						Check: func(*terraform.State) error {
							computeStore.queuePutFailure(testResourceGroup, "vm1", defaultExtensionName(),
								500, "InternalError", "simulated: virtual machine agent unreachable")
							return nil
						},
					},
					{
						Config:      cfg,
						Destroy:     true,
						ExpectError: regexp.MustCompile(causeRegexp(newIscsiClearingFailedError(apiFailure{})).String() + ".*x-ms-correlation-request-id"),
					},
				},
			})
			// The queued failure is one-shot: the automatic post-test destroy retries
			// against a fresh store and succeeds, leaving no leftover state.
		})

		t.Run("destroy succeeds quietly when the extension is already gone", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			vmID := testVMID(testSubscriptionID, "vm1")
			vgID := testVGID("pool1", "vg1")
			cfg := oneVGConfig(vmID, vgID, "")

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: cfg},
					{
						// PreConfig, not Check: a Check-time mutation would run before
						// this step's own post-apply empty-plan validation, which would
						// see the drift meant for destroy.
						PreConfig: func() {
							// Stands in for the virtual machine (and its extension) being
							// gone entirely; Delete treats a 404 on the extension's Get
							// the same way either way.
							computeStore.removeExtensionOutOfBand(testResourceGroup, "vm1", defaultExtensionName())
						},
						Config:  cfg,
						Destroy: true,
					},
				},
			})
		})

		t.Run("destroy succeeds when the extension vanishes between the clearing GET and the clearing PUT itself", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			vmID := testVMID(testSubscriptionID, "vm1")
			vgID := testVGID("pool1", "vg1")
			cfg := oneVGConfig(vmID, vgID, "")

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config: cfg,
						Check: func(*terraform.State) error {
							// The Get still succeeds; only the clearing PUT itself 404s,
							// as if the extension disappeared in the window between the
							// two calls.
							computeStore.queuePutFailure(testResourceGroup, "vm1", defaultExtensionName(),
								http.StatusNotFound, "ResourceNotFound", "simulated: the virtual machine was deleted just before the clearing PUT")
							return nil
						},
					},
					{
						Config:  cfg,
						Destroy: true,
					},
				},
			})
		})

		t.Run("install ending reboot-required, destroy's clearing PUT failing the same way still succeeds", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesWindows, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			vmID := testVMID(testSubscriptionID, "vm1")
			vgID := testVGID("pool1", "vg1")
			cfg := oneVGConfig(vmID, vgID, "")
			extName := defaultExtensionName()

			rebootRequired := &armcompute.InstanceViewStatus{
				Code: to.Ptr("ProvisioningState/failed/0"), Level: to.Ptr(armcompute.StatusLevelTypesError),
				Message: to.Ptr(handlerMessageRebootRequired),
			}
			// The install ends Failed/reboot-required: the guest never rebooted, so it
			// starts with zero iSCSI sessions, targets or portals.
			computeStore.queueLROFailure(testResourceGroup, "vm1", extName, rebootRequired)

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config: cfg,
						Check: func(*terraform.State) error {
							// The clearing PUT fails the same way: reboot is still
							// required, so there's nothing to disconnect.
							computeStore.queueLROFailure(testResourceGroup, "vm1", extName, rebootRequired)
							return nil
						},
					},
					{
						Config:  cfg,
						Destroy: true,
					},
				},
			})

			log := computeStore.operationLog()
			if len(log) != 3 || log[0].Op != "PUT" || log[1].Op != "PUT" || log[2].Op != "DELETE" {
				t.Fatalf("expected [create PUT, clearing PUT, DELETE] despite both PUTs' LROs failing reboot-required, got %+v", log)
			}
		})

		t.Run("a clearing PUT failing with a genuine (non-reboot-required) error still blocks destroy", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			vmID := testVMID(testSubscriptionID, "vm1")
			vgID := testVGID("pool1", "vg1")
			cfg := oneVGConfig(vmID, vgID, "")
			extName := defaultExtensionName()

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config: cfg,
						Check: func(*terraform.State) error {
							computeStore.queueLROFailure(testResourceGroup, "vm1", extName, &armcompute.InstanceViewStatus{
								Code: to.Ptr("ProvisioningState/failed/0"), Level: to.Ptr(armcompute.StatusLevelTypesError),
								Message: to.Ptr("Enable failed: iSCSI configuration failed for steps: iscsi-setup. Details: could not connect to iSCSI target 10.0.1.4:3260, iqn.x."),
							})
							return nil
						},
					},
					{
						Config:      cfg,
						Destroy:     true,
						ExpectError: causeRegexp(newIscsiClearingFailedError(apiFailure{})),
					},
				},
			})
			// The queued failure is one-shot and already spent: the automatic post-test
			// destroy retries against a fresh store and succeeds, leaving no leftover
			// state.
		})
	})

	t.Run("scenario 12: mapping-contract variants", func(t *testing.T) {
		t.Run("re-casing the VM ID and extension name in the fake doesn't overwrite state's own casing", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "VM1", armcompute.OperatingSystemTypesLinux, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			vmID := testVMID(testSubscriptionID, "VM1")
			vgID := testVGID("pool1", "vg1")
			extName := "MyCustom-Ext"
			extID := testExtensionID("VM1", extName)
			cfg := oneVGConfig(vmID, vgID, fmt.Sprintf("  extension_name = %q\n", extName))

			checkSpellingPreserved := resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(testVMConnResourceAddr, "virtual_machine_id", vmID),
				resource.TestCheckResourceAttr(testVMConnResourceAddr, "extension_name", extName),
				resource.TestCheckResourceAttr(testVMConnResourceAddr, "id", extID),
			)

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config: cfg,
						Check:  checkSpellingPreserved,
					},
					{
						// A real step, not PlanOnly: PlanOnly steps skip their own Check,
						// so this makes sure checkSpellingPreserved actually runs against
						// the re-cased response.
						PreConfig: func() { computeStore.recaseIDs = true },
						Config:    cfg,
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
						},
						Check: checkSpellingPreserved,
					},
				},
			})
		})

		t.Run("an extension with nil Settings maps to all-default values without crashing", func(t *testing.T) {
			factories, _, _, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			extName := defaultExtensionName()
			computeStore.seedExtension(testResourceGroup, "vm1", extName, extensionPublisher, extensionTypeLinux, nil)

			vmID := testVMID(testSubscriptionID, "vm1")
			cfg := oneVGConfig(vmID, testVGID("pool1", "vg1"), "")
			extID := testExtensionID("vm1", extName)

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config:        cfg,
						ResourceName:  testVMConnResourceAddr,
						ImportState:   true,
						ImportStateId: extID,
						ImportStateCheck: func(states []*terraform.InstanceState) error {
							if len(states) != 1 {
								return fmt.Errorf("expected exactly one imported instance state, got %d", len(states))
							}
							s := states[0]
							if s.Attributes["iscsi_session_count"] != "0" {
								return fmt.Errorf("iscsi_session_count = %q, want \"0\" (nil Settings has no sessionCount key)", s.Attributes["iscsi_session_count"])
							}
							if s.Attributes["resolved_targets.#"] != "0" {
								return fmt.Errorf("resolved_targets.# = %q, want \"0\"", s.Attributes["resolved_targets.#"])
							}
							if s.Attributes["reboot_allowed"] != "false" {
								return fmt.Errorf("reboot_allowed = %q, want \"false\"", s.Attributes["reboot_allowed"])
							}
							return nil
						},
					},
				},
			})
		})
	})

	t.Run("scenario 13: an existing extension of our type blocks create with an import hint", func(t *testing.T) {
		t.Run("under the name this resource would use", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			extName := defaultExtensionName()
			computeStore.seedExtension(testResourceGroup, "vm1", extName, extensionPublisher, extensionTypeLinux, map[string]any{})
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			vmID := testVMID(testSubscriptionID, "vm1")
			cfg := oneVGConfig(vmID, testVGID("pool1", "vg1"), "")

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config:      cfg,
						ExpectError: causeRegexp(newExtensionAlreadyExistsError(testExtensionID("vm1", extName))),
					},
				},
			})

			if log := computeStore.operationLog(); len(log) != 0 {
				t.Fatalf("expected no PUT to have been attempted, got %+v", log)
			}
		})

		t.Run("under a different name than this resource would use", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			computeStore.seedExtension(testResourceGroup, "vm1", "PureStorageMountIscsi", extensionPublisher, extensionTypeLinux, map[string]any{})
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			vmID := testVMID(testSubscriptionID, "vm1")
			cfg := oneVGConfig(vmID, testVGID("pool1", "vg1"), "")

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config:      cfg,
						ExpectError: causeRegexp(newExtensionAlreadyExistsError(testExtensionID("vm1", "PureStorageMountIscsi"))),
					},
				},
			})

			if log := computeStore.operationLog(); len(log) != 0 {
				t.Fatalf("expected no PUT to have been attempted, got %+v", log)
			}
		})

		t.Run("a race the List-based check misses still surfaces Azure's own 400 with request-tracing IDs", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			extName := defaultExtensionName()
			// Stands in for another writer creating the handler's other instance in the
			// window between this resource's own List check and its PUT: the fake queues
			// the same synchronous 400 body Azure's real duplicate-handler check returns,
			// which the List above never saw.
			computeStore.queuePutFailure(testResourceGroup, "vm1", extName, http.StatusBadRequest, "BadRequest",
				duplicateExtensionMessage("Linux", "PureStorage.Extensions.MountIscsiLinux", extName))

			vmID := testVMID(testSubscriptionID, "vm1")
			cfg := oneVGConfig(vmID, testVGID("pool1", "vg1"), "")

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config: cfg,
						ExpectError: regexp.MustCompile(causeRegexp(newExtensionOfTypeAlreadyExistsError(vmID, extensionTypeLinux, &apiFailure{})).String() +
							".*x-ms-correlation-request-id"),
					},
				},
			})
		})
	})

	// A genuinely failed LRO (not the reboot-required outcome) fails apply with the
	// handler's own message and taints the resource.
	t.Run("scenario 14: a genuinely failed LRO", func(t *testing.T) {
		t.Run("create fails with the handler's own message, taints the resource, and the next plan replaces it", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			vmID := testVMID(testSubscriptionID, "vm1")
			vgID := testVGID("pool1", "vg1")
			cfg := oneVGConfig(vmID, vgID, "")

			failMessage := "Enable failed: iSCSI configuration failed for steps: iscsi-setup. Details: could not " +
				"connect to iSCSI target 10.0.1.4:3260, iqn.2010-01.com.purestorage.psc:aaa-vg1 - make sure it " +
				"exists and has volumes."
			computeStore.queueLROFailure(testResourceGroup, "vm1", defaultExtensionName(), &armcompute.InstanceViewStatus{
				Code: to.Ptr("ProvisioningState/failed/0"), Level: to.Ptr(armcompute.StatusLevelTypesError),
				Message: to.Ptr(failMessage),
			})

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config:      cfg,
						ExpectError: regexp.MustCompile(regexp.QuoteMeta("could not connect to iSCSI target")),
					},
					{
						// Create's early resp.State.Set left a tainted resource in state
						// despite the error above; the next plan must replace it, not
						// update or leave it empty.
						Config: cfg,
						// PlanOnly steps disallow PreApply plan checks; PostApplyPostRefresh
						// is used instead.
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PostApplyPostRefresh: []plancheck.PlanCheck{
								plancheck.ExpectResourceAction(testVMConnResourceAddr, plancheck.ResourceActionReplace),
							},
						},
						PlanOnly:           true,
						ExpectNonEmptyPlan: true,
					},
				},
			})
			// Teardown destroys the tainted resource, proving destroy succeeds even
			// though the resource never finished configuring.
		})

		t.Run("an Update LRO failing while the instance view still shows the previous success does not surface as \"iSCSI configuration failed\" with stale success text", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg2", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg2", 3260),
			})

			vmID := testVMID(testSubscriptionID, "vm1")
			vg1 := testVGID("pool1", "vg1")
			vg2 := testVGID("pool1", "vg2")
			cfgOneVG := oneVGConfig(vmID, vg1, "")
			cfgTwoVGs := twoVGConfig(vmID, vg1, vg2, "")
			extName := defaultExtensionName()

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: cfgOneVG}, // creates successfully; the instance view now holds the default success status.
					{
						PreConfig: func() {
							computeStore.queueLROFailurePreservingInstanceView(testResourceGroup, "vm1", extName)
						},
						Config: cfgTwoVGs,
						// classifyLROFailure falls back to the LRO's own error since the
						// instance view it fetched is a stale success, not a failure; a
						// regression would instead summarize this as "iSCSI configuration
						// failed" with the stale success text.
						ExpectError: summaryRegexp((operation{action: actionUpdate, kind: kindVirtualMachineConnection, unnamed: true}).fail(detailError{})),
					},
				},
			})
		})
	})

	t.Run("scenario 15: plan fails when virtual_machine_id names a virtual machine the fake doesn't have", func(t *testing.T) {
		factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
		vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
		})

		// Deliberately not seeded: ModifyPlan's own Get must 404 the same way Create's
		// would, and fail planning instead of deferring to a doomed apply.
		vmID := testVMID(testSubscriptionID, "missing-vm")
		vgID := testVGID("pool1", "vg1")
		cfg := oneVGConfig(vmID, vgID, "")

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config:      cfg,
					ExpectError: diagRegexp(diag.WithPath(path.Root("virtual_machine_id"), invalidAttributeValueDiagnostic(newNotFoundError(kindVirtualMachine, vmID, nil, apiFailure{})))),
				},
			},
		})

		if log := computeStore.operationLog(); len(log) != 0 {
			t.Fatalf("expected no PUT to have been attempted, got %+v", log)
		}
	})

	t.Run("scenario 16: the virtual machine's operating system changing between plans replaces the resource", func(t *testing.T) {
		factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
		computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
		vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
		})

		vmID := testVMID(testSubscriptionID, "vm1")
		vgID := testVGID("pool1", "vg1")
		cfg := oneVGConfig(vmID, vgID, "")

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: cfg}, // creates against the Linux VM.
				{
					PreConfig: func() {
						// Stands in for the same virtual machine being rebuilt with a
						// different operating system outside Terraform: the extension type
						// this resource would now derive disagrees with the one already in
						// state, and Update has no way to PUT a different extension type in
						// place (it PUTs whatever prior state already holds).
						computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesWindows, "eastus2")
					},
					Config: cfg,
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction(testVMConnResourceAddr, plancheck.ResourceActionReplace),
						},
					},
					Check: resource.TestCheckResourceAttr(testVMConnResourceAddr, "extension_type", "MountIscsiWindows"),
				},
			},
		})

		log := computeStore.operationLog()
		linuxKey := computeExtensionKey(testResourceGroup, "vm1", defaultExtensionName())
		windowsKey := computeExtensionKey(testResourceGroup, "vm1", defaultExtensionName())
		createIdx, deleteLinuxIdx, putWindowsIdx := -1, -1, -1
		for i, e := range log {
			switch {
			case createIdx == -1 && e.Op == "PUT" && e.Key == linuxKey:
				createIdx = i
			case deleteLinuxIdx == -1 && e.Op == "DELETE" && e.Key == linuxKey:
				deleteLinuxIdx = i
			case putWindowsIdx == -1 && e.Op == "PUT" && e.Key == windowsKey:
				putWindowsIdx = i
			}
		}
		if createIdx == -1 {
			t.Fatalf("expected the step-1 create's PUT against the Linux extension, got %+v", log)
		}
		// A DELETE against the old (Linux) extension key proves the replace actually
		// removed it, rather than just this loop matching the step-1 create's own PUT.
		if deleteLinuxIdx == -1 || deleteLinuxIdx <= createIdx {
			t.Fatalf("expected a DELETE against the Linux extension after the step-1 create (index %d), got %+v", createIdx, log)
		}
		if putWindowsIdx == -1 {
			t.Fatalf("expected a PUT against the new Windows extension, got %+v", log)
		}
	})

	// The handler reports a reboot as success, then transitioning once the guest's
	// agent re-runs it, and only minutes later its real result. Apply must not stop at
	// any of the earlier statuses.
	t.Run("scenario 17: Windows create that fails after the reboot reports the failure, not Creation complete", func(t *testing.T) {
		origInterval := instanceViewPollInterval
		instanceViewPollInterval = 10 * time.Millisecond
		defer func() { instanceViewPollInterval = origInterval }()

		factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
		computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesWindows, "eastus2")
		vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.250.142.132", "iqn.2010-01.com.purestorage.psc:8d4a9d0e421a1464e218-empty-vg", 3260),
		})

		vmID := testVMID(testSubscriptionID, "vm1")
		vgID := testVGID("pool1", "vg1")
		cfg := oneVGConfig(vmID, vgID, "  reboot_allowed = true\n")

		failMessage := "Enable failed: iSCSI configuration failed for steps: iscsi-mount.\nDetails: iscsi-mount: " +
			"setting up iSCSI sessions: could not connect to iSCSI target 10.250.142.132:3260, " +
			"iqn.2010-01.com.purestorage.psc:8d4a9d0e421a1464e218-empty-vg - make sure it exists and has volumes."
		computeStore.setInstanceViewSequence(testResourceGroup, "vm1", defaultExtensionName(),
			&armcompute.InstanceViewStatus{
				Code: to.Ptr("ProvisioningState/succeeded"), Level: to.Ptr(armcompute.StatusLevelTypesInfo),
				Message: to.Ptr("Enable succeeded: Rebooting as requested..."),
			},
			&armcompute.InstanceViewStatus{
				Code: to.Ptr("ProvisioningState/transitioning"), Level: to.Ptr(armcompute.StatusLevelTypesInfo),
				Message: to.Ptr("Enable in progress: Setting up iSCSI configuration"),
			},
			&armcompute.InstanceViewStatus{
				Code: to.Ptr("ProvisioningState/failed/0"), Level: to.Ptr(armcompute.StatusLevelTypesError),
				Message: to.Ptr(failMessage),
			},
		)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config:      cfg,
					ExpectError: regexp.MustCompile(`(?s)iSCSI\s+configuration\s+failed\s+for\s+steps:\s+iscsi-mount.*could\s+not\s+connect\s+to\s+iSCSI\s+target`),
				},
			},
		})

		if got := computeStore.expandedInstanceViewGetCount(testResourceGroup, "vm1", defaultExtensionName()); got < 3 {
			t.Fatalf("expected apply to poll through all 3 scripted statuses, got %d expanded GETs", got)
		}
	})

	t.Run("scenario 18: Windows create ending in an unrecognized status code errors", func(t *testing.T) {
		origInterval := instanceViewPollInterval
		instanceViewPollInterval = 10 * time.Millisecond
		defer func() { instanceViewPollInterval = origInterval }()

		factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
		computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesWindows, "eastus2")
		vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
		})

		cfg := oneVGConfig(testVMID(testSubscriptionID, "vm1"), testVGID("pool1", "vg1"), "  reboot_allowed = true\n")

		computeStore.setInstanceViewSequence(testResourceGroup, "vm1", defaultExtensionName(),
			&armcompute.InstanceViewStatus{
				Code: to.Ptr("ProvisioningState/transitioning"), Level: to.Ptr(armcompute.StatusLevelTypesInfo),
				Message: to.Ptr("Enable in progress: Setting up iSCSI configuration"),
			},
			&armcompute.InstanceViewStatus{
				Code: to.Ptr("ProvisioningState/warning"), Level: to.Ptr(armcompute.StatusLevelTypesWarning),
				Message: to.Ptr("Enable completed with warnings"),
			},
		)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config:      cfg,
					ExpectError: regexp.MustCompile(`(?s)does\s+not\s+recognise.*ProvisioningState/warning`),
				},
			},
		})
	})
}

// TestVirtualMachineConnectionResourceDynamicBlocks checks that a dynamic block whose
// for_each derives from a variable doesn't crash validation, and that an unknown
// volume_group_ids set leaves resolved_targets unknown at plan and resolves it at apply.
func TestVirtualMachineConnectionResourceDynamicBlocks(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	t.Run("reapply_on_boot via a dynamic block", func(t *testing.T) {
		vmID := testVMID(testSubscriptionID, "vm1")
		vgID := testVGID("pool1", "vg1")

		cfg := providerBlock() + fmt.Sprintf(`
variable "reapply" {
  type = list(object({
    enabled                   = bool
    reboot_on_reapply_allowed = bool
  }))
  default = []
}

resource "everpure_azure_virtual_machine_connection" "test" {
  virtual_machine_id = %[1]q
  volume_group_ids    = [%[2]q]

  dynamic "reapply_on_boot" {
    for_each = var.reapply
    content {
      enabled                   = reapply_on_boot.value.enabled
      reboot_on_reapply_allowed = reapply_on_boot.value.reboot_on_reapply_allowed
    }
  }
}
`, vmID, vgID)

		t.Run("one element: plan and apply succeed, the block lands in state", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config: cfg,
						ConfigVariables: config.Variables{
							"reapply": config.ListVariable(config.ObjectVariable(map[string]config.Variable{
								"enabled":                   config.BoolVariable(true),
								"reboot_on_reapply_allowed": config.BoolVariable(true),
							})),
						},
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(testVMConnResourceAddr, "reapply_on_boot.#", "1"),
							resource.TestCheckResourceAttr(testVMConnResourceAddr, "reapply_on_boot.0.enabled", "true"),
							resource.TestCheckResourceAttr(testVMConnResourceAddr, "reapply_on_boot.0.reboot_on_reapply_allowed", "true"),
						),
					},
				},
			})
		})

		t.Run("empty list (the variable's default): applies clean with no block", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config: cfg,
						Check:  resource.TestCheckResourceAttr(testVMConnResourceAddr, "reapply_on_boot.#", "0"),
					},
				},
			})
		})
	})

	t.Run("windows_settings via a dynamic block", func(t *testing.T) {
		vmID := testVMID(testSubscriptionID, "vm1")
		vgID := testVGID("pool1", "vg1")

		cfg := providerBlock() + fmt.Sprintf(`
variable "windows" {
  type = list(object({
    disk_timeout_in_seconds   = number
    link_down_time_in_seconds = number
  }))
  default = []
}

resource "everpure_azure_virtual_machine_connection" "test" {
  virtual_machine_id = %[1]q
  volume_group_ids    = [%[2]q]

  dynamic "windows_settings" {
    for_each = var.windows
    content {
      disk_timeout_in_seconds   = windows_settings.value.disk_timeout_in_seconds
      link_down_time_in_seconds = windows_settings.value.link_down_time_in_seconds
    }
  }
}
`, vmID, vgID)

		t.Run("one element: plan and apply succeed, the block lands in state", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config: cfg,
						ConfigVariables: config.Variables{
							"windows": config.ListVariable(config.ObjectVariable(map[string]config.Variable{
								"disk_timeout_in_seconds":   config.IntegerVariable(240),
								"link_down_time_in_seconds": config.IntegerVariable(180),
							})),
						},
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(testVMConnResourceAddr, "windows_settings.#", "1"),
							resource.TestCheckResourceAttr(testVMConnResourceAddr, "windows_settings.0.disk_timeout_in_seconds", "240"),
							resource.TestCheckResourceAttr(testVMConnResourceAddr, "windows_settings.0.link_down_time_in_seconds", "180"),
						),
					},
				},
			})
		})

		t.Run("empty list (the variable's default): applies clean with no block", func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{
						Config: cfg,
						Check:  resource.TestCheckResourceAttr(testVMConnResourceAddr, "windows_settings.#", "0"),
					},
				},
			})
		})
	})

	// The volume group's ID is unknown at plan time since it's created in this same
	// config.
	t.Run("unknown volume_group_ids (volume group created in the same config)", func(t *testing.T) {
		factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
		computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
		vgStore.setConnectionParameters(testResourceGroup, testStoragePoolName, "vg-dyn", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg-dyn", 3260),
		})

		vmID := testVMID(testSubscriptionID, "vm1")
		cfg := providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg-dyn"
  storage_pool_id     = %[1]q
  deletion_protection = false
}

resource "everpure_azure_virtual_machine_connection" "test" {
  virtual_machine_id = %[2]q
  volume_group_ids    = [everpure_azure_volume_group.test.id]
}
`, testStoragePoolIDForVG(), vmID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: cfg,
					ConfigPlanChecks: resource.ConfigPlanChecks{
						PreApply: []plancheck.PlanCheck{
							plancheck.ExpectUnknownValue(testVMConnResourceAddr, tfjsonpath.New("resolved_targets")),
						},
					},
					Check: resource.TestCheckResourceAttr(testVMConnResourceAddr, "resolved_targets.#", "1"),
				},
			},
		})
	})
}

// hangingTransport blocks every request whose client name starts with prefix until the
// request context ends, then fails with the context's error, and passes everything else
// to next. It models an ARM endpoint that accepts a connection and never answers.
type hangingTransport struct {
	prefix string
	next   policy.Transporter
}

func (h *hangingTransport) Do(req *http.Request) (*http.Response, error) {
	name, _ := req.Context().Value(runtime.CtxAPINameKey{}).(string)
	if strings.HasPrefix(name, h.prefix) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	return h.next.Do(req)
}

// TestVirtualMachineConnectionPlanTimeout checks that the ARM calls ModifyPlan makes are
// bounded by the configured read timeout, so a hung endpoint fails the plan instead of
// hanging it.
func TestVirtualMachineConnectionPlanTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	tests := []struct {
		name string
		// hangPrefix is the SDK client-name prefix of the calls that never answer.
		hangPrefix string
	}{
		{name: "virtual machine lookup hangs", hangPrefix: "VirtualMachine"},
		{name: "connection parameters lookup hangs", hangPrefix: "VolumeGroups"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, vgStore, computeStore, opts := newTestVMConnectionTransport()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})
			opts.Transport = &hangingTransport{prefix: tt.hangPrefix, next: opts.Transport}
			p := newProvider("test", fixedCredentialFactory(&azcorefake.TokenCredential{}), opts)()
			factories := map[string]func() (tfprotov6.ProviderServer, error){
				"everpure": providerserver.NewProtocol6WithError(p),
			}

			cfg := oneVGConfig(testVMID(testSubscriptionID, "vm1"), testVGID("pool1", "vg1"), `
  timeouts {
    read = "1s"
  }`)

			done := make(chan struct{})
			start := time.Now()
			go func() {
				defer close(done)
				resource.Test(t, resource.TestCase{
					IsUnitTest:               true,
					ProtoV6ProviderFactories: factories,
					Steps: []resource.TestStep{{
						Config:             cfg,
						PlanOnly:           true,
						ExpectError:        regexp.MustCompile(`(?s)Could not plan.*context deadline exceeded`),
						ExpectNonEmptyPlan: true,
					}},
				})
			}()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("plan still running after 30s, want it to fail at the 1s read timeout")
			}
			if got := time.Since(start); got > 20*time.Second {
				t.Fatalf("plan took %s, want it to fail shortly after the 1s read timeout", got)
			}
		})
	}
}

// TestVirtualMachineConnectionResourceConfigCasingChange: a case-only respelling of
// virtual_machine_id or of an element of volume_group_ids in config must plan nothing.
func TestVirtualMachineConnectionResourceConfigCasingChange(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	vmID := testVMID(testSubscriptionID, "vm1")
	vgID := testVGID("pool1", "vg1")

	for name, cfg := range map[string]string{
		"virtual machine id": oneVGConfig(strings.ToUpper(vmID), vgID, ""),
		"volume group id":    oneVGConfig(vmID, strings.ToUpper(vgID), ""),
	} {
		t.Run(name, func(t *testing.T) {
			factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
			computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
			vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
				endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			})

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories,
				Steps: []resource.TestStep{
					{Config: oneVGConfig(vmID, vgID, "")},
					{Config: cfg, PlanOnly: true},
				},
			})
		})
	}
}
