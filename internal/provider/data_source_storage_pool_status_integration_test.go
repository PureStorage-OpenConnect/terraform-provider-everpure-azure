// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azcorefake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	psbfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock/fake"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

const testStoragePoolStatusDataSourceAddr = "data.everpure_azure_storage_pool_status.test"

func seededHealthInfo() armpurestorageblock.StoragePoolHealthInfo {
	info := fullHealthInfo()
	info.Health.UsedCapacityPercentage = to.Ptr(92.0)
	return info
}

// floatingHealthNumbers matches the integer values of the bandwidth and IOPS objects
// ("current", "provisioned", "max") in a health response body.
var floatingHealthNumbers = regexp.MustCompile(`"(current|provisioned|max)":(-?[0-9]+)([,}])`)

// floatingHealthTransport rewrites the health response's bandwidth and IOPS integers into
// JSON floats ("provisioned":1073741824 becomes "provisioned":1073741824.0), the format
// the live service uses, which the fake server does not produce. Other requests and
// error responses pass through untouched.
type floatingHealthTransport struct {
	inner policy.Transporter
}

func (f floatingHealthTransport) Do(req *http.Request) (*http.Response, error) {
	resp, err := f.inner.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK || !strings.HasSuffix(req.URL.Path, "/getHealthStatus") {
		return resp, err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	body = floatingHealthNumbers.ReplaceAll(body, []byte(`"$1":$2.0$3`))
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Content-Length")
	return resp, nil
}

// TestStoragePoolStatusDataSource has no dynamic-block case: the data source's schema
// has no blocks, and its one input, storage_pool_id, is a scalar.
var poolStatusOp = operation{action: actionRetrieve, kind: kindStoragePoolStatus, unnamed: true}

func TestStoragePoolStatusDataSource(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	poolStatusConfig := func(poolID string) string {
		return providerBlock() + fmt.Sprintf(`
data "everpure_azure_storage_pool_status" "test" {
  storage_pool_id = %[1]q
}
`, poolID)
	}

	t.Run("happy path: seeded metrics come back, alerts in the seeded order", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		seedTestStoragePool(store)
		store.setHealth(testResourceGroup, testStoragePoolName, seededHealthInfo())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: poolStatusConfig(testStoragePoolIDForVG()),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "storage_pool_id", testStoragePoolIDForVG()),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "used_capacity_percentage", "92"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "data_reduction_ratio", "3.5"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "estimated_max_capacity_in_bytes", "1000"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "bandwidth_usage.current_in_bytes_per_second", "100"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "bandwidth_usage.provisioned_in_bytes_per_second", "1073741824"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "bandwidth_usage.max_in_bytes_per_second", "2147483648"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "iops_usage.current", "7"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "iops_usage.provisioned", "10240"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "iops_usage.max", "20480"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "space.total_used_in_bytes", "10"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "space.unique_in_bytes", "4"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "space.snapshots_in_bytes", "3"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "space.shared_in_bytes", "3"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "alerts.#", "3"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "alerts.0.level", "warning"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "alerts.0.message", "capacity above 80%"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "alerts.1.level", "info"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "alerts.2.level", "error"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "alerts.2.message", "disconnected"),
					),
				},
			},
		})
	})

	t.Run("bandwidth and IOPS arriving as JSON floats are read as integers", func(t *testing.T) {
		factories, store := newTestProviderFactoriesWithTransport(func(inner policy.Transporter) policy.Transporter {
			return floatingHealthTransport{inner: inner}
		})
		seedTestStoragePool(store)
		store.setHealth(testResourceGroup, testStoragePoolName, seededHealthInfo())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: poolStatusConfig(testStoragePoolIDForVG()),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "used_capacity_percentage", "92"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "estimated_max_capacity_in_bytes", "1000"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "bandwidth_usage.current_in_bytes_per_second", "100"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "bandwidth_usage.provisioned_in_bytes_per_second", "1073741824"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "bandwidth_usage.max_in_bytes_per_second", "2147483648"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "iops_usage.current", "7"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "iops_usage.provisioned", "10240"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "iops_usage.max", "20480"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "space.total_used_in_bytes", "10"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "alerts.#", "3"),
					),
				},
			},
		})
	})

	t.Run("an existing pool with nothing seeded reads as idle, with an empty alert list", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		seedTestStoragePool(store)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: poolStatusConfig(testStoragePoolIDForVG()),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "used_capacity_percentage", "0"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "alerts.#", "0"),
					),
				},
			},
		})
	})

	t.Run("chained from a storage pool resource in the same configuration (storage_pool_id unknown at plan)", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		store.setHealth(testResourceGroup, "pool1", seededHealthInfo())

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

data "everpure_azure_storage_pool_status" "test" {
  storage_pool_id = everpure_azure_storage_pool.test.id
}
`, testResourceGroup, testReservationID(), testSubnetID("snet1"))

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "used_capacity_percentage", "92"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "alerts.#", "3"),
					),
				},
			},
		})
	})

	t.Run("scoped inside a check block: a passing assert and a failing assert both leave the run successful", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		seedTestStoragePool(store)
		store.setHealth(testResourceGroup, testStoragePoolName, seededHealthInfo())

		// Both assertions would fail on the seeded health (a warning and an error
		// alert, 92 % used): a failed assert inside a check block is reported as a
		// warning, so the step still succeeds, which is what a check block is for.
		failing := providerBlock() + fmt.Sprintf(`
check "storage_pool_status" {
  data "everpure_azure_storage_pool_status" "test" {
    storage_pool_id = %[1]q
  }

  assert {
    condition = length([
      for a in data.everpure_azure_storage_pool_status.test.alerts : a
      if coalesce(a.level, "") != "info"
    ]) == 0
    error_message = "pool has warning or error alerts"
  }

  assert {
    condition     = data.everpure_azure_storage_pool_status.test.used_capacity_percentage < 75
    error_message = "pool is above 75%% of its maximum capacity"
  }
}
`, testStoragePoolIDForVG())

		passing := providerBlock() + fmt.Sprintf(`
check "storage_pool_status" {
  data "everpure_azure_storage_pool_status" "test" {
    storage_pool_id = %[1]q
  }

  assert {
    condition     = data.everpure_azure_storage_pool_status.test.used_capacity_percentage < 99
    error_message = "pool is above 99%% of its maximum capacity"
  }
}
`, testStoragePoolIDForVG())

		// A check block's data source can't be referenced outside the check, is never
		// stored in state, and plugin-testing can't observe warnings, so the steps
		// above only prove the check is non-fatal. These steps read the same health
		// through an unscoped data source and put the identical conditions in output
		// preconditions, which fail the run: the failing conditions are shown to
		// evaluate false and the passing one true.
		unscoped := func(outputs string) string {
			return providerBlock() + fmt.Sprintf(`
data "everpure_azure_storage_pool_status" "test" {
  storage_pool_id = %[1]q
}
%[2]s`, testStoragePoolIDForVG(), outputs)
		}

		alertsOutput := unscoped(`
output "no_warning_or_error_alerts" {
  value = true
  precondition {
    condition = length([
      for a in data.everpure_azure_storage_pool_status.test.alerts : a
      if coalesce(a.level, "") != "info"
    ]) == 0
    error_message = "pool has warning or error alerts"
  }
}
`)
		capacityOutput := unscoped(`
output "below_75_percent" {
  value = true
  precondition {
    condition     = data.everpure_azure_storage_pool_status.test.used_capacity_percentage < 75
    error_message = "pool is above 75% of its maximum capacity"
  }
}
`)
		passingOutput := unscoped(`
output "below_99_percent" {
  value = true
  precondition {
    condition     = data.everpure_azure_storage_pool_status.test.used_capacity_percentage < 99
    error_message = "pool is above 99% of its maximum capacity"
  }
}
`)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				// A data source scoped to a check block is never stored in state, so
				// every plan re-reads it during apply and shows a pending read.
				{Config: passing, ExpectNonEmptyPlan: true},
				{Config: failing, ExpectNonEmptyPlan: true},
				{
					Config: unscoped(""),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "used_capacity_percentage", "92"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "alerts.0.level", "warning"),
						resource.TestCheckResourceAttr(testStoragePoolStatusDataSourceAddr, "alerts.2.level", "error"),
					),
				},
				{Config: passingOutput},
				{
					Config:      alertsOutput,
					ExpectError: regexp.MustCompile(`pool\s+has\s+warning\s+or\s+error\s+alerts`),
				},
				{
					Config:      capacityOutput,
					ExpectError: regexp.MustCompile(`pool\s+is\s+above\s+75%\s+of\s+its\s+maximum\s+capacity`),
				},
			},
		})
	})

	t.Run("a missing pool inside a check block is downgraded to a warning", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		seedTestStoragePool(store)

		missingID := armids.StoragePoolID{
			ResourceGroupID: armids.ResourceGroupID{SubscriptionID: testSubscriptionID, ResourceGroupName: testResourceGroup},
			StoragePoolName: "does-not-exist",
		}.FullID()

		checked := providerBlock() + fmt.Sprintf(`
check "storage_pool_status" {
  data "everpure_azure_storage_pool_status" "test" {
    storage_pool_id = %[1]q
  }

  assert {
    condition     = length(data.everpure_azure_storage_pool_status.test.alerts) == 0
    error_message = "pool has alerts"
  }
}
`, missingID)

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: checked, ExpectNonEmptyPlan: true},
				// The same read outside a check block is fatal: this is the diagnostic
				// the check block above downgrades to a warning.
				{
					Config:      poolStatusConfig(missingID),
					ExpectError: diagRegexp(operation{action: actionRetrieve, kind: kindStoragePoolStatus, unnamed: true}.fail(newNotFoundError(kindStoragePool, missingID, nil, apiFailure{}))),
				},
			},
		})
	})

	t.Run("a pool without metrics fails the read with the service's alert, but only warns inside a check block", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		seedTestStoragePool(store)
		// A pool whose creation failed or whose backing storage can't be reached comes
		// back 200 with every metric zeroed and one alert saying why. The zeros are not
		// measurements, so the read fails instead of handing them to a threshold check.
		store.setHealth(testResourceGroup, testStoragePoolName, armpurestorageblock.StoragePoolHealthInfo{
			Alerts: []*armpurestorageblock.Alert{
				{Level: to.Ptr(armpurestorageblock.AlertLevelError), Message: to.Ptr("Storage pool creation failed. Please reach out to support.")},
			},
			Health: &armpurestorageblock.HealthDetails{
				BandwidthUsage:         &armpurestorageblock.BandwidthUsage{Current: to.Ptr(int64(0)), Provisioned: to.Ptr(int64(0)), Max: to.Ptr(int64(0))},
				DataReductionRatio:     to.Ptr(0.0),
				EstimatedMaxCapacity:   to.Ptr(int64(0)),
				IopsUsage:              &armpurestorageblock.IopsUsage{Current: to.Ptr(int64(0)), Provisioned: to.Ptr(int64(0)), Max: to.Ptr(int64(0))},
				Space:                  &armpurestorageblock.Space{TotalUsed: to.Ptr(int64(0)), Unique: to.Ptr(int64(0)), Snapshots: to.Ptr(int64(0)), Shared: to.Ptr(int64(0))},
				UsedCapacityPercentage: to.Ptr(0.0),
			},
		})

		checked := providerBlock() + fmt.Sprintf(`
check "storage_pool_status" {
  data "everpure_azure_storage_pool_status" "test" {
    storage_pool_id = %[1]q
  }

  assert {
    condition     = data.everpure_azure_storage_pool_status.test.used_capacity_percentage < 75
    error_message = "pool is above 75%% of its maximum capacity"
  }
}
`, testStoragePoolIDForVG())

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{Config: checked, ExpectNonEmptyPlan: true},
				{
					Config: poolStatusConfig(testStoragePoolIDForVG()),
					ExpectError: regexp.MustCompile(
						`(?s)` + summaryRegexp(poolStatusOp.fail(detailError{})).String() + `.*` +
							wordsRegexp(newStoragePoolNoMetricsError(testStoragePoolIDForVG(), "[error]").Error()) + `.*Storage\s+pool\s+creation\s+failed`),
				},
			},
		})
	})

	t.Run("a response without a health payload fails the read and says no alert explained it", func(t *testing.T) {
		factories, store := newTestProviderFactories()
		seedTestStoragePool(store)
		store.setHealth(testResourceGroup, testStoragePoolName, armpurestorageblock.StoragePoolHealthInfo{})

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config:      poolStatusConfig(testStoragePoolIDForVG()),
					ExpectError: diagRegexp(poolStatusOp.fail(newStoragePoolNoMetricsError(testStoragePoolIDForVG(), ""))),
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
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config:      poolStatusConfig(missingID),
					ExpectError: diagRegexp(operation{action: actionRetrieve, kind: kindStoragePoolStatus, unnamed: true}.fail(newNotFoundError(kindStoragePool, missingID, nil, apiFailure{}))),
				},
			},
		})
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
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config:      poolStatusConfig(otherSubscriptionID),
					ExpectError: subscriptionMismatchRegexp(kindStoragePool, otherSubscriptionID),
				},
			},
		})
	})

	t.Run("errors at validation when storage_pool_id is not a storage pool ID", func(t *testing.T) {
		factories, _ := newTestProviderFactories()

		resource.Test(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{
				{
					Config:      poolStatusConfig("not-an-id"),
					ExpectError: regexp.MustCompile(`(?s)must be the full Azure Resource Manager ID of a\s+storage\s+pool`),
				},
			},
		})
	})
}

// TestGetStoragePoolHealth drives the wrapper against the fake through a client, without
// terraform, so it can also prove the float rewriting really reaches the SDK (the plain
// SDK call fails on it) and that real error statuses are not mistaken for decode errors.
func TestGetStoragePoolHealth(t *testing.T) {
	newClient := func(t *testing.T, store *fakeStoragePoolStore, floats bool) *armpurestorageblock.StoragePoolsClient {
		t.Helper()
		srv := store.newStoragePoolsServer()
		var transport policy.Transporter = psbfake.NewStoragePoolsServerTransport(&srv)
		if floats {
			transport = floatingHealthTransport{inner: transport}
		}
		client, err := armpurestorageblock.NewStoragePoolsClient(store.subscriptionID, &azcorefake.TokenCredential{},
			&arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: transport}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return client
	}

	t.Run("integer bodies take the SDK path unchanged", func(t *testing.T) {
		store := newFakeStoragePoolStore(testSubscriptionID)
		seedTestStoragePool(store)
		store.setHealth(testResourceGroup, testStoragePoolName, seededHealthInfo())

		info, err := getStoragePoolHealth(context.Background(), newClient(t, store, false), testResourceGroup, testStoragePoolName)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := *info.Health.BandwidthUsage.Provisioned; got != 1073741824 {
			t.Fatalf("provisioned = %d", got)
		}
	})

	t.Run("float bodies fail the plain SDK call but are recovered by the wrapper", func(t *testing.T) {
		store := newFakeStoragePoolStore(testSubscriptionID)
		seedTestStoragePool(store)
		store.setHealth(testResourceGroup, testStoragePoolName, seededHealthInfo())
		client := newClient(t, store, true)

		if _, err := client.GetHealthStatus(context.Background(), testResourceGroup, testStoragePoolName, nil); err == nil {
			t.Fatal("the plain SDK call accepted float-formatted integers; the rewriting transport is not effective")
		}

		info, err := getStoragePoolHealth(context.Background(), client, testResourceGroup, testStoragePoolName)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		h := info.Health
		if *h.BandwidthUsage.Provisioned != 1073741824 || *h.BandwidthUsage.Max != 2147483648 ||
			*h.IopsUsage.Provisioned != 10240 || *h.IopsUsage.Max != 20480 || *h.IopsUsage.Current != 7 {
			t.Fatalf("unexpected values: %+v %+v", h.BandwidthUsage, h.IopsUsage)
		}
		if len(info.Alerts) != 3 {
			t.Fatalf("alerts = %d, want 3", len(info.Alerts))
		}
	})

	t.Run("a real error status is returned unchanged and still classifies as not found", func(t *testing.T) {
		store := newFakeStoragePoolStore(testSubscriptionID)
		seedTestStoragePool(store)

		_, err := getStoragePoolHealth(context.Background(), newClient(t, store, true), testResourceGroup, "does-not-exist")
		if err == nil {
			t.Fatal("expected an error")
		}
		var respErr *azcore.ResponseError
		if !errors.As(err, &respErr) || respErr.StatusCode != http.StatusNotFound {
			t.Fatalf("expected a 404 *azcore.ResponseError, got %v", err)
		}
		if !isNotFound(err) {
			t.Fatalf("isNotFound(%v) = false", err)
		}
	})
}
