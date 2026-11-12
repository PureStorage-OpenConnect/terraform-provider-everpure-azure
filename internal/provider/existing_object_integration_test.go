// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// existingObjectEnv is one resource under test for a create that finds its name taken.
type existingObjectEnv struct {
	factories    map[string]func() (tfprotov6.ProviderServer, error)
	config       string
	addr         string
	kind         kind
	name         string
	resourceType string

	// configuredID is the ID the configuration spells for the object.
	configuredID string

	// seed makes the object exist with the given provisioning state, absent when "", and
	// the given ID, the default spelling when "".
	seed func(state, id string)

	// failGets makes every Get answer a server error.
	failGets func()

	// creates is how many creates reached the fake.
	creates func() int
}

func provisioningStatePtr[T ~string](state string) *T {
	if state == "" {
		return nil
	}
	return to.Ptr(T(state))
}

func existingPoolEnv() existingObjectEnv {
	factories, store := newTestProviderFactories()
	id := store.fakePoolID(testResourceGroup, "pool1")
	return existingObjectEnv{
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
		addr:         "everpure_azure_storage_pool.test",
		kind:         kindStoragePool,
		name:         "pool1",
		resourceType: "everpure_azure_storage_pool",
		configuredID: id,
		seed: func(state, seededID string) {
			seedTestPool(store, testResourceGroup, "pool1")
			store.mu.Lock()
			defer store.mu.Unlock()
			pool := store.pools[poolKey(testResourceGroup, "pool1")]
			pool.Properties.ProvisioningState = provisioningStatePtr[armpurestorageblock.ProvisioningState](state)
			if seededID != "" {
				pool.ID = to.Ptr(seededID)
			}
		},
		failGets: func() {
			store.mu.Lock()
			defer store.mu.Unlock()
			store.getFailStatus = 500
		},
		creates: func() int {
			store.mu.Lock()
			defer store.mu.Unlock()
			return store.createCallCount
		},
	}
}

