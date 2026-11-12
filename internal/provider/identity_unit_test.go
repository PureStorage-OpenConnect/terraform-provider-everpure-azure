// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azcorefake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	psbfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock/fake"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/identityschema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

func nullIdentity(t *testing.T, schema identityschema.Schema) *tfsdk.ResourceIdentity {
	t.Helper()
	return &tfsdk.ResourceIdentity{
		Schema: schema,
		Raw:    tftypes.NewValue(schema.Type().TerraformType(context.Background()), nil),
	}
}

func TestSetIdentityFromIDIfNull(t *testing.T) {
	ctx := context.Background()
	id := armids.StoragePoolID{
		ResourceGroupID: armids.ResourceGroupID{SubscriptionID: testSubscriptionID, ResourceGroupName: testResourceGroup},
		StoragePoolName: "pool1",
	}.FullID()

	t.Run("a null identity, as state from before identity existed has, is filled in", func(t *testing.T) {
		identity := nullIdentity(t, armids.StoragePoolIdentitySchema())
		if diags := setIdentityFromIDIfNull(ctx, identity, id, armids.ParseStoragePoolID); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		var got armids.StoragePoolID
		if diags := identity.Get(ctx, &got); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		want := armids.StoragePoolID{ResourceGroupID: armids.ResourceGroupID{SubscriptionID: testSubscriptionID, ResourceGroupName: testResourceGroup}, StoragePoolName: "pool1"}
		if got != want {
			t.Fatalf("identity = %+v, want %+v", got, want)
		}
	})

	t.Run("a present identity is left alone", func(t *testing.T) {
		identity := nullIdentity(t, armids.StoragePoolIdentitySchema())
		first := armids.StoragePoolID{ResourceGroupID: armids.ResourceGroupID{SubscriptionID: "a", ResourceGroupName: "b"}, StoragePoolName: "c"}
		if diags := identity.Set(ctx, first); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if diags := setIdentityFromIDIfNull(ctx, identity, id, armids.ParseStoragePoolID); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		var got armids.StoragePoolID
		if diags := identity.Get(ctx, &got); diags.HasError() || got != first {
			t.Fatalf("identity = %+v (diagnostics %v), want it left as %+v", got, diags, first)
		}
	})

	t.Run("a nil identity (no identity support negotiated) is a no-op", func(t *testing.T) {
		if diags := setIdentityFromIDIfNull(ctx, nil, id, armids.ParseStoragePoolID); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
	})
}

// identityImportCase is one ImportState-by-identity call made directly on a
// resource: the terraform CLI cannot be made to send an arbitrary identity, so
// the subscription and malformed-value rules are driven here.
type identityImportCase struct {
	name     string
	resource func(*Clients) resource.Resource
	schema   identityschema.Schema
	identity any
	wantID   string
	wantErr  string
}

