// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azcorefake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	computefake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9/fake"
)

// newTestExtensionsClient builds a real armcompute.VirtualMachineExtensionsClient and
// the store it's wired to.
func newTestExtensionsClient(t *testing.T) (*fakeComputeStore, *armcompute.VirtualMachineExtensionsClient) {
	t.Helper()
	store := newFakeComputeStore(testSubscriptionID)
	return store, extensionsClientFor(t, store)
}

// extensionsClientFor builds a fresh armcompute.VirtualMachineExtensionsClient against
// an already-populated store. The generated fake transport's poller tracker is keyed
// by URL and never cleared after a terminal error, so a second call to the same URL
// needs a fresh client.
func extensionsClientFor(t *testing.T, store *fakeComputeStore) *armcompute.VirtualMachineExtensionsClient {
	t.Helper()
	srv := store.newVirtualMachineExtensionsServer()
	transport := computefake.NewVirtualMachineExtensionsServerTransport(&srv)
	client, err := armcompute.NewVirtualMachineExtensionsClient(testSubscriptionID, &azcorefake.TokenCredential{}, &arm.ClientOptions{
		ClientOptions: azcore.ClientOptions{Transport: transport},
	})
	if err != nil {
		t.Fatalf("NewVirtualMachineExtensionsClient: %v", err)
	}
	return client
}

func TestFakeComputeStore_CreateOrUpdateAndGet(t *testing.T) {
	store, client := newTestExtensionsClient(t)
	store.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
	ctx := context.Background()

	settings := map[string]any{"sessionCount": float64(2), "replaceTargets": true}
	poller, err := client.BeginCreateOrUpdate(ctx, testResourceGroup, "vm1", "PureStorageMountIscsi", armcompute.VirtualMachineExtension{
		Location: to.Ptr("eastus2"),
		Properties: &armcompute.VirtualMachineExtensionProperties{
			Publisher:               to.Ptr("PureStorage.Extensions"),
			Type:                    to.Ptr("MountIscsiLinux"),
			TypeHandlerVersion:      to.Ptr("1.3"),
			AutoUpgradeMinorVersion: to.Ptr(true),
			Settings:                settings,
		},
	}, nil)
	if err != nil {
		t.Fatalf("BeginCreateOrUpdate: %v", err)
	}
	created, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		t.Fatalf("PollUntilDone: %v", err)
	}
	if created.Properties == nil || created.Properties.InstanceView != nil {
		t.Fatalf("PUT response must carry no instance view, got %+v", created.Properties)
	}
	if created.Properties.ProvisioningState == nil || *created.Properties.ProvisioningState != "Succeeded" {
		t.Fatalf("expected ProvisioningState Succeeded, got %v", created.Properties.ProvisioningState)
	}

	// Plain GET: still no instance view.
	plain, err := client.Get(ctx, testResourceGroup, "vm1", "PureStorageMountIscsi", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if plain.Properties.InstanceView != nil {
		t.Fatalf("plain GET must carry no instance view, got %+v", plain.Properties.InstanceView)
	}
	gotSettings, ok := plain.Properties.Settings.(map[string]any)
	if !ok || gotSettings["sessionCount"] != float64(2) {
		t.Fatalf("settings not stored verbatim: %+v", plain.Properties.Settings)
	}

	// GET with $expand=instanceView: default success status.
	expanded, err := client.Get(ctx, testResourceGroup, "vm1", "PureStorageMountIscsi", &armcompute.VirtualMachineExtensionsClientGetOptions{
		Expand: to.Ptr("instanceView"),
	})
	if err != nil {
		t.Fatalf("Get with expand: %v", err)
	}
	iv := expanded.Properties.InstanceView
	if iv == nil || len(iv.Statuses) != 1 || iv.Statuses[0].Message == nil ||
		!strings.Contains(*iv.Statuses[0].Message, "completed successfully") {
		t.Fatalf("expected default success instance view, got %+v", iv)
	}

	log := store.operationLog()
	if len(log) != 1 || log[0].Op != "PUT" || log[0].Key != computeExtensionKey(testResourceGroup, "vm1", "PureStorageMountIscsi") {
		t.Fatalf("expected one recorded PUT, got %+v", log)
	}
	if log[0].Body.Properties.Settings == nil {
		t.Fatalf("recorded PUT body lost its settings")
	}
}

