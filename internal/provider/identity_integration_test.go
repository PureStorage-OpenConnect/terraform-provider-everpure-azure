// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
)

// expectStoredIdentity records a resource's identity values from the state it is
// given, and on every later use asserts the state's identity is unchanged, so one
// instance threaded through several steps proves the identity is stable across
// refreshes.
type expectStoredIdentity struct {
	address string
	seen    map[string]any
}

func (e *expectStoredIdentity) CheckState(_ context.Context, req statecheck.CheckStateRequest, resp *statecheck.CheckStateResponse) {
	if req.State == nil || req.State.Values == nil || req.State.Values.RootModule == nil {
		resp.Error = errors.New("state has no root module")
		return
	}
	for _, r := range req.State.Values.RootModule.Resources {
		if r.Address != e.address {
			continue
		}
		if len(r.IdentityValues) == 0 {
			resp.Error = fmt.Errorf("%s has no identity in state", e.address)
			return
		}
		if e.seen == nil {
			e.seen = r.IdentityValues
			return
		}
		if fmt.Sprint(e.seen) != fmt.Sprint(r.IdentityValues) {
			resp.Error = fmt.Errorf("identity of %s changed: was %v, now %v", e.address, e.seen, r.IdentityValues)
		}
		return
	}
	resp.Error = fmt.Errorf("%s not found in state", e.address)
}

// expectPlannedIdentity asserts the planned value of a resource carries exactly the
// given identity, which is how an import by ID is shown to set identity.
type expectPlannedIdentity struct {
	address  string
	identity map[string]string
}

func (e expectPlannedIdentity) CheckPlan(_ context.Context, req plancheck.CheckPlanRequest, resp *plancheck.CheckPlanResponse) {
	if req.Plan == nil || req.Plan.PlannedValues == nil || req.Plan.PlannedValues.RootModule == nil {
		resp.Error = errors.New("plan has no planned root module")
		return
	}
	for _, r := range req.Plan.PlannedValues.RootModule.Resources {
		if r.Address != e.address {
			continue
		}
		if len(r.IdentityValues) != len(e.identity) {
			resp.Error = fmt.Errorf("planned identity of %s is %v, want %v", e.address, r.IdentityValues, e.identity)
			return
		}
		for k, want := range e.identity {
			if got, _ := r.IdentityValues[k].(string); got != want {
				resp.Error = fmt.Errorf("planned identity of %s is %v, want %v", e.address, r.IdentityValues, e.identity)
				return
			}
		}
		return
	}
	resp.Error = fmt.Errorf("%s not found in planned values", e.address)
}

func identityChecks(address string, identity map[string]string) statecheck.StateCheck {
	checks := make(map[string]knownvalue.Check, len(identity))
	for k, v := range identity {
		checks[k] = knownvalue.StringExact(v)
	}
	return statecheck.ExpectIdentity(address, checks)
}

// identityFixture is one resource's worth of inputs for TestResourceIdentity.
type identityFixture struct {
	address   string
	factories map[string]func() (tfprotov6.ProviderServer, error)
	config    string
	identity  map[string]string
	// recase makes the fake ARM echo differently-cased IDs from now on.
	recase func()
}

func reservationIdentityFixture() identityFixture {
	factories, store := newReservationTestProviderFactories()
	return identityFixture{
		address:   reservationAddr,
		factories: factories,
		config:    reservationConfig("psc_capacity_perf_ondemand", ""),
		identity: map[string]string{
			"subscription_id": testSubscriptionID, "resource_group_name": testResourceGroup, "name": "res1",
		},
		recase: func() {
			store.mutate(testResourceGroup, "res1", func(r *armpurestorageblock.Reservation) {
				r.ID = ptrTo(strings.ToUpper(*r.ID))
			})
		},
	}
}

func poolIdentityFixture() identityFixture {
	factories, store := newTestProviderFactories()
	return identityFixture{
		address:   "everpure_azure_storage_pool.test",
		factories: factories,
		config: providerBlock() + fmt.Sprintf(`
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
`, testResourceGroup, testReservationID(), testSubnetID("snet1")),
		identity: map[string]string{
			"subscription_id": testSubscriptionID, "resource_group_name": testResourceGroup, "name": "pool1",
		},
		recase: func() {
			store.mutatePool(testResourceGroup, "pool1", func(p *armpurestorageblock.StoragePool) {
				p.ID = ptrTo(strings.ToUpper(*p.ID))
			})
		},
	}
}

