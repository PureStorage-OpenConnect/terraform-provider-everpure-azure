// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azcorefake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	computefake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9/fake"
	psbfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock/fake"
	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// TestClientsReachComputeClients checks that a single *Clients hands out working
// armcompute clients alongside its armpurestorageblock ones, every request carries
// the correlation ID, and multiModuleTransport routes each client to its own module's
// fake server.
func TestClientsReachComputeClients(t *testing.T) {
	poolStore := newFakeStoragePoolStore(testSubscriptionID)

	computeStore := newFakeComputeStore(testSubscriptionID)
	computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")

	poolSrv := poolStore.newStoragePoolsServer()
	vmSrv := computeStore.newVirtualMachinesServer()
	extSrv := computeStore.newVirtualMachineExtensionsServer()

	transport := &multiModuleTransport{
		purestorageblock: psbfake.NewStoragePoolsServerTransport(&poolSrv),
		compute: computefake.NewServerFactoryTransport(&computefake.ServerFactory{
			VirtualMachinesServer:          vmSrv,
			VirtualMachineExtensionsServer: extSrv,
		}),
	}
	capture := &capturingTransport{inner: transport}

	const correlationID = "corr-compute-1"
	opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: capture}}
	opts.PerCallPolicies = append(opts.PerCallPolicies, &correlationRequestIDPolicy{id: correlationID})

	clients := &Clients{
		cred:           &azcorefake.TokenCredential{},
		opts:           opts,
		subscriptionID: testSubscriptionID,
		correlationID:  correlationID,
	}
	holder := &clientsHolder{clients: clients}
	var diags diag.Diagnostics

	vmClient, ok := holder.virtualMachinesClient(testSubscriptionID, &diags)
	if !ok || diags.HasError() {
		t.Fatalf("virtualMachinesClient: unexpected diagnostics: %v", diags)
	}
	vmResp, err := vmClient.Get(context.Background(), testResourceGroup, "vm1", nil)
	if err != nil {
		t.Fatalf("VirtualMachinesClient.Get: unexpected error: %v", err)
	}
	if vmResp.Properties == nil || vmResp.Properties.StorageProfile == nil ||
		vmResp.Properties.StorageProfile.OSDisk == nil || vmResp.Properties.StorageProfile.OSDisk.OSType == nil ||
		*vmResp.Properties.StorageProfile.OSDisk.OSType != armcompute.OperatingSystemTypesLinux {
		t.Fatalf("VirtualMachinesClient.Get returned unexpected VM: %+v", vmResp.VirtualMachine)
	}

	extClient, ok := holder.virtualMachineExtensionsClient(testSubscriptionID, &diags)
	if !ok || diags.HasError() {
		t.Fatalf("virtualMachineExtensionsClient: unexpected diagnostics: %v", diags)
	}
	_, err = extClient.Get(context.Background(), testResourceGroup, "vm1", "PureStorageMountIscsi", nil)
	if !isNotFound(err) {
		t.Fatalf("VirtualMachineExtensionsClient.Get on a never-created extension: got %v, want a 404", err)
	}

	spClient, ok := holder.storagePoolsClient(&diags)
	if !ok || diags.HasError() {
		t.Fatalf("storagePoolsClient: unexpected diagnostics: %v", diags)
	}
	_, err = spClient.Get(context.Background(), testResourceGroup, "pool-does-not-exist", nil)
	if !isNotFound(err) {
		t.Fatalf("StoragePoolsClient.Get on a never-created pool: got %v, want a 404", err)
	}

	var sawVMGet, sawExtGet, sawPoolGet bool
	for _, r := range capture.requests {
		if r.correlationID != correlationID {
			t.Fatalf("request %s %s carried correlation ID %q, want %q", r.method, r.path, r.correlationID, correlationID)
		}
		switch {
		case r.method == http.MethodGet && strings.Contains(r.path, "/virtualMachines/vm1") && !strings.Contains(r.path, "/extensions/"):
			sawVMGet = true
		case r.method == http.MethodGet && strings.Contains(r.path, "/extensions/PureStorageMountIscsi"):
			sawExtGet = true
		case r.method == http.MethodGet && strings.Contains(r.path, "/storagePools/pool-does-not-exist"):
			sawPoolGet = true
		}
	}
	if !sawVMGet {
		t.Fatal("expected to observe the VirtualMachinesClient.Get request")
	}
	if !sawExtGet {
		t.Fatal("expected to observe the VirtualMachineExtensionsClient.Get request")
	}
	if !sawPoolGet {
		t.Fatal("expected to observe the StoragePoolsClient.Get request routed through the same combined transport")
	}
}
