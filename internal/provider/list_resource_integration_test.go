// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	tfjson "github.com/hashicorp/terraform-json"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/querycheck"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
)

// Every list resource test drives `terraform query` through terraform-plugin-testing's
// Query steps against the fake ARM stores. The list resources' configuration has no
// blocks, so the dynamic-block-unknown-list regression covered elsewhere in this
// package has nothing to apply to here; the round-trip tests in
// list_resource_roundtrip_test.go cover it on the resources' own configs.

// queryResults runs fn over the results of one list block, which it finds by address
// ("list.<type>.<name>"). It is the building block for assertions the stock querycheck
// checks can't express, such as result order.
type queryResults struct {
	address string
	fn      func(results []tfjson.ListResourceFoundData) error
}

func (q queryResults) CheckQuery(_ context.Context, req querycheck.CheckQueryRequest, resp *querycheck.CheckQueryResponse) {
	var found []tfjson.ListResourceFoundData
	for _, r := range req.Query {
		if r.Address == q.address {
			found = append(found, r)
		}
	}
	resp.Error = q.fn(found)
}

// expectDisplayNames asserts the list block's results are exactly names, in order.
func expectDisplayNames(address string, names ...string) querycheck.QueryResultCheck {
	return queryResults{address: address, fn: func(results []tfjson.ListResourceFoundData) error {
		var got []string
		for _, r := range results {
			got = append(got, r.DisplayName)
		}
		if fmt.Sprint(got) != fmt.Sprint(names) {
			return fmt.Errorf("display names = %v, want %v", got, names)
		}
		return nil
	}}
}

// queryStep is a Query step for one list block named "test" of the given type. body is
// the list block's contents, verbatim (attributes, limit, config block).
func queryStep(listType, body string, checks ...querycheck.QueryResultCheck) resource.TestStep {
	return resource.TestStep{
		Query: true,
		Config: fmt.Sprintf(`
provider "everpure" {
  subscription_id = %q
}

list %q "test" {
  provider = everpure
%s
}
`, testSubscriptionID, listType, body),
		QueryResultChecks: checks,
	}
}

// queryErrorStep is queryStep for a query expected to fail.
func queryErrorStep(listType, body string, expect *regexp.Regexp) resource.TestStep {
	step := queryStep(listType, body)
	step.ExpectError = expect
	return step
}

func queryTestCase(factories map[string]func() (tfprotov6.ProviderServer, error), steps ...resource.TestStep) resource.TestCase {
	return resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		TerraformVersionChecks:   []tfversion.TerraformVersionCheck{tfversion.SkipBelow(tfversion.Version1_14_0)},
		Steps:                    steps,
	}
}

func listBlockConfig(attributes, config string) string {
	return fmt.Sprintf("%s\n  config {\n%s\n  }\n", attributes, config)
}

// seedListPool adds a fully populated pool directly to the store.
func seedListPool(store *fakeStoragePoolStore, resourceGroupName, name string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.pools[poolKey(resourceGroupName, name)] = &armpurestorageblock.StoragePool{
		ID:       to.Ptr(store.fakePoolID(resourceGroupName, name)),
		Name:     to.Ptr(name),
		Location: to.Ptr("eastus2"),
		Properties: &armpurestorageblock.StoragePoolProperties{
			AvailabilityZone:             to.Ptr("1"),
			ProvisionedBandwidthMbPerSec: to.Ptr(int64(1024)),
			ProvisionedIops:              to.Ptr(int64(10240)),
			DataRetentionPeriod:          to.Ptr(int64(3600)),
			ReservationResourceID:        to.Ptr(testReservationID()),
			StoragePoolInternalID:        to.Ptr("internal-" + name),
			VnetInjection: &armpurestorageblock.VnetInjection{
				SubnetID: to.Ptr(testSubnetID("snet1")),
				VnetID:   to.Ptr(testVnetID()),
			},
		},
	}
}