func volumeGroupIdentityFixture() identityFixture {
	factories, _, vgStore := newTestVolumeGroupProviderFactories()
	return identityFixture{
		address:   "everpure_azure_volume_group.test",
		factories: factories,
		config: providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name                = "vg1"
  storage_pool_id     = %[1]q
  deletion_protection = false
}
`, testStoragePoolIDForVG()),
		identity: map[string]string{
			"subscription_id": testSubscriptionID, "resource_group_name": testResourceGroup,
			"storage_pool_name": testStoragePoolName, "name": "vg1",
		},
		recase: func() {
			vgStore.mutateVolumeGroup(testResourceGroup, testStoragePoolName, "vg1", func(vg *armpurestorageblock.VolumeGroup) {
				vg.ID = ptrTo(strings.ToUpper(*vg.ID))
			})
		},
	}
}

func volumeIdentityFixture() identityFixture {
	factories, store := newTestVolumeProviderFactories()
	return identityFixture{
		address:   "everpure_azure_volume.test",
		factories: factories,
		config: providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                      = "vol1"
  volume_group_id           = %[1]q
  deletion_protection       = false
  provisioned_size_in_bytes = %[2]d
}
`, testVolumeGroupID("vg1"), int64(bytesPerGiB)),
		identity: map[string]string{
			"subscription_id": testSubscriptionID, "resource_group_name": testResourceGroup,
			"storage_pool_name": testStoragePoolName, "volume_group_name": "vg1", "name": "vol1",
		},
		recase: func() { store.recaseIDs = true },
	}
}

func snapshotIdentityFixture() identityFixture {
	factories, store := newTestSnapshotProviderFactories()
	return identityFixture{
		address:   "everpure_azure_snapshot.test",
		factories: factories,
		config:    snapshotConfig("snap1", testVolumeGroupID("vg1"), "  deletion_protection = false\n"),
		identity: map[string]string{
			"subscription_id": testSubscriptionID, "resource_group_name": testResourceGroup,
			"storage_pool_name": testStoragePoolName, "volume_group_name": "vg1", "name": "snap1",
		},
		recase: func() {
			store.mutateSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap1", func(snap *armpurestorageblock.VolumeGroupSnapshot) {
				snap.ID = ptrTo(strings.ToUpper(*snap.ID))
			})
		},
	}
}

func virtualMachineConnectionIdentityFixture() identityFixture {
	factories, _, vgStore, computeStore := newTestVMConnectionProviderFactories()
	computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
	vgStore.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
		endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
	})
	return identityFixture{
		address:   testVMConnResourceAddr,
		factories: factories,
		config:    oneVGConfig(testVMID(testSubscriptionID, "vm1"), testVGID("pool1", "vg1"), ""),
		identity: map[string]string{
			"subscription_id": testSubscriptionID, "resource_group_name": testResourceGroup,
			"virtual_machine_name": "vm1", "name": defaultExtensionName(),
		},
		recase: func() { computeStore.recaseIDs = true },
	}
}

func ptrTo[T any](v T) *T { return &v }

// TestResourceIdentity drives each resource through create, refresh, a refresh
// while ARM echoes differently-cased IDs, import by identity and import by ID,
// asserting the identity is set each time and never changes between reads.
func TestResourceIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	fixtures := map[string]func() identityFixture{
		"reservation":                reservationIdentityFixture,
		"storage pool":               poolIdentityFixture,
		"volume group":               volumeGroupIdentityFixture,
		"volume":                     volumeIdentityFixture,
		"snapshot":                   snapshotIdentityFixture,
		"virtual machine connection": virtualMachineConnectionIdentityFixture,
	}

	for name, build := range fixtures {
		t.Run(name, func(t *testing.T) {
			f := build()
			stable := &expectStoredIdentity{address: f.address}

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: f.factories,
				Steps: []resource.TestStep{
					{
						// Identity is set by create.
						Config: f.config,
						ConfigStateChecks: []statecheck.StateCheck{
							identityChecks(f.address, f.identity),
							stable,
						},
					},
					{
						// Identity is unchanged by a plain refresh.
						Config: f.config,
						ConfigStateChecks: []statecheck.StateCheck{
							identityChecks(f.address, f.identity),
							stable,
						},
					},
					{
						// ARM echoing other casing must not move the identity: a change
						// between reads is a hard Terraform error.
						PreConfig: f.recase,
						Config:    f.config,
						ConfigStateChecks: []statecheck.StateCheck{
							identityChecks(f.address, f.identity),
							stable,
						},
					},
					{
						// Import by identity resolves to the same identity.
						Config:             f.config,
						ResourceName:       f.address,
						ImportState:        true,
						ImportStateKind:    resource.ImportBlockWithResourceIdentity,
						ExpectNonEmptyPlan: true,
					},
					{
						// Import by ID sets the identity too.
						Config:             f.config,
						ResourceName:       f.address,
						ImportState:        true,
						ImportStateKind:    resource.ImportBlockWithID,
						ExpectNonEmptyPlan: true,
						ImportPlanChecks: resource.ImportPlanChecks{
							PreApply: []plancheck.PlanCheck{expectPlannedIdentity{address: f.address, identity: f.identity}},
						},
					},
				},
			})
		})
	}
}