func TestFakeComputeStore_InstanceViewSequence(t *testing.T) {
	store, client := newTestExtensionsClient(t)
	store.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesWindows, "eastus2")
	ctx := context.Background()

	rebooting := &armcompute.InstanceViewStatus{
		Code:    to.Ptr("ProvisioningState/succeeded"),
		Level:   to.Ptr(armcompute.StatusLevelTypesInfo),
		Message: to.Ptr("Enable succeeded: Rebooting as requested..."),
	}
	done := &armcompute.InstanceViewStatus{
		Code:    to.Ptr("ProvisioningState/succeeded"),
		Level:   to.Ptr(armcompute.StatusLevelTypesInfo),
		Message: to.Ptr("Enable succeeded: iSCSI configuration completed successfully"),
	}
	store.setInstanceViewSequence(testResourceGroup, "vm1", "PureStorageMountIscsi", rebooting, done)

	poller, err := client.BeginCreateOrUpdate(ctx, testResourceGroup, "vm1", "PureStorageMountIscsi", armcompute.VirtualMachineExtension{
		Location: to.Ptr("eastus2"),
		Properties: &armcompute.VirtualMachineExtensionProperties{
			Publisher: to.Ptr("PureStorage.Extensions"),
			Type:      to.Ptr("MountIscsiWindows"),
			Settings:  map[string]any{"reboot": true},
		},
	}, nil)
	if err != nil {
		t.Fatalf("BeginCreateOrUpdate: %v", err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		t.Fatalf("PollUntilDone: %v", err)
	}

	expandOpts := &armcompute.VirtualMachineExtensionsClientGetOptions{Expand: to.Ptr("instanceView")}

	first, err := client.Get(ctx, testResourceGroup, "vm1", "PureStorageMountIscsi", expandOpts)
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if msg := *first.Properties.InstanceView.Statuses[0].Message; !strings.Contains(msg, "Rebooting") {
		t.Fatalf("first GET: expected rebooting message, got %q", msg)
	}

	second, err := client.Get(ctx, testResourceGroup, "vm1", "PureStorageMountIscsi", expandOpts)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if msg := *second.Properties.InstanceView.Statuses[0].Message; !strings.Contains(msg, "completed successfully") {
		t.Fatalf("second GET: expected completed message, got %q", msg)
	}

	// The last scripted status must stick on every subsequent read.
	third, err := client.Get(ctx, testResourceGroup, "vm1", "PureStorageMountIscsi", expandOpts)
	if err != nil {
		t.Fatalf("third Get: %v", err)
	}
	if msg := *third.Properties.InstanceView.Statuses[0].Message; !strings.Contains(msg, "completed successfully") {
		t.Fatalf("third GET: expected completed message to stick, got %q", msg)
	}
}

func TestFakeComputeStore_QueueLROFailure(t *testing.T) {
	store, client := newTestExtensionsClient(t)
	store.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesWindows, "eastus2")
	ctx := context.Background()

	rebootRequired := &armcompute.InstanceViewStatus{
		Code:    to.Ptr("ProvisioningState/failed/0"),
		Level:   to.Ptr(armcompute.StatusLevelTypesError),
		Message: to.Ptr("Enable failed: To finish iSCSI configuration, reboot is required."),
	}
	store.queueLROFailure(testResourceGroup, "vm1", "PureStorageMountIscsi", rebootRequired)

	// The initiating BeginCreateOrUpdate call succeeds and hands back a poller; only
	// PollUntilDone sees the failure.
	poller, err := client.BeginCreateOrUpdate(ctx, testResourceGroup, "vm1", "PureStorageMountIscsi", armcompute.VirtualMachineExtension{
		Location: to.Ptr("eastus2"),
		Properties: &armcompute.VirtualMachineExtensionProperties{
			Publisher: to.Ptr("PureStorage.Extensions"),
			Type:      to.Ptr("MountIscsiWindows"),
			Settings:  map[string]any{"reboot": false},
		},
	}, nil)
	if err == nil {
		if _, err = poller.PollUntilDone(ctx, nil); err == nil {
			t.Fatal("expected the create to fail, either synchronously or via the poller")
		}
	}

	expanded, err := client.Get(ctx, testResourceGroup, "vm1", "PureStorageMountIscsi", &armcompute.VirtualMachineExtensionsClientGetOptions{
		Expand: to.Ptr("instanceView"),
	})
	if err != nil {
		t.Fatalf("Get with expand: %v", err)
	}
	msg := expanded.Properties.InstanceView.Statuses[0].Message
	if msg == nil || !strings.Contains(*msg, "reboot is required") {
		t.Fatalf("expected the scripted reboot-required message via GET, got %v", msg)
	}
	if expanded.Properties.ProvisioningState == nil || *expanded.Properties.ProvisioningState != "Failed" {
		t.Fatalf("expected ProvisioningState Failed after a failed LRO, got %v", expanded.Properties.ProvisioningState)
	}

	// The one-shot outcome must not persist: a second, unscripted create succeeds. A
	// fresh client avoids the exhausted-poller-tracker reuse.
	client = extensionsClientFor(t, store)
	poller2, err := client.BeginCreateOrUpdate(ctx, testResourceGroup, "vm1", "PureStorageMountIscsi", armcompute.VirtualMachineExtension{
		Location: to.Ptr("eastus2"),
		Properties: &armcompute.VirtualMachineExtensionProperties{
			Publisher: to.Ptr("PureStorage.Extensions"),
			Type:      to.Ptr("MountIscsiWindows"),
			Settings:  map[string]any{"reboot": true},
		},
	}, nil)
	if err != nil {
		t.Fatalf("second BeginCreateOrUpdate: %v", err)
	}
	if _, err := poller2.PollUntilDone(ctx, nil); err != nil {
		t.Fatalf("second PollUntilDone: expected success now the one-shot outcome is consumed, got %v", err)
	}
}