func TestStoragePoolListResource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	const address = "list.everpure_azure_storage_pool.test"
	const listType = "everpure_azure_storage_pool"

	seedThree := func(store *fakeStoragePoolStore) {
		seedListPool(store, "rg-b", "pool-a")
		seedListPool(store, "rg-a", "pool-z")
		seedListPool(store, "rg-a", "pool-m")
	}

	t.Run("a resource group scope lists only that group's pools, sorted by id", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		seedThree(store)
		store.mu.Lock()
		store.pageSize = 1
		store.mu.Unlock()

		resource.Test(t, queryTestCase(factories, queryStep(listType,
			listBlockConfig("", `    resource_group_name = "rg-a"`),
			querycheck.ExpectLength("everpure_azure_storage_pool.test", 2),
			expectDisplayNames(address, "pool-m", "pool-z"),
		)))
	})

	t.Run("no resource group lists the whole subscription, sorted by id across groups and pages", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		seedThree(store)
		store.mu.Lock()
		store.pageSize = 1
		store.mu.Unlock()

		resource.Test(t, queryTestCase(factories, queryStep(listType, listBlockConfig("", ""),
			querycheck.ExpectLength("everpure_azure_storage_pool.test", 3),
			expectDisplayNames(address, "pool-m", "pool-z", "pool-a"),
			queryResults{address: address, fn: func(results []tfjson.ListResourceFoundData) error {
				want := map[string]any{
					"subscription_id":     testSubscriptionID,
					"resource_group_name": "rg-a",
					"name":                "pool-m",
				}
				if fmt.Sprint(results[0].Identity) != fmt.Sprint(want) {
					return fmt.Errorf("first identity = %v, want %v", results[0].Identity, want)
				}
				if results[0].ResourceObject != nil {
					return fmt.Errorf("resource object present without include_resource: %v", results[0].ResourceObject)
				}
				return nil
			}},
		)))
	})

	t.Run("an empty scope gives zero results and no error", func(t *testing.T) {
		factories, _ := newTestProviderFactories()

		resource.Test(t, queryTestCase(factories,
			queryStep(listType, listBlockConfig("", ""), querycheck.ExpectLength("everpure_azure_storage_pool.test", 0)),
			queryStep(listType, listBlockConfig("", `    resource_group_name = "rg-none"`), querycheck.ExpectLength("everpure_azure_storage_pool.test", 0)),
		))
	})

	t.Run("include_resource returns the resource object as import and Read would leave it", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		seedListPool(store, "rg-a", "pool-m")
		store.mutatePool("rg-a", "pool-m", func(p *armpurestorageblock.StoragePool) {
			p.Tags = map[string]*string{"team": to.Ptr("storage")}
		})

		resource.Test(t, queryTestCase(factories, queryStep(listType,
			"  include_resource = true\n"+listBlockConfig("", `    resource_group_name = "rg-a"`),
			queryResults{address: address, fn: func(results []tfjson.ListResourceFoundData) error {
				if len(results) != 1 {
					return fmt.Errorf("got %d results, want 1", len(results))
				}
				got := results[0].ResourceObject
				wantID := store.fakePoolID("rg-a", "pool-m")
				if got["id"] != wantID || got["name"] != "pool-m" || got["resource_group_name"] != "rg-a" ||
					got["location"] != "eastus2" || fmt.Sprint(got["provisioned_bandwidth_in_mbps"]) != "1024" {
					return fmt.Errorf("unexpected resource object: %v", got)
				}
				if tags, _ := got["tags"].(map[string]any); fmt.Sprint(tags) != "map[team:storage]" {
					return fmt.Errorf("tags = %v", got["tags"])
				}
				return nil
			}},
		)))
	})

	t.Run("limit caps the results after sorting", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		seedThree(store)

		resource.Test(t, queryTestCase(factories, queryStep(listType,
			"  limit = 2\n"+listBlockConfig("", `    resource_group_name = "rg-a"`),
			expectDisplayNames(address, "pool-m", "pool-z"),
		), queryStep(listType,
			"  limit = 2\n"+listBlockConfig("", ""),
			expectDisplayNames(address, "pool-m", "pool-z"),
		)))
	})

	t.Run("a pool with no properties fails the query", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		seedThree(store)
		store.mutatePool("rg-a", "pool-z", func(p *armpurestorageblock.StoragePool) { p.Properties = nil })

		resource.Test(t, queryTestCase(factories, queryErrorStep(listType, listBlockConfig("", ""),
			diagRegexp(unexpectedResponseDiagnostic(newNoPropertiesError(kindStoragePool))))))
	})

	t.Run("a pool with no id fails the query", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		seedThree(store)
		store.mutatePool("rg-a", "pool-z", func(p *armpurestorageblock.StoragePool) { p.ID = nil })

		resource.Test(t, queryTestCase(factories, queryErrorStep(listType, listBlockConfig("", ""),
			diagRegexp(unexpectedResponseDiagnostic(newMissingResponseIDError(kindStoragePool))))))
	})

	t.Run("a timeouts block is accepted", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		seedThree(store)

		resource.Test(t, queryTestCase(factories, queryStep(listType,
			listBlockConfig("", "    timeouts {\n      list = \"2m\"\n    }"),
			querycheck.ExpectLength("everpure_azure_storage_pool.test", 3),
		)))
	})

	t.Run("an unparseable timeout is rejected", func(t *testing.T) {
		factories, _ := newTestProviderFactories()

		resource.Test(t, queryTestCase(factories, queryErrorStep(listType,
			listBlockConfig("", "    timeouts {\n      list = \"soon\"\n    }"),
			regexp.MustCompile(`(?s)Invalid Attribute Value.*"soon"`))))
	})
}