func TestImportStateByIdentity(t *testing.T) {
	ctx := context.Background()
	otherSubscription := "22222222-2222-2222-2222-222222222222"

	rg := func(sub string) armids.ResourceGroupID {
		return armids.ResourceGroupID{SubscriptionID: sub, ResourceGroupName: testResourceGroup}
	}
	resID := func(sub, name string) armids.ReservationID {
		return armids.ReservationID{ResourceGroupID: rg(sub), ReservationName: name}
	}
	poolID := func(sub, name string) armids.StoragePoolID {
		return armids.StoragePoolID{ResourceGroupID: rg(sub), StoragePoolName: name}
	}
	vgID := func(sub string) armids.VolumeGroupID {
		return armids.VolumeGroupID{ResourceGroupID: rg(sub), StoragePoolName: "pool1", VolumeGroupName: "vg1"}
	}
	volID := func(sub string) armids.VolumeID {
		return armids.VolumeID{
			ResourceGroupID: rg(sub), StoragePoolName: "pool1", VolumeGroupName: "vg1", VolumeName: "vol1",
		}
	}
	snapID := func(sub string) armids.VolumeGroupSnapshotID {
		return armids.VolumeGroupSnapshotID{
			ResourceGroupID: rg(sub), StoragePoolName: "pool1", VolumeGroupName: "vg1", SnapshotName: "snap1",
		}
	}
	extID := func(sub string) armids.VirtualMachineExtensionID {
		return armids.VirtualMachineExtensionID{
			ResourceGroupID: rg(sub), VirtualMachineName: "vm1", ExtensionName: "ext1",
		}
	}

	mismatch := "the provider is configured for subscription"

	cases := []identityImportCase{
		{
			name:     "reservation in the provider's subscription",
			resource: func(*Clients) resource.Resource { return &ReservationResource{} },
			schema:   armids.ReservationIdentitySchema(),
			identity: resID(testSubscriptionID, "res1"),
			wantID:   resID(testSubscriptionID, "res1").FullID(),
		},
		{
			name:     "reservation in another subscription",
			resource: func(*Clients) resource.Resource { return &ReservationResource{} },
			schema:   armids.ReservationIdentitySchema(),
			identity: resID(otherSubscription, "res1"),
			wantErr:  mismatch,
		},
		{
			name:     "storage pool in the provider's subscription",
			resource: func(*Clients) resource.Resource { return &StoragePoolResource{} },
			schema:   armids.StoragePoolIdentitySchema(),
			identity: poolID(testSubscriptionID, "pool1"),
			wantID:   poolID(testSubscriptionID, "pool1").FullID(),
		},
		{
			name:     "storage pool in another subscription",
			resource: func(*Clients) resource.Resource { return &StoragePoolResource{} },
			schema:   armids.StoragePoolIdentitySchema(),
			identity: poolID(otherSubscription, "pool1"),
			wantErr:  mismatch,
		},
		{
			name:     "storage pool name carrying a path separator",
			resource: func(*Clients) resource.Resource { return &StoragePoolResource{} },
			schema:   armids.StoragePoolIdentitySchema(),
			identity: poolID(testSubscriptionID, "pool1/volumeGroups/x"),
			wantErr:  "does not describe a valid resource",
		},
		{
			name:     "volume group in the provider's subscription",
			resource: func(*Clients) resource.Resource { return &VolumeGroupResource{} },
			schema:   armids.VolumeGroupIdentitySchema(),
			identity: vgID(testSubscriptionID),
			wantID:   vgID(testSubscriptionID).FullID(),
		},
		{
			name:     "volume group in another subscription",
			resource: func(*Clients) resource.Resource { return &VolumeGroupResource{} },
			schema:   armids.VolumeGroupIdentitySchema(),
			identity: vgID(otherSubscription),
			wantErr:  mismatch,
		},
		{
			name:     "volume in the provider's subscription",
			resource: func(*Clients) resource.Resource { return &VolumeResource{} },
			schema:   armids.VolumeIdentitySchema(),
			identity: volID(testSubscriptionID),
			wantID:   volID(testSubscriptionID).FullID(),
		},
		{
			name:     "volume in another subscription",
			resource: func(*Clients) resource.Resource { return &VolumeResource{} },
			schema:   armids.VolumeIdentitySchema(),
			identity: volID(otherSubscription),
			wantErr:  mismatch,
		},
		{
			name:     "snapshot in the provider's subscription",
			resource: func(*Clients) resource.Resource { return &SnapshotResource{} },
			schema:   armids.VolumeGroupSnapshotIdentitySchema(),
			identity: snapID(testSubscriptionID),
			wantID:   snapID(testSubscriptionID).FullID(),
		},
		{
			name:     "snapshot in another subscription",
			resource: func(*Clients) resource.Resource { return &SnapshotResource{} },
			schema:   armids.VolumeGroupSnapshotIdentitySchema(),
			identity: snapID(otherSubscription),
			wantErr:  mismatch,
		},
		{
			name: "snapshot created by a snapshot schedule",
			resource: func(clients *Clients) resource.Resource {
				store := newFakeSnapshotStore(testSubscriptionID)
				store.seedSnapshot(testResourceGroup, "pool1", "vg1", "snap1", time.Now().UTC(), true)
				srv := store.newVolumeGroupSnapshotsServer()
				clients.opts = &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: psbfake.NewVolumeGroupSnapshotsServerTransport(&srv)}}
				return &SnapshotResource{}
			},
			schema:   armids.VolumeGroupSnapshotIdentitySchema(),
			identity: snapID(testSubscriptionID),
			wantErr:  "created by the volume group's snapshot schedule",
		},
		{
			name:     "virtual machine connection in the provider's subscription",
			resource: func(*Clients) resource.Resource { return &VirtualMachineConnectionResource{} },
			schema:   armids.VirtualMachineExtensionIdentitySchema(),
			identity: extID(testSubscriptionID),
			wantID:   extID(testSubscriptionID).FullID(),
		},
		{
			name:     "virtual machine connection in another subscription is accepted",
			resource: func(*Clients) resource.Resource { return &VirtualMachineConnectionResource{} },
			schema:   armids.VirtualMachineExtensionIdentitySchema(),
			identity: extID(otherSubscription),
			wantID:   extID(otherSubscription).FullID(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clients := &Clients{cred: &azcorefake.TokenCredential{}, subscriptionID: testSubscriptionID}
			r := tc.resource(clients)
			configurable, ok := r.(resource.ResourceWithConfigure)
			if !ok {
				t.Fatalf("%T does not implement ResourceWithConfigure", r)
			}
			configurable.Configure(ctx, resource.ConfigureRequest{ProviderData: clients}, &resource.ConfigureResponse{})

			var schemaResp resource.SchemaResponse
			r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)

			req := resource.ImportStateRequest{Identity: nullIdentity(t, tc.schema)}
			if diags := req.Identity.Set(ctx, tc.identity); diags.HasError() {
				t.Fatalf("unexpected diagnostics: %v", diags)
			}
			resp := &resource.ImportStateResponse{
				State: tfsdk.State{
					Schema: schemaResp.Schema,
					Raw:    tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), nil),
				},
				Identity: nullIdentity(t, tc.schema),
			}

			importer, ok := r.(resource.ResourceWithImportState)
			if !ok {
				t.Fatalf("%T does not implement ResourceWithImportState", r)
			}
			importer.ImportState(ctx, req, resp)

			if tc.wantErr != "" {
				if !resp.Diagnostics.HasError() {
					t.Fatal("expected an error")
				}
				requireContains(t, resp.Diagnostics[0].Detail(), tc.wantErr)
				return
			}
			if resp.Diagnostics.HasError() {
				t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
			}

			var gotID string
			if diags := resp.State.GetAttribute(ctx, path.Root("id"), &gotID); diags.HasError() {
				t.Fatalf("unexpected diagnostics: %v", diags)
			}
			if gotID != tc.wantID {
				t.Fatalf("imported id = %q, want %q", gotID, tc.wantID)
			}
			if resp.Identity.Raw.IsFullyNull() {
				t.Fatal("expected the response to carry an identity")
			}
		})
	}

	t.Run("neither an ID nor an identity", func(t *testing.T) {
		clients := &Clients{cred: &azcorefake.TokenCredential{}, subscriptionID: testSubscriptionID}
		r := &StoragePoolResource{}
		r.Configure(ctx, resource.ConfigureRequest{ProviderData: clients}, &resource.ConfigureResponse{})
		resp := &resource.ImportStateResponse{}
		r.ImportState(ctx, resource.ImportStateRequest{}, resp)
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an error")
		}
	})
}