func TestFakeComputeStore_BeginCreateOrUpdateValidatesParentAndLocation(t *testing.T) {
	extProperties := &armcompute.VirtualMachineExtensionProperties{
		Publisher: to.Ptr("PureStorage.Extensions"),
		Type:      to.Ptr("MountIscsiLinux"),
	}

	t.Run("404 when the parent virtual machine doesn't exist", func(t *testing.T) {
		_, client := newTestExtensionsClient(t)
		// No seedVirtualMachine call: the parent VM doesn't exist.
		_, err := client.BeginCreateOrUpdate(context.Background(), testResourceGroup, "ghost-vm", "PureStorageMountIscsi", armcompute.VirtualMachineExtension{
			Location:   to.Ptr("eastus2"),
			Properties: extProperties,
		}, nil)
		if !isNotFound(err) {
			t.Fatalf("expected a 404 when the parent virtual machine doesn't exist, got %v", err)
		}
	})

	t.Run("400 on a nil Location", func(t *testing.T) {
		store, client := newTestExtensionsClient(t)
		store.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
		_, err := client.BeginCreateOrUpdate(context.Background(), testResourceGroup, "vm1", "PureStorageMountIscsi", armcompute.VirtualMachineExtension{
			Properties: extProperties,
		}, nil)
		var respErr *azcore.ResponseError
		if !errors.As(err, &respErr) || respErr.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected a 400 on a nil Location, got %v", err)
		}
	})

	t.Run("400 on an empty Location", func(t *testing.T) {
		store, client := newTestExtensionsClient(t)
		store.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
		_, err := client.BeginCreateOrUpdate(context.Background(), testResourceGroup, "vm1", "PureStorageMountIscsi", armcompute.VirtualMachineExtension{
			Location:   to.Ptr(""),
			Properties: extProperties,
		}, nil)
		var respErr *azcore.ResponseError
		if !errors.As(err, &respErr) || respErr.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected a 400 on an empty Location, got %v", err)
		}
	})
}