func TestVolumeGroupListResource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	const address = "list.everpure_azure_volume_group.test"
	const listType = "everpure_azure_volume_group"
	poolConfig := fmt.Sprintf("    storage_pool_id = %q", testStoragePoolIDForVG())

	t.Run("lists the pool's volume groups sorted by id, over a multi-page response", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg-c", "PT1H", "P7D")
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg-a", "PT1H", "P7D")
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg-b", "PT1H", "P7D")
		// A group on another pool must not be listed.
		vgStore.seedVolumeGroup(testResourceGroup, "pool2", "vg-other", "PT1H", "P7D")
		vgStore.mu.Lock()
		vgStore.pageSize = 1
		vgStore.mu.Unlock()

		resource.Test(t, queryTestCase(factories, queryStep(listType, listBlockConfig("", poolConfig),
			querycheck.ExpectLength("everpure_azure_volume_group.test", 3),
			expectDisplayNames(address, "vg-a", "vg-b", "vg-c"),
			queryResults{address: address, fn: func(results []tfjson.ListResourceFoundData) error {
				want := map[string]any{
					"subscription_id":     testSubscriptionID,
					"resource_group_name": testResourceGroup,
					"storage_pool_name":   testStoragePoolName,
					"name":                "vg-a",
				}
				if fmt.Sprint(results[0].Identity) != fmt.Sprint(want) {
					return fmt.Errorf("first identity = %v, want %v", results[0].Identity, want)
				}
				return nil
			}},
		)))
	})

	t.Run("a pool with no volume groups gives zero results", func(t *testing.T) {
		factories, _, _ := newTestVolumeGroupProviderFactories()

		resource.Test(t, queryTestCase(factories, queryStep(listType, listBlockConfig("", poolConfig),
			querycheck.ExpectLength("everpure_azure_volume_group.test", 0))))
	})

	t.Run("a missing storage pool is an error", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()
		vgStore.markStoragePoolMissing(testResourceGroup, testStoragePoolName)

		resource.Test(t, queryTestCase(factories, queryErrorStep(listType, listBlockConfig("", poolConfig),
			diagRegexp((operation{action: actionList, kind: kindVolumeGroups, unnamed: true}).fail(newNotFoundError(kindStoragePool, testStoragePoolIDForVG(), nil, apiFailure{}))))))
	})

	t.Run("a storage pool in another subscription is an error", func(t *testing.T) {
		factories, _, _ := newTestVolumeGroupProviderFactories()
		other := strings.Replace(testStoragePoolIDForVG(), testSubscriptionID, "22222222-2222-2222-2222-222222222222", 1)

		resource.Test(t, queryTestCase(factories, queryErrorStep(listType,
			listBlockConfig("", fmt.Sprintf("    storage_pool_id = %q", other)),
			subscriptionMismatchRegexp(kindStoragePool, other))))
	})

	t.Run("include_resource returns the resource object as import and Read would leave it", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg-a", "PT1H", "P7D")

		resource.Test(t, queryTestCase(factories, queryStep(listType,
			"  include_resource = true\n"+listBlockConfig("", poolConfig),
			queryResults{address: address, fn: func(results []tfjson.ListResourceFoundData) error {
				if len(results) != 1 {
					return fmt.Errorf("got %d results, want 1", len(results))
				}
				got := results[0].ResourceObject
				if got["name"] != "vg-a" || got["storage_pool_id"] != testStoragePoolIDForVG() ||
					got["source_type"] != "none" || got["deletion_protection"] != true ||
					got["eradicate_on_destroy"] != false {
					return fmt.Errorf("unexpected resource object: %v", got)
				}
				policy, _ := got["snapshot_protection_policy"].([]any)
				if fmt.Sprint(policy) != "[map[frequency:PT1H retention:P7D]]" {
					return fmt.Errorf("snapshot_protection_policy = %v", got["snapshot_protection_policy"])
				}
				return nil
			}},
		)))
	})

	t.Run("limit caps the results after sorting", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()
		for _, name := range []string{"vg-c", "vg-a", "vg-b"} {
			vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, name, "PT1H", "P7D")
		}

		resource.Test(t, queryTestCase(factories, queryStep(listType,
			"  limit = 2\n"+listBlockConfig("", poolConfig),
			expectDisplayNames(address, "vg-a", "vg-b"),
		)))
	})

	t.Run("a volume group with no properties fails the query", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg-a", "PT1H", "P7D")
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg-b", "PT1H", "P7D")
		vgStore.mutateVolumeGroup(testResourceGroup, testStoragePoolName, "vg-b", func(vg *armpurestorageblock.VolumeGroup) {
			vg.Properties = nil
		})

		resource.Test(t, queryTestCase(factories, queryErrorStep(listType, listBlockConfig("", poolConfig),
			diagRegexp(unexpectedResponseDiagnostic(newNoPropertiesError(kindVolumeGroup))))))
	})

	t.Run("a volume group with no id fails the query", func(t *testing.T) {
		factories, _, vgStore := newTestVolumeGroupProviderFactories()
		vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg-a", "PT1H", "P7D")
		vgStore.mutateVolumeGroup(testResourceGroup, testStoragePoolName, "vg-a", func(vg *armpurestorageblock.VolumeGroup) {
			vg.ID = nil
		})

		resource.Test(t, queryTestCase(factories, queryErrorStep(listType, listBlockConfig("", poolConfig),
			diagRegexp(unexpectedResponseDiagnostic(newMissingResponseIDError(kindVolumeGroup))))))
	})
}

