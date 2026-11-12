// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azcorefake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	psbfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock/fake"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

// newTestConnectionParametersHolder wires store's VolumeGroupsServer fake behind a
// *clientsHolder built directly, without the provider's own Configure.
func newTestConnectionParametersHolder(store *fakeVolumeGroupStore) *clientsHolder {
	srv := store.newVolumeGroupsServer()
	transport := psbfake.NewVolumeGroupsServerTransport(&srv)
	opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: transport}}
	return &clientsHolder{clients: &Clients{
		cred:           &azcorefake.TokenCredential{},
		opts:           opts,
		subscriptionID: store.subscriptionID,
	}}
}

func testVGID(storagePoolName, vgName string) string {
	return armids.VolumeGroupID{
		ResourceGroupID: armids.ResourceGroupID{
			SubscriptionID:    testSubscriptionID,
			ResourceGroupName: testResourceGroup,
		},
		StoragePoolName: storagePoolName,
		VolumeGroupName: vgName,
	}.FullID()
}

func endpoint(ip, iqn string, port int32) *armpurestorageblock.IscsiEndpoint {
	return &armpurestorageblock.IscsiEndpoint{IP: to.Ptr(ip), Iqn: to.Ptr(iqn), Port: to.Ptr(port)}
}

func TestResolveTargets(t *testing.T) {
	ctx := context.Background()

	t.Run("concatenates shared portal IPs with distinct IQNs across volume groups, nothing dropped", func(t *testing.T) {
		store := newFakeVolumeGroupStore(testSubscriptionID)
		holder := newTestConnectionParametersHolder(store)

		store.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			endpoint("10.0.1.5", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
		})
		store.setConnectionParameters(testResourceGroup, "pool1", "vg2", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg2", 3260),
			endpoint("10.0.1.5", "iqn.2010-01.com.purestorage.psc:aaa-vg2", 3260),
		})

		got, diags := resolveVMExtensionTargets(ctx, holder, testVMConnectionOp, []string{testVGID("pool1", "vg1"), testVGID("pool1", "vg2")})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if len(got) != 4 {
			t.Fatalf("got %d targets, want 4: %+v", len(got), got)
		}
	})

	t.Run("de-dups exact (iqn, ip, port) duplicates only", func(t *testing.T) {
		store := newFakeVolumeGroupStore(testSubscriptionID)
		holder := newTestConnectionParametersHolder(store)

		// vg1 and vg2 return the exact same endpoint; it must collapse to one.
		store.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:shared", 3260),
		})
		store.setConnectionParameters(testResourceGroup, "pool1", "vg2", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:shared", 3260),
		})

		got, diags := resolveVMExtensionTargets(ctx, holder, testVMConnectionOp, []string{testVGID("pool1", "vg1"), testVGID("pool1", "vg2")})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if len(got) != 1 {
			t.Fatalf("got %d targets, want 1 (exact duplicate de-duped): %+v", len(got), got)
		}
	})

	t.Run("is deterministic regardless of input order", func(t *testing.T) {
		store := newFakeVolumeGroupStore(testSubscriptionID)
		holder := newTestConnectionParametersHolder(store)

		store.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.5", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
		})
		store.setConnectionParameters(testResourceGroup, "pool1", "vg2", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg2", 3260),
		})

		forward, diags := resolveVMExtensionTargets(ctx, holder, testVMConnectionOp, []string{testVGID("pool1", "vg1"), testVGID("pool1", "vg2")})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		backward, diags := resolveVMExtensionTargets(ctx, holder, testVMConnectionOp, []string{testVGID("pool1", "vg2"), testVGID("pool1", "vg1")})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}

		if len(forward) != len(backward) {
			t.Fatalf("forward has %d targets, backward has %d", len(forward), len(backward))
		}
		for i := range forward {
			if forward[i] != backward[i] {
				t.Fatalf("target %d differs by input order: %+v vs %+v", i, forward[i], backward[i])
			}
		}
	})

	t.Run("a volume group with zero endpoints is an error naming it", func(t *testing.T) {
		store := newFakeVolumeGroupStore(testSubscriptionID)
		holder := newTestConnectionParametersHolder(store)
		vgID := testVGID("pool1", "vg-empty")
		store.setConnectionParameters(testResourceGroup, "pool1", "vg-empty", nil)

		_, diags := resolveVMExtensionTargets(ctx, holder, testVMConnectionOp, []string{vgID})
		if !diags.HasError() {
			t.Fatal("expected an error for a volume group with zero endpoints")
		}
		requireDiagContains(t, diags, vgID)
	})

	t.Run("a volume group that no longer exists (404) names it and suggests removing it", func(t *testing.T) {
		store := newFakeVolumeGroupStore(testSubscriptionID)
		holder := newTestConnectionParametersHolder(store)
		vgID := testVGID("pool1", "vg-gone")
		// Never seeded in store.connectionParameters, so ListConnectionParameters 404s.

		_, diags := resolveVMExtensionTargets(ctx, holder, testVMConnectionOp, []string{vgID})
		if !diags.HasError() {
			t.Fatal("expected an error for a volume group that no longer exists")
		}
		want := diag.WithPath(path.Root("volume_group_ids"), testVMConnectionOp.fail(newNotFoundError(kindVolumeGroup, vgID, nil, apiFailure{})))
		wantParagraph, _, _ := strings.Cut(want.Detail(), "\n\n")
		found := false
		for _, d := range diags {
			withPath, ok := d.(diag.DiagnosticWithPath)
			if ok && d.Summary() == want.Summary() && strings.Contains(d.Detail(), wantParagraph) &&
				withPath.Path().Equal(path.Root("volume_group_ids")) {
				found = true
			}
		}
		if !found {
			t.Fatalf("diagnostics = %v, want one like %v on volume_group_ids", diags, want)
		}
	})

	t.Run("a volume group ID in a different subscription is rejected before any call", func(t *testing.T) {
		store := newFakeVolumeGroupStore(testSubscriptionID)
		holder := newTestConnectionParametersHolder(store)
		otherSubscriptionVGID := armids.VolumeGroupID{
			ResourceGroupID: armids.ResourceGroupID{
				SubscriptionID:    "22222222-2222-2222-2222-222222222222",
				ResourceGroupName: testResourceGroup,
			},
			StoragePoolName: "pool1",
			VolumeGroupName: "vg1",
		}.FullID()

		_, diags := resolveVMExtensionTargets(ctx, holder, testVMConnectionOp, []string{otherSubscriptionVGID})
		if !diags.HasError() {
			t.Fatal("expected an error for a volume group in a different subscription")
		}
		want := diag.WithPath(path.Root("volume_group_ids"), testVMConnectionOp.fail(newSubscriptionMismatchError(kindVolumeGroup, otherSubscriptionVGID, "22222222-2222-2222-2222-222222222222", testSubscriptionID)))
		requireDiagContains(t, diags, want.Detail())
	})

	t.Run("an unconfigured provider (nil clients) reports a diagnostic instead of panicking", func(t *testing.T) {
		// clients.clients is nil when Configure hasn't run yet, e.g. an unresolved
		// subscription_id elsewhere in the same plan.
		holder := &clientsHolder{}
		_, diags := resolveVMExtensionTargets(ctx, holder, testVMConnectionOp, []string{testVGID("pool1", "vg1")})
		if !diags.HasError() {
			t.Fatal("expected an error for an unconfigured provider")
		}
		requireDiagContains(t, diags, operationConfigureProvider.fail(newProviderNotConfiguredError()).Detail())
	})
}

