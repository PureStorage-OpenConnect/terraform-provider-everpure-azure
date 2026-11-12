// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azcorefake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	psbfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock/fake"
	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

func TestIsSubscriptionMismatch(t *testing.T) {
	tests := []struct {
		name               string
		idSub, expectedSub string
		want               bool
	}{
		{"matching subscriptions, case-insensitively", "ABCD", "abcd", false},
		{"different subscriptions", "22222222-2222-2222-2222-222222222222", "11111111-1111-1111-1111-111111111111", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSubscriptionMismatch(tt.idSub, tt.expectedSub); got != tt.want {
				t.Fatalf("isSubscriptionMismatch(%q, %q) = %v, want %v", tt.idSub, tt.expectedSub, got, tt.want)
			}
		})
	}
}

// TestStateInOtherSubscription checks that Read, Update and Delete refuse state whose ID
// is in another subscription than the provider's, before any call. Each fake store
// ignores the subscription and has nothing seeded, so without the check Read would remove
// the resource from state and Delete would succeed on the 404.
func TestStateInOtherSubscription(t *testing.T) {
	const otherSubscription = "22222222-2222-2222-2222-222222222222"
	inOtherSubscription := func(id string) string {
		return strings.Replace(id, testSubscriptionID, otherSubscription, 1)
	}
	idValue := func(id string) armids.IDValue { return armids.IDValue{StringValue: types.StringValue(id)} }
	mismatch := func(k kind, id string) detailError {
		return newSubscriptionMismatchError(k, id, otherSubscription, testSubscriptionID)
	}

	t.Run("storage pool", func(t *testing.T) {
		r, _ := newTestStoragePoolDirectResource(t)
		data := testStoragePoolStateModel(t)
		pool, err := armids.ParseStoragePoolID(inOtherSubscription(r.requestedID(data)))
		if err != nil {
			t.Fatalf("parsing storage pool ID: %v", err)
		}
		data.ID = idValue(pool.FullID())
		state := newTestStoragePoolState(t, r, data)
		planData := data
		planData.ProvisionedBandwidthInMbps = types.Int64Value(2048)
		plan := newTestStoragePoolState(t, r, planData)

		requireStateInOtherSubscriptionRefused(t, r, state, &plan, func(action verb) diag.Diagnostic {
			op := operation{action: action, kind: kindStoragePool, name: data.Name.ValueString(), id: r.requestedID(data)}
			return diag.WithPath(path.Root("id"), op.fail(mismatch(kindStoragePool, pool.FullID())))
		})
	})

	t.Run("volume group", func(t *testing.T) {
		r, _ := newTestVolumeGroupDeleteResource(t)
		pool, err := armids.ParseStoragePoolID(inOtherSubscription(testStoragePoolIDForVG()))
		if err != nil {
			t.Fatalf("parsing storage pool ID: %v", err)
		}
		inOther := func(data *VolumeGroupResourceModel) {
			data.StoragePoolID = idValue(pool.FullID())
			data.ID = idValue(pool.VolumeGroup("vg1").FullID())
		}
		state := newTestVolumeGroupDeleteState(t, r, inOther)
		plan := newTestVolumeGroupDeleteState(t, r, func(data *VolumeGroupResourceModel) {
			inOther(data)
			data.Tags = types.MapValueMust(types.StringType, map[string]attr.Value{"env": types.StringValue("test")})
		})

		requireStateInOtherSubscriptionRefused(t, r, state, &plan, func(action verb) diag.Diagnostic {
			op := operation{action: action, kind: kindVolumeGroup, name: "vg1", id: pool.VolumeGroup("vg1").FullID()}
			return diag.WithPath(path.Root("storage_pool_id"), op.fail(mismatch(kindStoragePool, pool.FullID())))
		})
	})

	t.Run("volume", func(t *testing.T) {
		r, _ := newTestVolumeDeleteResource(t)
		vg, err := armids.ParseVolumeGroupID(inOtherSubscription(testVolumeGroupIDFixture))
		if err != nil {
			t.Fatalf("parsing volume group ID: %v", err)
		}
		inOther := func(data *VolumeResourceModel) {
			data.VolumeGroupID = idValue(vg.FullID())
			data.ID = idValue(vg.Volume("vol1").FullID())
			data.ProvisionedSizeInBytes = types.Int64Value(1 << 30)
		}
		state := newTestVolumeDeleteState(t, r, inOther)
		plan := newTestVolumeDeleteState(t, r, func(data *VolumeResourceModel) {
			inOther(data)
			data.ProvisionedSizeInBytes = types.Int64Value(2 << 30)
		})

		requireStateInOtherSubscriptionRefused(t, r, state, &plan, func(action verb) diag.Diagnostic {
			op := operation{action: action, kind: kindVolume, name: "vol1", id: vg.Volume("vol1").FullID()}
			return diag.WithPath(path.Root("volume_group_id"), op.fail(mismatch(kindVolumeGroup, vg.FullID())))
		})
	})

	// A snapshot's Update makes no call, so only Read and Delete are checked.
	t.Run("snapshot", func(t *testing.T) {
		r, _ := newTestSnapshotDirectResource(t)
		vg, err := armids.ParseVolumeGroupID(inOtherSubscription(testSnapshotVolumeGroupID))
		if err != nil {
			t.Fatalf("parsing volume group ID: %v", err)
		}
		state := newTestSnapshotDeleteState(t, r, func(data *SnapshotResourceModel) {
			data.VolumeGroupID = idValue(vg.FullID())
			data.ID = idValue(vg.Snapshot("snap1").FullID())
		})

		requireStateInOtherSubscriptionRefused(t, r, state, nil, func(action verb) diag.Diagnostic {
			op := operation{action: action, kind: kindSnapshot, name: "snap1", id: vg.Snapshot("snap1").FullID()}
			return diag.WithPath(path.Root("volume_group_id"), op.fail(mismatch(kindVolumeGroup, vg.FullID())))
		})
	})
}