func TestVolumeListResource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	const address = "list.everpure_azure_volume.test"
	const listType = "everpure_azure_volume"
	groupConfig := fmt.Sprintf("    volume_group_id = %q", testVolumeGroupID("vg1"))

	t.Run("lists the volume group's live volumes sorted by id, over a multi-page response", func(t *testing.T) {
		factories, store := newTestVolumeProviderFactories()
		store.seedVolume(testResourceGroup, testStoragePoolName, "vg1", "vol-c", 1073741824)
		store.seedVolume(testResourceGroup, testStoragePoolName, "vg1", "vol-a", 1073741824)
		store.seedVolume(testResourceGroup, testStoragePoolName, "vg1", "vol-b", 1073741824)
		// A soft-deleted copy and a volume in another group must not be listed.
		store.seedDestroyedVolume(testResourceGroup, testStoragePoolName, "vg1", "vol-a-20260901000000", "vol-a",
			time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
		store.seedVolume(testResourceGroup, testStoragePoolName, "vg2", "vol-other", 1073741824)
		store.mu.Lock()
		store.pageSize = 1
		store.mu.Unlock()

		resource.Test(t, queryTestCase(factories, queryStep(listType, listBlockConfig("", groupConfig),
			querycheck.ExpectLength("everpure_azure_volume.test", 3),
			expectDisplayNames(address, "vol-a", "vol-b", "vol-c"),
			queryResults{address: address, fn: func(results []tfjson.ListResourceFoundData) error {
				want := map[string]any{
					"subscription_id":     testSubscriptionID,
					"resource_group_name": testResourceGroup,
					"storage_pool_name":   testStoragePoolName,
					"volume_group_name":   "vg1",
					"name":                "vol-a",
				}
				if fmt.Sprint(results[0].Identity) != fmt.Sprint(want) {
					return fmt.Errorf("first identity = %v, want %v", results[0].Identity, want)
				}
				return nil
			}},
		)))
	})

	t.Run("a volume group with only soft-deleted volumes gives zero results", func(t *testing.T) {
		factories, store := newTestVolumeProviderFactories()
		store.seedDestroyedVolume(testResourceGroup, testStoragePoolName, "vg1", "vol-a-20260901000000", "vol-a",
			time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))

		resource.Test(t, queryTestCase(factories, queryStep(listType, listBlockConfig("", groupConfig),
			querycheck.ExpectLength("everpure_azure_volume.test", 0))))
	})

	t.Run("an empty volume group gives zero results", func(t *testing.T) {
		factories, _ := newTestVolumeProviderFactories()

		resource.Test(t, queryTestCase(factories, queryStep(listType, listBlockConfig("", groupConfig),
			querycheck.ExpectLength("everpure_azure_volume.test", 0))))
	})

	t.Run("a missing volume group is an error", func(t *testing.T) {
		factories, store := newTestVolumeProviderFactories()
		store.markVolumeGroupMissing(testResourceGroup, testStoragePoolName, "vg1")

		resource.Test(t, queryTestCase(factories, queryErrorStep(listType, listBlockConfig("", groupConfig),
			diagRegexp((operation{action: actionList, kind: kindVolumes, unnamed: true}).fail(newNotFoundError(kindVolumeGroup, testVolumeGroupID("vg1"), nil, apiFailure{}))))))
	})

	t.Run("a volume group in another subscription is an error", func(t *testing.T) {
		factories, _ := newTestVolumeProviderFactories()
		other := strings.Replace(testVolumeGroupID("vg1"), testSubscriptionID, "22222222-2222-2222-2222-222222222222", 1)

		resource.Test(t, queryTestCase(factories, queryErrorStep(listType,
			listBlockConfig("", fmt.Sprintf("    volume_group_id = %q", other)),
			subscriptionMismatchRegexp(kindVolumeGroup, other))))
	})

	t.Run("include_resource returns the resource object as import and Read would leave it", func(t *testing.T) {
		factories, store := newTestVolumeProviderFactories()
		store.seedVolume(testResourceGroup, testStoragePoolName, "vg1", "vol-a", 2*bytesPerGiB)

		resource.Test(t, queryTestCase(factories, queryStep(listType,
			"  include_resource = true\n"+listBlockConfig("", groupConfig),
			queryResults{address: address, fn: func(results []tfjson.ListResourceFoundData) error {
				if len(results) != 1 {
					return fmt.Errorf("got %d results, want 1", len(results))
				}
				got := results[0].ResourceObject
				if got["name"] != "vol-a" || got["volume_group_id"] != testVolumeGroupID("vg1") ||
					fmt.Sprint(got["provisioned_size_in_bytes"]) != strconv.Itoa(2*bytesPerGiB) || got["source_type"] != "none" ||
					got["deletion_protection"] != true || got["eradicate_on_destroy"] != false ||
					got["provisioned_size_in_gib"] != nil {
					return fmt.Errorf("unexpected resource object: %v", got)
				}
				return nil
			}},
		)))
	})

	t.Run("limit caps the results after sorting, and soft-deleted volumes do not count toward it", func(t *testing.T) {
		factories, store := newTestVolumeProviderFactories()
		for _, name := range []string{"vol-c", "vol-a", "vol-b"} {
			store.seedVolume(testResourceGroup, testStoragePoolName, "vg1", name, bytesPerGiB)
		}
		// Sorts before every live volume ("vol-a-2..." < "vol-b"), so a limit applied
		// before filtering would return it in place of vol-b.
		store.seedDestroyedVolume(testResourceGroup, testStoragePoolName, "vg1", "vol-a-20260901000000", "vol-a",
			time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))

		resource.Test(t, queryTestCase(factories, queryStep(listType,
			"  limit = 2\n"+listBlockConfig("", groupConfig),
			expectDisplayNames(address, "vol-a", "vol-b"),
		)))
	})

	t.Run("an unset limit is Terraform's default of 100, and a higher limit lifts it", func(t *testing.T) {
		factories, store := newTestVolumeProviderFactories()
		for i := range 101 {
			store.seedVolume(testResourceGroup, testStoragePoolName, "vg1", fmt.Sprintf("vol-%03d", i), bytesPerGiB)
		}

		last := func(name string) querycheck.QueryResultCheck {
			return queryResults{address: address, fn: func(results []tfjson.ListResourceFoundData) error {
				if got := results[len(results)-1].DisplayName; got != name {
					return fmt.Errorf("last result = %s, want %s", got, name)
				}
				return nil
			}}
		}
		resource.Test(t, queryTestCase(factories,
			queryStep(listType, listBlockConfig("", groupConfig),
				querycheck.ExpectLength("everpure_azure_volume.test", 100), last("vol-099")),
			queryStep(listType, "  limit = 101\n"+listBlockConfig("", groupConfig),
				querycheck.ExpectLength("everpure_azure_volume.test", 101), last("vol-100")),
		))
	})

	t.Run("a volume with no properties fails the query", func(t *testing.T) {
		factories, store := newTestVolumeProviderFactories()
		store.seedVolume(testResourceGroup, testStoragePoolName, "vg1", "vol-a", bytesPerGiB)
		store.mutateVolume(testResourceGroup, testStoragePoolName, "vg1", "vol-a", func(v *armpurestorageblock.Volume) {
			v.Properties = nil
		})

		resource.Test(t, queryTestCase(factories, queryErrorStep(listType, listBlockConfig("", groupConfig),
			diagRegexp(unexpectedResponseDiagnostic(newNoPropertiesError(kindVolume))))))
	})

	t.Run("a volume with no id fails the query", func(t *testing.T) {
		factories, store := newTestVolumeProviderFactories()
		store.seedVolume(testResourceGroup, testStoragePoolName, "vg1", "vol-a", bytesPerGiB)
		store.mutateVolume(testResourceGroup, testStoragePoolName, "vg1", "vol-a", func(v *armpurestorageblock.Volume) {
			v.ID = nil
		})

		resource.Test(t, queryTestCase(factories, queryErrorStep(listType, listBlockConfig("", groupConfig),
			diagRegexp(unexpectedResponseDiagnostic(newMissingResponseIDError(kindVolume))))))
	})
}