var testVMConnectionOp = operation{action: actionCreate, kind: kindVirtualMachineConnection, in: "vm-id", unnamed: true}

// requireDiagContains fails the test unless some diagnostic's summary or detail
// contains substr.
func requireDiagContains(t *testing.T, diags diag.Diagnostics, substr string) {
	t.Helper()
	for _, d := range diags {
		if strings.Contains(d.Summary(), substr) || strings.Contains(d.Detail(), substr) {
			return
		}
	}
	t.Fatalf("no diagnostic contains %q: %v", substr, diags)
}

// testVolumeGroupsClient gets a *armpurestorageblock.VolumeGroupsClient from holder the
// same way production code does, failing the test if Configure-equivalent setup didn't
// produce one.
func testVolumeGroupsClient(t *testing.T, holder *clientsHolder) *armpurestorageblock.VolumeGroupsClient {
	t.Helper()
	var diags diag.Diagnostics
	client, ok := holder.volumeGroupsClient(&diags)
	if !ok {
		t.Fatalf("failed to build volume groups client: %v", diags)
	}
	return client
}

func TestListVolumeGroupConnectionEndpoints(t *testing.T) {
	ctx := context.Background()

	t.Run("maps endpoints and skips nil entries", func(t *testing.T) {
		store := newFakeVolumeGroupStore(testSubscriptionID)
		holder := newTestConnectionParametersHolder(store)
		client := testVolumeGroupsClient(t, holder)
		vg := armids.VolumeGroupID{
			ResourceGroupID: armids.ResourceGroupID{
				SubscriptionID:    testSubscriptionID,
				ResourceGroupName: testResourceGroup,
			},
			StoragePoolName: "pool1",
			VolumeGroupName: "vg1",
		}
		store.setConnectionParameters(testResourceGroup, "pool1", "vg1", []*armpurestorageblock.IscsiEndpoint{
			endpoint("10.0.1.5", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3260),
			nil,
			endpoint("10.0.1.4", "iqn.2010-01.com.purestorage.psc:aaa-vg1", 3261),
		})

		got, err := listVolumeGroupConnectionEndpoints(ctx, client, vg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []iscsiTarget{
			{IQN: "iqn.2010-01.com.purestorage.psc:aaa-vg1", IP: "10.0.1.5", Port: 3260},
			{IQN: "iqn.2010-01.com.purestorage.psc:aaa-vg1", IP: "10.0.1.4", Port: 3261},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v (nil entry skipped, order preserved)", got, want)
		}
	})

	t.Run("an unseeded volume group returns an error isNotFound recognizes", func(t *testing.T) {
		store := newFakeVolumeGroupStore(testSubscriptionID)
		holder := newTestConnectionParametersHolder(store)
		client := testVolumeGroupsClient(t, holder)
		vg := armids.VolumeGroupID{
			ResourceGroupID: armids.ResourceGroupID{
				SubscriptionID:    testSubscriptionID,
				ResourceGroupName: testResourceGroup,
			},
			StoragePoolName: "pool1",
			VolumeGroupName: "vg-gone",
		}
		// Never seeded in store.connectionParameters, so ListConnectionParameters 404s.

		_, err := listVolumeGroupConnectionEndpoints(ctx, client, vg)
		if err == nil {
			t.Fatal("expected an error for an unseeded volume group")
		}
		if !isNotFound(err) {
			t.Fatalf("expected isNotFound(err) to be true, got err: %v", err)
		}
	})
}

func TestSortISCSITargets(t *testing.T) {
	targets := []iscsiTarget{
		{IQN: "iqn:vg2", IP: "10.0.1.4", Port: 3260},
		{IQN: "iqn:vg1", IP: "10.0.1.5", Port: 3260},
		{IQN: "iqn:vg1", IP: "10.0.1.4", Port: 3261},
		{IQN: "iqn:vg1", IP: "10.0.1.4", Port: 3260},
	}

	sortISCSITargets(targets)

	want := []iscsiTarget{
		{IQN: "iqn:vg1", IP: "10.0.1.4", Port: 3260},
		{IQN: "iqn:vg1", IP: "10.0.1.4", Port: 3261},
		{IQN: "iqn:vg1", IP: "10.0.1.5", Port: 3260},
		{IQN: "iqn:vg2", IP: "10.0.1.4", Port: 3260},
	}
	if !reflect.DeepEqual(targets, want) {
		t.Fatalf("got %+v, want %+v", targets, want)
	}
}

// TestConnectionParametersResponseShape pins the field sets of the SDK types this
// package decodes ListConnectionParameters' response from. If a future SDK version
// adds a field to any of them, this test fails until it's reviewed: a credential added
// to this response (for example a CHAP secret) must be served through a separate,
// ephemeral resource, never persisted through this data source's result.
func TestConnectionParametersResponseShape(t *testing.T) {
	requireFields := func(t *testing.T, v any, want []string) {
		t.Helper()
		typ := reflect.TypeOf(v)
		got := make([]string, typ.NumField())
		for i := range got {
			got[i] = typ.Field(i).Name
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf(
				"%s's fields are %v, want %v. If a field was added, review whether it is a credential (which "+
					"must be served through a separate, ephemeral resource, never persisted through this data "+
					"source's result) before updating this test's expectation.",
				typ.Name(), got, want,
			)
		}
	}

	requireFields(t, armpurestorageblock.ConnectionParametersResponse{}, []string{"Iscsi"})
	requireFields(t, armpurestorageblock.IscsiConnectionParameters{}, []string{"Endpoints"})
	requireFields(t, armpurestorageblock.IscsiEndpoint{}, []string{"IP", "Iqn", "Port"})
}