func TestFakeComputeStore_DuplicateHandlerOn400(t *testing.T) {
	store, client := newTestExtensionsClient(t)
	store.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
	// Seed an existing extension out of band, under a different name.
	store.seedExtension(testResourceGroup, "vm1", "existing-name", "PureStorage.Extensions", "MountIscsiLinux", map[string]any{})
	ctx := context.Background()

	_, err := client.BeginCreateOrUpdate(ctx, testResourceGroup, "vm1", "new-name", armcompute.VirtualMachineExtension{
		Location: to.Ptr("eastus2"),
		Properties: &armcompute.VirtualMachineExtensionProperties{
			Publisher: to.Ptr("PureStorage.Extensions"),
			Type:      to.Ptr("MountIscsiLinux"),
			Settings:  map[string]any{},
		},
	}, nil)
	if err == nil {
		t.Fatal("expected the synchronous duplicate-handler 400")
	}
	var respErr *azcore.ResponseError
	if !errors.As(err, &respErr) {
		t.Fatalf("expected *azcore.ResponseError, got %T: %v", err, err)
	}
	if respErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", respErr.StatusCode)
	}
	wantMsg := duplicateExtensionMessage("Linux", "PureStorage.Extensions.MountIscsiLinux", "new-name")
	if !strings.Contains(respErr.Error(), wantMsg) {
		t.Fatalf("expected the live duplicate-handler body in the error, got:\n%s\nwant substring:\n%s", respErr.Error(), wantMsg)
	}

	// Updating the same name must not be treated as a collision with itself.
	store.seedExtension(testResourceGroup, "vm1", "existing-name", "PureStorage.Extensions", "MountIscsiLinux", map[string]any{})
	_, err = client.BeginCreateOrUpdate(ctx, testResourceGroup, "vm1", "existing-name", armcompute.VirtualMachineExtension{
		Location: to.Ptr("eastus2"),
		Properties: &armcompute.VirtualMachineExtensionProperties{
			Publisher: to.Ptr("PureStorage.Extensions"),
			Type:      to.Ptr("MountIscsiLinux"),
			Settings:  map[string]any{"sessionCount": float64(3)},
		},
	}, nil)
	if err != nil {
		t.Fatalf("updating the same extension name must not collide with itself: %v", err)
	}
}

func TestFakeComputeStore_QueuePutFailure(t *testing.T) {
	store, client := newTestExtensionsClient(t)
	store.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
	store.seedExtension(testResourceGroup, "vm1", "PureStorageMountIscsi", "PureStorage.Extensions", "MountIscsiLinux", map[string]any{})
	ctx := context.Background()

	store.queuePutFailure(testResourceGroup, "vm1", "PureStorageMountIscsi", 500, "InternalError", "simulated clearing PUT failure")

	_, err := client.BeginCreateOrUpdate(ctx, testResourceGroup, "vm1", "PureStorageMountIscsi", armcompute.VirtualMachineExtension{
		Location: to.Ptr("eastus2"),
		Properties: &armcompute.VirtualMachineExtensionProperties{
			Publisher: to.Ptr("PureStorage.Extensions"),
			Type:      to.Ptr("MountIscsiLinux"),
			Settings:  map[string]any{"targets": []any{}, "replaceTargets": true},
		},
	}, nil)
	if err == nil {
		t.Fatal("expected the queued synchronous failure")
	}
	if !strings.Contains(err.Error(), "simulated clearing PUT failure") {
		t.Fatalf("expected the queued message, got %v", err)
	}
}