func TestSnapshotListResource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	const address = "list.everpure_azure_snapshot.test"
	const listType = "everpure_azure_snapshot"
	groupConfig := fmt.Sprintf("    volume_group_id = %q", testVolumeGroupID("vg1"))
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	seed := func(store *fakeSnapshotStore, vg, name string) {
		store.seedSnapshot(testResourceGroup, testStoragePoolName, vg, name, at, false)
	}

	t.Run("lists the volume group's live user snapshots", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		for _, name := range []string{"snap-c", "snap-a", "snap-b"} {
			seed(store, "vg1", name)
		}
		seed(store, "vg2", "snap-other")

		resource.Test(t, queryTestCase(factories, queryStep(listType, listBlockConfig("", groupConfig),
			querycheck.ExpectLength("everpure_azure_snapshot.test", 3),
			expectDisplayNames(address, "snap-a", "snap-b", "snap-c"),
			queryResults{address: address, fn: func(results []tfjson.ListResourceFoundData) error {
				want := map[string]any{
					"subscription_id":     testSubscriptionID,
					"resource_group_name": testResourceGroup,
					"storage_pool_name":   testStoragePoolName,
					"volume_group_name":   "vg1",
					"name":                "snap-a",
				}
				if fmt.Sprint(results[0].Identity) != fmt.Sprint(want) {
					return fmt.Errorf("first identity = %v, want %v", results[0].Identity, want)
				}
				return nil
			}},
		)))
		if store.getCallCount != 0 {
			t.Errorf("made %d GETs without include_resource, want none", store.getCallCount)
		}
	})

	t.Run("destroyed and schedule-created snapshots are not listed", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		seed(store, "vg1", "snap-b")
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "20260901000000", at, true)
		store.seedDestroyedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap-a-20260901000000", "snap-a", at, false)
		store.seedDestroyedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "20260801000000-20260901000000", "20260801000000", at, true)

		resource.Test(t, queryTestCase(factories, queryStep(listType, listBlockConfig("", groupConfig),
			querycheck.ExpectLength("everpure_azure_snapshot.test", 1),
			expectDisplayNames(address, "snap-b"),
		)))
	})

	t.Run("a volume group with only excluded snapshots gives zero results", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "20260901000000", at, true)
		store.seedDestroyedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap-a-20260901000000", "snap-a", at, false)

		resource.Test(t, queryTestCase(factories, queryStep(listType, listBlockConfig("", groupConfig),
			querycheck.ExpectLength("everpure_azure_snapshot.test", 0))))
	})

	t.Run("an empty volume group gives zero results", func(t *testing.T) {
		factories, _ := newTestSnapshotProviderFactories()

		resource.Test(t, queryTestCase(factories, queryStep(listType, listBlockConfig("", groupConfig),
			querycheck.ExpectLength("everpure_azure_snapshot.test", 0))))
	})

	t.Run("a missing volume group is an error", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		store.markVolumeGroupMissing(testResourceGroup, testStoragePoolName, "vg1")

		resource.Test(t, queryTestCase(factories, queryErrorStep(listType, listBlockConfig("", groupConfig),
			diagRegexp((operation{action: actionList, kind: kindSnapshots, unnamed: true}).fail(newNotFoundError(kindVolumeGroup, testVolumeGroupID("vg1"), nil, apiFailure{}))))))
	})

	t.Run("a volume group in another subscription is an error", func(t *testing.T) {
		factories, _ := newTestSnapshotProviderFactories()
		other := strings.Replace(testVolumeGroupID("vg1"), testSubscriptionID, "22222222-2222-2222-2222-222222222222", 1)

		resource.Test(t, queryTestCase(factories, queryErrorStep(listType,
			listBlockConfig("", fmt.Sprintf("    volume_group_id = %q", other)),
			subscriptionMismatchRegexp(kindVolumeGroup, other))))
	})

	t.Run("include_resource gives volume_snapshots as null and makes no GET", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		seed(store, "vg1", "snap-a")
		seed(store, "vg1", "snap-b")
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "20260901000000", at, true)

		resource.Test(t, queryTestCase(factories, queryStep(listType,
			"  include_resource = true\n"+listBlockConfig("", groupConfig),
			queryResults{address: address, fn: func(results []tfjson.ListResourceFoundData) error {
				if len(results) != 2 {
					return fmt.Errorf("got %d results, want 2", len(results))
				}
				got := results[0].ResourceObject
				if got["name"] != "snap-a" || got["volume_group_id"] != testVolumeGroupID("vg1") ||
					got["deletion_protection"] != true || got["eradicate_on_destroy"] != false ||
					got["source_recoverable_snapshot_id"] != nil || got["created_at"] != at.Format(time.RFC3339) {
					return fmt.Errorf("unexpected resource object: %v", got)
				}
				// Null, not an empty list: the list response carries no member volumes.
				if members, present := got["volume_snapshots"]; present && members != nil {
					return fmt.Errorf("volume_snapshots = %v, want null", members)
				}
				return nil
			}},
		)))
		if store.getCallCount != 0 {
			t.Errorf("made %d GETs with include_resource, want none", store.getCallCount)
		}
	})

	t.Run("limit caps the results after sorting and filtering", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		for _, name := range []string{"snap-c", "snap-a", "snap-b"} {
			seed(store, "vg1", name)
		}
		// Sorts before every live snapshot, so a limit applied before filtering would
		// return it in place of snap-b.
		store.seedDestroyedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap-a-20260901000000", "snap-a", at, false)

		resource.Test(t, queryTestCase(factories, queryStep(listType,
			"  limit = 2\n  include_resource = true\n"+listBlockConfig("", groupConfig),
			expectDisplayNames(address, "snap-a", "snap-b"),
		)))
	})

	t.Run("results are newest first", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap-old", at.Add(-time.Hour), false)
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap-b", at, false)
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap-a", at, false)
		store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap-new", at.Add(time.Hour), false)

		resource.Test(t, queryTestCase(factories, queryStep(listType, listBlockConfig("", groupConfig),
			expectDisplayNames(address, "snap-new", "snap-a", "snap-b", "snap-old"),
		)))
	})

	t.Run("limit is sent as $top with a filter and an order, and paging stops once it is reached", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		for _, name := range []string{"snap-d", "snap-c", "snap-a", "snap-b"} {
			seed(store, "vg1", name)
		}
		// Filtered out by the API, so it takes no place on the first page.
		store.seedDestroyedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap-a-20260901000000", "snap-a", at, false)

		resource.Test(t, queryTestCase(factories, queryStep(listType,
			"  limit = 2\n"+listBlockConfig("", groupConfig),
			expectDisplayNames(address, "snap-a", "snap-b"),
		)))
		opts := store.lastListOptions
		if opts == nil || derefString(opts.Filter) != manualSnapshotsFilter || derefString(opts.Orderby) != snapshotsOrderBy ||
			opts.Top == nil || *opts.Top != 2 || opts.Skip == nil || *opts.Skip != 0 {
			t.Errorf("list options = %+v, want the manual-snapshots filter, $orderby=createdAt desc, $top=2 and $skip=0", opts)
		}
		if store.listCallCount != 1 {
			t.Errorf("made %d list calls, want 1: the first page already holds the limit", store.listCallCount)
		}
	})

	t.Run("pages with $skip in pages of at most 100 until the limit", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		for i := range 205 {
			seed(store, "vg1", fmt.Sprintf("snap-%03d", i))
		}

		resource.Test(t, queryTestCase(factories, queryStep(listType,
			"  limit = 150\n"+listBlockConfig("", groupConfig),
			querycheck.ExpectLength("everpure_azure_snapshot.test", 150),
		)))
		opts := store.lastListOptions
		if store.listCallCount != 2 || opts == nil || opts.Top == nil || *opts.Top != maxSnapshotListPageSize ||
			opts.Skip == nil || *opts.Skip != maxSnapshotListPageSize {
			t.Errorf("made %d list calls, last options %+v; want 2 calls, the last with $top=100 and $skip=100", store.listCallCount, opts)
		}
	})

	t.Run("a snapshot created between pages is not listed twice, nor uses up the limit", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		for i := range 205 {
			seed(store, "vg1", fmt.Sprintf("snap-%03d", i))
		}
		store.beforeListCall = func(call int) {
			if call == 2 {
				store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap-new", at.Add(time.Hour), false)
			}
		}

		resource.Test(t, queryTestCase(factories, queryStep(listType,
			"  limit = 150\n"+listBlockConfig("", groupConfig),
			queryResults{address: address, fn: func(results []tfjson.ListResourceFoundData) error {
				seen := map[string]bool{}
				for _, r := range results {
					if seen[r.DisplayName] {
						return fmt.Errorf("%s listed twice", r.DisplayName)
					}
					seen[r.DisplayName] = true
				}
				if len(results) != 150 || !seen["snap-149"] {
					return fmt.Errorf("got %d results (snap-149 present: %v), want 150 ending at snap-149", len(results), seen["snap-149"])
				}
				return nil
			}},
		)))
	})

	t.Run("a snapshot with no properties fails the query", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		seed(store, "vg1", "snap-a")
		store.mutateSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap-a", func(s *armpurestorageblock.VolumeGroupSnapshot) {
			s.Properties = nil
		})

		resource.Test(t, queryTestCase(factories, queryErrorStep(listType, listBlockConfig("", groupConfig),
			diagRegexp(unexpectedResponseDiagnostic(newNoPropertiesError(kindSnapshot))))))
	})

	t.Run("a snapshot with no id fails the query", func(t *testing.T) {
		factories, store := newTestSnapshotProviderFactories()
		seed(store, "vg1", "snap-a")
		store.mutateSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap-a", func(s *armpurestorageblock.VolumeGroupSnapshot) {
			s.ID = nil
		})

		resource.Test(t, queryTestCase(factories, queryErrorStep(listType, listBlockConfig("", groupConfig),
			diagRegexp(unexpectedResponseDiagnostic(newMissingResponseIDError(kindSnapshot))))))
	})
}