func existingVolumeGroupEnv() existingObjectEnv {
	factories, _, store := newTestVolumeGroupProviderFactories()
	id := store.fakeVGID(testResourceGroup, testStoragePoolName, "vg1")
	return existingObjectEnv{
		factories: factories,
		config: providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume_group" "test" {
  name            = "vg1"
  storage_pool_id = %[1]q
}
`, testStoragePoolIDForVG()),
		addr:         "everpure_azure_volume_group.test",
		kind:         kindVolumeGroup,
		name:         "vg1",
		resourceType: "everpure_azure_volume_group",
		configuredID: id,
		seed: func(state, seededID string) {
			store.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg1", "PT1H", "P1D")
			store.mu.Lock()
			defer store.mu.Unlock()
			vg := store.groups[vgKey(testResourceGroup, testStoragePoolName, "vg1")]
			vg.Properties.ProvisioningState = provisioningStatePtr[armpurestorageblock.ProvisioningState](state)
			if seededID != "" {
				vg.ID = to.Ptr(seededID)
			}
		},
		failGets: func() {
			store.mu.Lock()
			defer store.mu.Unlock()
			store.failGetOnce = true
		},
		creates: func() int {
			store.mu.Lock()
			defer store.mu.Unlock()
			return store.createCallCount
		},
	}
}

func existingVolumeEnv() existingObjectEnv {
	factories, store := newTestVolumeProviderFactories()
	return existingObjectEnv{
		factories: factories,
		config: providerBlock() + fmt.Sprintf(`
resource "everpure_azure_volume" "test" {
  name                    = "vol1"
  volume_group_id         = %[1]q
  provisioned_size_in_gib = 5
}
`, testVolumeGroupID("vg1")),
		addr:         "everpure_azure_volume.test",
		kind:         kindVolume,
		name:         "vol1",
		resourceType: "everpure_azure_volume",
		configuredID: testVolumeSourceID("vg1", "vol1"),
		seed: func(state, seededID string) {
			store.seedVolume(testResourceGroup, testStoragePoolName, "vg1", "vol1", 1<<30)
			store.mu.Lock()
			defer store.mu.Unlock()
			vol := store.volumes[volumeKey(testResourceGroup, testStoragePoolName, "vg1", "vol1")]
			vol.Properties.ProvisioningState = provisioningStatePtr[armpurestorageblock.ProvisioningState](state)
			if seededID != "" {
				vol.ID = to.Ptr(seededID)
			}
		},
		failGets: func() {
			store.mu.Lock()
			defer store.mu.Unlock()
			store.getFailStatus = 500
		},
		creates: func() int {
			store.mu.Lock()
			defer store.mu.Unlock()
			return store.createCallCount
		},
	}
}

func existingSnapshotEnv() existingObjectEnv {
	factories, store := newTestSnapshotProviderFactories()
	return existingObjectEnv{
		factories:    factories,
		config:       snapshotConfig("snap1", testVolumeGroupID("vg1"), ""),
		addr:         "everpure_azure_snapshot.test",
		kind:         kindSnapshot,
		name:         "snap1",
		resourceType: "everpure_azure_snapshot",
		configuredID: testVolumeGroupSnapshotID("vg1", "snap1"),
		seed: func(state, seededID string) {
			store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap1", time.Now().UTC(), false)
			store.mu.Lock()
			defer store.mu.Unlock()
			snap := store.snapshots[snapshotKey(testResourceGroup, testStoragePoolName, "vg1", "snap1")]
			snap.Properties.ProvisioningState = provisioningStatePtr[armpurestorageblock.ProvisioningState](state)
			if seededID != "" {
				snap.ID = to.Ptr(seededID)
			}
		},
		failGets: func() {
			store.mu.Lock()
			defer store.mu.Unlock()
			store.getFailStatus = 500
		},
		creates: func() int {
			store.mu.Lock()
			defer store.mu.Unlock()
			return store.createCallCount
		},
	}
}

func existingReservationEnv() existingObjectEnv {
	factories, store := newReservationTestProviderFactories()
	return existingObjectEnv{
		factories:    factories,
		config:       reservationConfig("psc_capacity_perf_ondemand", reservationUserBlock),
		addr:         reservationAddr,
		kind:         kindReservation,
		name:         "res1",
		resourceType: "everpure_azure_reservation",
		configuredID: reservationAPIID(),
		seed: func(state, seededID string) {
			store.seed(testResourceGroup, "res1", armpurestorageblock.Reservation{
				Location: to.Ptr("eastus2"),
				Properties: &armpurestorageblock.ReservationPropertiesBaseResourceProperties{
					ProvisioningState: provisioningStatePtr[armpurestorageblock.ProvisioningState](state),
				},
			})
			if seededID != "" {
				store.mutate(testResourceGroup, "res1", func(res *armpurestorageblock.Reservation) {
					res.ID = to.Ptr(seededID)
				})
			}
		},
		failGets: func() {
			store.mu.Lock()
			defer store.mu.Unlock()
			store.getFailure[reservationKey(testResourceGroup, "res1")] = fakeFailure{status: 500, code: "InternalServerError"}
		},
		creates: func() int {
			store.mu.Lock()
			defer store.mu.Unlock()
			return len(store.putBodies)
		},
	}
}

// existingObjectEnvs are the resources whose Create refuses an object that exists.
var existingObjectEnvs = map[string]func() existingObjectEnv{
	"storage pool": existingPoolEnv,
	"volume group": existingVolumeGroupEnv,
	"volume":       existingVolumeEnv,
	"snapshot":     existingSnapshotEnv,
	"reservation":  existingReservationEnv,
}

func TestCreateRefusesAnExistingObject(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	for name, newEnv := range existingObjectEnvs {
		t.Run(name, func(t *testing.T) {
			env := newEnv()
			// Azure may spell the ID in another case than the configuration does; the
			// message must carry its spelling, which is the one terraform import needs.
			returnedID := strings.ToUpper(env.configuredID)
			op := operation{action: actionCreate, kind: env.kind, name: env.name}

			mustRefuse := func(state, seededID, wantID string) resource.TestStep {
				return resource.TestStep{
					PreConfig: func() { env.seed(state, seededID) },
					Config:    env.config,
					ConfigPlanChecks: resource.ConfigPlanChecks{
						// A refused create leaves nothing in state, so the next plan
						// still creates instead of replacing a tainted object.
						PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(env.addr, plancheck.ResourceActionCreate)},
					},
					ExpectError: causeRegexp(existingObjectCause(env.kind, env.resourceType, wantID, state)),
				}
			}

			steps := []resource.TestStep{
				mustRefuse("Succeeded", "", env.configuredID),
				mustRefuse("", returnedID, returnedID),
				mustRefuse("Accepted", "", env.configuredID),
				mustRefuse("Deleting", "", env.configuredID),
				mustRefuse("Failed", "", env.configuredID),
				mustRefuse("Canceled", "", env.configuredID),
				mustRefuse("SomethingNew", "", env.configuredID),
				{
					PreConfig:   env.failGets,
					Config:      env.config,
					ExpectError: summaryRegexp(op.fail(detailError{})),
				},
			}

			resource.Test(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: env.factories,
				Steps:                    steps,
			})

			if got := env.creates(); got != 0 {
				t.Errorf("got %d creates, want 0: the object that exists must never be sent a PUT", got)
			}
		})
	}
}

// TestCreateBeforeDestroyReplacementDoesNotAdoptTheOldObject covers a replacement under an
// unchanged name: the new object's create finds the old one still there and must fail
// instead of upserting it, since the old one is deleted right after.
func TestCreateBeforeDestroyReplacementDoesNotAdoptTheOldObject(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	factories, store := newTestProviderFactories()
	config := func(location string) string {
		return providerBlock() + fmt.Sprintf(`
resource "everpure_azure_storage_pool" "test" {
  name                          = "pool1"
  resource_group_name           = %[1]q
  location                      = %[2]q
  zone                          = "1"
  reservation_id                = %[3]q
  provisioned_bandwidth_in_mbps = 1024

  vnet_injection {
    subnet_id = %[4]q
  }

  lifecycle {
    create_before_destroy = true
  }
}
`, testResourceGroup, location, testReservationID(), testSubnetID("snet1"))
	}
	id := store.fakePoolID(testResourceGroup, "pool1")

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: config("eastus2"),
				Check:  resource.TestCheckResourceAttr("everpure_azure_storage_pool.test", "id", id),
			},
			{
				Config:      config("westus2"),
				ExpectError: causeRegexp(newAlreadyExistsError(kindStoragePool, id, "everpure_azure_storage_pool")),
			},
		},
	})

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.createCallCount != 1 {
		t.Errorf("got %d creates, want 1: the replacement must not have sent a PUT", store.createCallCount)
	}
}
