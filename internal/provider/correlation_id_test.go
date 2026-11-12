// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azcorefake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	psbfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock/fake"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestCorrelationRequestIDPolicy(t *testing.T) {
	req, err := runtime.NewRequest(context.Background(), http.MethodGet, "https://management.azure.com/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No further policies are attached, so Next() has nothing to call; only
	// the header write below is under test.
	_, _ = (&correlationRequestIDPolicy{id: "run-1"}).Do(req)

	if got := req.Raw().Header.Get("X-Ms-Correlation-Request-Id"); got != "run-1" {
		t.Fatalf("X-Ms-Correlation-Request-Id = %q, want %q", got, "run-1")
	}
}

// capturedRequest is one HTTP round trip observed by capturingTransport.
type capturedRequest struct {
	method                 string
	path                   string
	correlationID          string
	auxiliaryAuthorization string
}

// capturingTransport wraps a fake ARM transport and records the method,
// path, X-Ms-Correlation-Request-Id and x-ms-authorization-auxiliary headers
// of every request that passes through it, so a test can confirm a header
// rides along on every call, including a create's LRO polls, not just the
// initial request. Guarded by
// a mutex: terraform-plugin-testing drives the provider over the plugin
// protocol, which dispatches from more than one goroutine.
type capturingTransport struct {
	inner policy.Transporter

	mu       sync.Mutex
	requests []capturedRequest
}

func (c *capturingTransport) Do(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.requests = append(c.requests, capturedRequest{
		method:                 req.Method,
		path:                   req.URL.Path,
		correlationID:          req.Header.Get("X-Ms-Correlation-Request-Id"),
		auxiliaryAuthorization: req.Header.Get("X-Ms-Authorization-Auxiliary"),
	})
	c.mu.Unlock()
	return c.inner.Do(req)
}

func TestCorrelationIDOnEveryRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("drives the real terraform CLI; skipped with -short")
	}

	store := newFakeStoragePoolStore(testSubscriptionID)
	store.pollCreate = true
	srv := store.newStoragePoolsServer()
	capture := &capturingTransport{inner: psbfake.NewStoragePoolsServerTransport(&srv)}

	opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: capture}}
	p := newProvider("test", fixedCredentialFactory(&azcorefake.TokenCredential{}), opts)()
	factories := map[string]func() (tfprotov6.ProviderServer, error){
		"everpure": providerserver.NewProtocol6WithError(p),
	}

	subnetID := testSubnetID("snet1")
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
`, testResourceGroup, testReservationID(), subnetID)

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{Config: config},
		},
	})

	var sawCreate, sawPoll bool
	want := ""
	for i, r := range capture.requests {
		if r.correlationID == "" {
			t.Fatalf("request %d (%s %s) carried no correlation ID", i, r.method, r.path)
		}
		if want == "" {
			want = r.correlationID
		} else if r.correlationID != want {
			t.Fatalf("request %d (%s %s) carried correlation ID %q, want %q (same value for the whole provider process)",
				i, r.method, r.path, r.correlationID, want)
		}
		if r.method == http.MethodPut {
			sawCreate = true
		}
		// The fake SDK's poller dispatches non-terminal polls to a distinct
		// "/get/fake/status" path (confirmed by observing the actual request
		// sequence), separate from both the initial create and Terraform's
		// own post-apply refresh GET on the resource itself.
		if strings.Contains(r.path, "/get/fake/status") {
			sawPoll = true
		}
	}
	if !sawCreate {
		t.Fatal("expected to observe the initial create PUT")
	}
	if !sawPoll {
		t.Fatal("expected to observe at least one LRO poll request, distinct from the create and any resource GET")
	}
}