// TestReadTaintedStateWithNullID covers the state a create leaves behind when it
// is accepted but the poll then fails: Terraform stores the unknown id as null.
// Read must still refresh it and produce an identity, whether the object exists
// or is gone. The fakes cannot fail a create poll, so this calls Read directly.
func TestReadTaintedStateWithNullID(t *testing.T) {
	ctx := context.Background()

	poolOpts := func() (*fakeStoragePoolStore, *arm.ClientOptions) {
		store := newFakeStoragePoolStore(testSubscriptionID)
		srv := store.newStoragePoolsServer()
		return store, &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: psbfake.NewStoragePoolsServerTransport(&srv)}}
	}
	volumeOpts := func() (*fakeVolumeStore, *arm.ClientOptions) {
		store := newFakeVolumeStore(testSubscriptionID)
		srv := store.newVolumesServer()
		return store, &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: psbfake.NewVolumesServerTransport(&srv)}}
	}

	snapshotOpts := func() (*fakeSnapshotStore, *arm.ClientOptions) {
		store := newFakeSnapshotStore(testSubscriptionID)
		srv := store.newVolumeGroupSnapshotsServer()
		return store, &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: psbfake.NewVolumeGroupSnapshotsServerTransport(&srv)}}
	}

	type target struct {
		// goneWhenSeeded marks a target whose seeded object reads as absent (a destroyed
		// twin), so only the seeded variant runs and it must end up removed from state.
		goneWhenSeeded bool
		name           string
		schema         identityschema.Schema
		want           any
		newVal         func() any
		make           func(t *testing.T, seed bool) (resource.Resource, map[string]string)
	}

	targets := []target{
		{
			name:   "storage pool",
			schema: armids.StoragePoolIdentitySchema(),
			want:   armids.StoragePoolID{ResourceGroupID: armids.ResourceGroupID{SubscriptionID: testSubscriptionID, ResourceGroupName: testResourceGroup}, StoragePoolName: "pool1"},
			newVal: func() any { return &armids.StoragePoolID{} },
			make: func(t *testing.T, seed bool) (resource.Resource, map[string]string) {
				store, opts := poolOpts()
				if seed {
					createPoolDirect(t, ctx, newTestStoragePoolsClient(t, store), "pool1", testSubnetID("snet1"))
				}
				r := &StoragePoolResource{}
				r.Configure(ctx, resource.ConfigureRequest{ProviderData: &Clients{
					cred: &azcorefake.TokenCredential{}, opts: opts, subscriptionID: testSubscriptionID,
				}}, &resource.ConfigureResponse{})
				return r, map[string]string{"name": "pool1", "resource_group_name": testResourceGroup}
			},
		},
		{
			name:   "volume group",
			schema: armids.VolumeGroupIdentitySchema(),
			want: armids.VolumeGroupID{
				ResourceGroupID: armids.ResourceGroupID{SubscriptionID: testSubscriptionID, ResourceGroupName: testResourceGroup},
				StoragePoolName: testStoragePoolName, VolumeGroupName: "vg1",
			},
			newVal: func() any { return &armids.VolumeGroupID{} },
			make: func(t *testing.T, seed bool) (resource.Resource, map[string]string) {
				_, vgStore, opts := newTestVolumeGroupTransport()
				if seed {
					vgStore.seedVolumeGroup(testResourceGroup, testStoragePoolName, "vg1", "PT1H", "P1D")
				}
				r := &VolumeGroupResource{}
				r.Configure(ctx, resource.ConfigureRequest{ProviderData: &Clients{
					cred: &azcorefake.TokenCredential{}, opts: opts, subscriptionID: testSubscriptionID,
				}}, &resource.ConfigureResponse{})
				return r, map[string]string{"name": "vg1", "storage_pool_id": testStoragePoolIDForVG()}
			},
		},
		{
			name:   "volume",
			schema: armids.VolumeIdentitySchema(),
			want: armids.VolumeID{
				ResourceGroupID: armids.ResourceGroupID{SubscriptionID: testSubscriptionID, ResourceGroupName: testResourceGroup},
				StoragePoolName: testStoragePoolName, VolumeGroupName: "vg1", VolumeName: "vol1",
			},
			newVal: func() any { return &armids.VolumeID{} },
			make: func(t *testing.T, seed bool) (resource.Resource, map[string]string) {
				store, opts := volumeOpts()
				if seed {
					store.seedVolume(testResourceGroup, testStoragePoolName, "vg1", "vol1", int64(bytesPerGiB))
				}
				r := &VolumeResource{}
				r.Configure(ctx, resource.ConfigureRequest{ProviderData: &Clients{
					cred: &azcorefake.TokenCredential{}, opts: opts, subscriptionID: testSubscriptionID,
				}}, &resource.ConfigureResponse{})
				return r, map[string]string{"name": "vol1", "volume_group_id": testVolumeGroupID("vg1")}
			},
		},
		{
			name:   "snapshot",
			schema: armids.VolumeGroupSnapshotIdentitySchema(),
			want: armids.VolumeGroupSnapshotID{
				ResourceGroupID: armids.ResourceGroupID{SubscriptionID: testSubscriptionID, ResourceGroupName: testResourceGroup},
				StoragePoolName: testStoragePoolName, VolumeGroupName: "vg1", SnapshotName: "snap1",
			},
			newVal: func() any { return &armids.VolumeGroupSnapshotID{} },
			make: func(t *testing.T, seed bool) (resource.Resource, map[string]string) {
				store, opts := snapshotOpts()
				if seed {
					store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap1", time.Now().UTC(), false)
				}
				r := &SnapshotResource{}
				r.Configure(ctx, resource.ConfigureRequest{ProviderData: &Clients{
					cred: &azcorefake.TokenCredential{}, opts: opts, subscriptionID: testSubscriptionID,
				}}, &resource.ConfigureResponse{})
				return r, map[string]string{"name": "snap1", "volume_group_id": testVolumeGroupID("vg1")}
			},
		},
		{
			name:           "snapshot destroyed twin",
			goneWhenSeeded: true,
			schema:         armids.VolumeGroupSnapshotIdentitySchema(),
			want: armids.VolumeGroupSnapshotID{
				ResourceGroupID: armids.ResourceGroupID{SubscriptionID: testSubscriptionID, ResourceGroupName: testResourceGroup},
				StoragePoolName: testStoragePoolName, VolumeGroupName: "vg1", SnapshotName: "snap1",
			},
			newVal: func() any { return &armids.VolumeGroupSnapshotID{} },
			make: func(t *testing.T, seed bool) (resource.Resource, map[string]string) {
				store, opts := snapshotOpts()
				if seed {
					// A GET by the configured name lands on a destroyed copy of it.
					store.seedDestroyedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "snap1", "snap1", time.Now().UTC(), false)
				}
				r := &SnapshotResource{}
				r.Configure(ctx, resource.ConfigureRequest{ProviderData: &Clients{
					cred: &azcorefake.TokenCredential{}, opts: opts, subscriptionID: testSubscriptionID,
				}}, &resource.ConfigureResponse{})
				return r, map[string]string{"name": "snap1", "volume_group_id": testVolumeGroupID("vg1")}
			},
		},
	}

	for _, tc := range targets {
		for _, seed := range []bool{true, false} {
			if tc.goneWhenSeeded && !seed {
				continue
			}
			name := tc.name + " that exists"
			if !seed {
				name = tc.name + " that is gone"
			}
			if tc.goneWhenSeeded {
				name = tc.name
			}
			t.Run(name, func(t *testing.T) {
				r, attrs := tc.make(t, seed)

				var schemaResp resource.SchemaResponse
				r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
				state := tfsdk.State{
					Schema: schemaResp.Schema,
					Raw:    tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), nil),
				}
				for k, v := range attrs {
					if diags := state.SetAttribute(ctx, path.Root(k), v); diags.HasError() {
						t.Fatalf("unexpected diagnostics: %v", diags)
					}
				}

				resp := &resource.ReadResponse{State: state, Identity: nullIdentity(t, tc.schema)}
				r.Read(ctx, resource.ReadRequest{State: state, Identity: nullIdentity(t, tc.schema)}, resp)
				if resp.Diagnostics.HasError() {
					t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
				}

				got := tc.newVal()
				if diags := resp.Identity.Get(ctx, got); diags.HasError() {
					t.Fatalf("unexpected diagnostics reading identity: %v", diags)
				}
				if g := reflect.ValueOf(got).Elem().Interface(); g != tc.want {
					t.Fatalf("identity = %+v, want %+v", g, tc.want)
				}

				removed := resp.State.Raw.IsNull()
				if seed && !tc.goneWhenSeeded {
					if removed {
						t.Fatal("expected the resource to stay in state")
					}
					var idVal armids.IDValue
					if diags := resp.State.GetAttribute(ctx, path.Root("id"), &idVal); diags.HasError() || idVal.ValueString() == "" {
						t.Fatalf("expected Read to fill in id, got %v (diagnostics %v)", idVal, diags)
					}
				} else if !removed {
					t.Fatal("expected the resource to be removed from state")
				}
			})
		}
	}
}