// requireStateInOtherSubscriptionRefused runs Read, Update (when plan is given) and
// Delete on state and requires each to report want for its own action. Read must also
// keep the resource in state.
func requireStateInOtherSubscriptionRefused(
	t *testing.T, r fwresource.Resource, state tfsdk.State, plan *tfsdk.State, want func(verb) diag.Diagnostic,
) {
	t.Helper()
	ctx := context.Background()

	t.Run("read", func(t *testing.T) {
		resp := &fwresource.ReadResponse{State: state}
		r.Read(ctx, fwresource.ReadRequest{State: state}, resp)
		if w := want(actionRetrieve); !resp.Diagnostics.Contains(w) {
			t.Fatalf("diagnostics = %v, want %v", resp.Diagnostics, w)
		}
		if resp.State.Raw.IsNull() {
			t.Fatal("Read removed the resource from state")
		}
	})

	if plan != nil {
		t.Run("update", func(t *testing.T) {
			resp := &fwresource.UpdateResponse{State: tfsdk.State{Schema: plan.Schema, Raw: plan.Raw}}
			r.Update(ctx, fwresource.UpdateRequest{Plan: tfsdk.Plan{Schema: plan.Schema, Raw: plan.Raw}, State: state}, resp)
			if w := want(actionUpdate); !resp.Diagnostics.Contains(w) {
				t.Fatalf("diagnostics = %v, want %v", resp.Diagnostics, w)
			}
		})
	}

	t.Run("delete", func(t *testing.T) {
		resp := &fwresource.DeleteResponse{State: state}
		r.Delete(ctx, fwresource.DeleteRequest{State: state}, resp)
		if w := want(actionDelete); !resp.Diagnostics.Contains(w) {
			t.Fatalf("diagnostics = %v, want %v", resp.Diagnostics, w)
		}
	})
}

// newTestStoragePoolDirectResource builds a StoragePoolResource wired to an empty
// fakeStoragePoolStore through a direct Configure call, and returns the store with it.
func newTestStoragePoolDirectResource(t *testing.T) (*StoragePoolResource, *fakeStoragePoolStore) {
	t.Helper()

	store := newFakeStoragePoolStore(testSubscriptionID)
	srv := store.newStoragePoolsServer()
	transport := psbfake.NewStoragePoolsServerTransport(&srv)
	opts := &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: transport}}

	r := &StoragePoolResource{}
	var configureResp fwresource.ConfigureResponse
	r.Configure(context.Background(), fwresource.ConfigureRequest{ProviderData: &Clients{
		cred:           &azcorefake.TokenCredential{},
		opts:           opts,
		subscriptionID: testSubscriptionID,
	}}, &configureResp)
	if configureResp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", configureResp.Diagnostics)
	}
	return r, store
}

// testStoragePoolStateModel is the state of pool "pool1" on the shared test resource
// group, with a null id for the caller to set.
func testStoragePoolStateModel(t *testing.T) StoragePoolResourceModel {
	t.Helper()

	subnetID := testSubnetID("snet1")
	subnet, err := armids.TypeSubnet.Parse(subnetID)
	if err != nil {
		t.Fatalf("parsing subnet ID: %v", err)
	}
	vnetID, err := subnet.ParentID()
	if err != nil {
		t.Fatalf("cutting the virtual network ID: %v", err)
	}
	vnetInjection, diags := types.ListValueFrom(context.Background(), vnetInjectionObjectType, []VnetInjectionModel{{
		SubnetID:         armids.IDValue{StringValue: types.StringValue(subnetID)},
		VirtualNetworkID: armids.IDValue{StringValue: types.StringValue(vnetID)},
	}})
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}

	return StoragePoolResourceModel{
		ID:                           armids.IDValue{StringValue: types.StringNull()},
		Name:                         types.StringValue("pool1"),
		ResourceGroupName:            types.StringValue(testResourceGroup),
		Location:                     types.StringValue("eastus2"),
		Zone:                         types.StringValue("1"),
		ReservationID:                armids.IDValue{StringValue: types.StringValue(testReservationID())},
		ProvisionedBandwidthInMbps:   types.Int64Value(1024),
		Tags:                         types.MapNull(types.StringType),
		InternalID:                   types.StringValue("internal-pool1"),
		ProvisionedIops:              types.Int64Value(5120),
		DataRetentionPeriodInSeconds: types.Int64Value(3600),
		VnetInjection:                vnetInjection,
		PlatformConsole:              types.ListNull(platformConsoleObjectType),
		Timeouts: timeouts.Value{Object: types.ObjectNull(map[string]attr.Type{
			"create": types.StringType,
			"read":   types.StringType,
			"update": types.StringType,
			"delete": types.StringType,
		})},
	}
}

func newTestStoragePoolState(t *testing.T, r *StoragePoolResource, data StoragePoolResourceModel) tfsdk.State {
	t.Helper()

	ctx := context.Background()
	var schemaResp fwresource.SchemaResponse
	r.Schema(ctx, fwresource.SchemaRequest{}, &schemaResp)

	state := tfsdk.State{Schema: schemaResp.Schema}
	if diags := state.Set(ctx, &data); diags.HasError() {
		t.Fatalf("unexpected diagnostics building state: %v", diags)
	}
	return state
}