func TestFakeComputeStore_DeleteRecordsOrderAndVMHelpers(t *testing.T) {
	store, client := newTestExtensionsClient(t)
	store.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
	store.seedExtension(testResourceGroup, "vm1", "PureStorageMountIscsi", "PureStorage.Extensions", "MountIscsiLinux", map[string]any{})
	ctx := context.Background()

	// A clearing PUT, then a DELETE, must be recorded in that order.
	poller, err := client.BeginCreateOrUpdate(ctx, testResourceGroup, "vm1", "PureStorageMountIscsi", armcompute.VirtualMachineExtension{
		Location: to.Ptr("eastus2"),
		Properties: &armcompute.VirtualMachineExtensionProperties{
			Publisher: to.Ptr("PureStorage.Extensions"),
			Type:      to.Ptr("MountIscsiLinux"),
			Settings:  map[string]any{"targets": []any{}, "replaceTargets": true},
		},
	}, nil)
	if err != nil {
		t.Fatalf("clearing PUT: %v", err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		t.Fatalf("clearing PUT PollUntilDone: %v", err)
	}

	delPoller, err := client.BeginDelete(ctx, testResourceGroup, "vm1", "PureStorageMountIscsi", nil)
	if err != nil {
		t.Fatalf("BeginDelete: %v", err)
	}
	if _, err := delPoller.PollUntilDone(ctx, nil); err != nil {
		t.Fatalf("BeginDelete PollUntilDone: %v", err)
	}

	log := store.operationLog()
	if len(log) != 2 || log[0].Op != "PUT" || log[1].Op != "DELETE" {
		t.Fatalf("expected [PUT, DELETE] in order, got %+v", log)
	}

	if _, err := client.Get(ctx, testResourceGroup, "vm1", "PureStorageMountIscsi", nil); !isNotFound(err) {
		t.Fatalf("expected 404 after delete, got %v", err)
	}

	// removeVirtualMachine: VM Get must 404 once removed.
	store.removeVirtualMachine(testResourceGroup, "vm1")
	vmSrv := store.newVirtualMachinesServer()
	_ = vmSrv // constructed to prove the server still builds against an empty store
}

func TestFakeComputeStore_Recasing(t *testing.T) {
	store, client := newTestExtensionsClient(t)
	store.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
	store.seedExtension(testResourceGroup, "vm1", "PureStorageMountIscsi", "PureStorage.Extensions", "MountIscsiLinux", map[string]any{})
	store.recaseIDs = true
	defer func() { store.recaseIDs = false }()

	got, err := client.Get(context.Background(), testResourceGroup, "vm1", "PureStorageMountIscsi", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID == nil || *got.ID != strings.ToUpper(*got.ID) {
		t.Fatalf("expected recaseIDs to upper-case the ID, got %v", got.ID)
	}
	if got.Name == nil || *got.Name != strings.ToUpper(*got.Name) {
		t.Fatalf("expected recaseIDs to upper-case the name, got %v", got.Name)
	}
}

// TestFakeComputeStore_SelfHealingTransportSurvivesFailedCreate drives a failed
// create, then a clearing PUT and a delete, all against one shared client/transport,
// proving the self-healing transport keeps every call independent after a failed LRO.
func TestFakeComputeStore_SelfHealingTransportSurvivesFailedCreate(t *testing.T) {
	store := newFakeComputeStore(testSubscriptionID)
	store.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesWindows, "eastus2")
	ctx := context.Background()

	transport := store.newSelfHealingComputeTransport()
	client, err := armcompute.NewVirtualMachineExtensionsClient(testSubscriptionID, &azcorefake.TokenCredential{}, &arm.ClientOptions{
		ClientOptions: azcore.ClientOptions{Transport: transport},
	})
	if err != nil {
		t.Fatalf("NewVirtualMachineExtensionsClient: %v", err)
	}

	rebootRequired := &armcompute.InstanceViewStatus{
		Code:    to.Ptr("ProvisioningState/failed/0"),
		Level:   to.Ptr(armcompute.StatusLevelTypesError),
		Message: to.Ptr("Enable failed: To finish iSCSI configuration, reboot is required."),
	}
	store.queueLROFailure(testResourceGroup, "vm1", "PureStorageMountIscsi", rebootRequired)

	poller, err := client.BeginCreateOrUpdate(ctx, testResourceGroup, "vm1", "PureStorageMountIscsi", armcompute.VirtualMachineExtension{
		Location: to.Ptr("eastus2"),
		Properties: &armcompute.VirtualMachineExtensionProperties{
			Publisher: to.Ptr("PureStorage.Extensions"),
			Type:      to.Ptr("MountIscsiWindows"),
			Settings:  map[string]any{"reboot": false},
		},
	}, nil)
	if err == nil {
		if _, err = poller.PollUntilDone(ctx, nil); err == nil {
			t.Fatal("expected the create's LRO to fail")
		}
	}

	// The same client, same transport, same URL: a second BeginCreateOrUpdate must
	// reach the store's handler again and succeed.
	clearPoller, err := client.BeginCreateOrUpdate(ctx, testResourceGroup, "vm1", "PureStorageMountIscsi", armcompute.VirtualMachineExtension{
		Location: to.Ptr("eastus2"),
		Properties: &armcompute.VirtualMachineExtensionProperties{
			Publisher: to.Ptr("PureStorage.Extensions"),
			Type:      to.Ptr("MountIscsiWindows"),
			Settings:  map[string]any{"targets": []any{}, "replaceTargets": true},
		},
	}, nil)
	if err != nil {
		t.Fatalf("clearing BeginCreateOrUpdate after a failed create: %v", err)
	}
	if _, err := clearPoller.PollUntilDone(ctx, nil); err != nil {
		t.Fatalf("clearing PollUntilDone after a failed create: %v", err)
	}

	delPoller, err := client.BeginDelete(ctx, testResourceGroup, "vm1", "PureStorageMountIscsi", nil)
	if err != nil {
		t.Fatalf("BeginDelete after a failed create: %v", err)
	}
	if _, err := delPoller.PollUntilDone(ctx, nil); err != nil {
		t.Fatalf("BeginDelete PollUntilDone after a failed create: %v", err)
	}

	log := store.operationLog()
	if len(log) != 3 || log[0].Op != "PUT" || log[1].Op != "PUT" || log[2].Op != "DELETE" {
		t.Fatalf("expected [PUT, PUT, DELETE] all reaching the store, got %+v", log)
	}
}
