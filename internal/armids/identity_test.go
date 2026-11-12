// SPDX-License-Identifier: Apache-2.0

package armids

import (
	"context"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource/identityschema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func requiredFlags(attrs map[string]identityschema.Attribute) map[string]bool {
	out := make(map[string]bool, len(attrs))
	for name, a := range attrs {
		out[name] = a.IsRequiredForImport()
	}
	return out
}

// identityCase pairs one identity schema with the ID value it decodes to, the
// full ID that value names, and the parser for that full ID.
type identityCase struct {
	name   string
	schema identityschema.Schema
	id     fullIDer
	fullID string
	parse  func(string) (fullIDer, error)
}

const (
	testSub        = "00000000-0000-0000-0000-000000000000"
	testPoolFullID = "/subscriptions/" + testSub + "/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1"

	testReservationFullID = "/subscriptions/" + testSub + "/resourceGroups/rg/providers/PureStorage.Block/reservations/res1"
)

// fullIDer is what every ID struct offers; tests hold IDs as this interface so
// FullID() needs no type assertion, while == still compares dynamic type and value.
type fullIDer interface{ FullID() string }

func identityCases() []identityCase {
	rg := ResourceGroupID{SubscriptionID: testSub, ResourceGroupName: "rg"}
	return []identityCase{
		{
			name:   "reservation",
			schema: ReservationIdentitySchema(),
			id:     ReservationID{ResourceGroupID: rg, ReservationName: "res1"},
			fullID: testReservationFullID,
			parse:  func(s string) (fullIDer, error) { return ParseReservationID(s) },
		},
		{
			name:   "storage pool",
			schema: StoragePoolIdentitySchema(),
			id:     StoragePoolID{ResourceGroupID: rg, StoragePoolName: "pool1"},
			fullID: testPoolFullID,
			parse:  func(s string) (fullIDer, error) { return ParseStoragePoolID(s) },
		},
		{
			name:   "volume group",
			schema: VolumeGroupIdentitySchema(),
			id:     VolumeGroupID{ResourceGroupID: rg, StoragePoolName: "pool1", VolumeGroupName: "vg1"},
			fullID: testPoolFullID + "/volumeGroups/vg1",
			parse:  func(s string) (fullIDer, error) { return ParseVolumeGroupID(s) },
		},
		{
			name:   "volume",
			schema: VolumeIdentitySchema(),
			id:     VolumeID{ResourceGroupID: rg, StoragePoolName: "pool1", VolumeGroupName: "vg1", VolumeName: "vol1"},
			fullID: testPoolFullID + "/volumeGroups/vg1/volumes/vol1",
			parse:  func(s string) (fullIDer, error) { return ParseVolumeID(s) },
		},
		{
			name:   "volume group snapshot",
			schema: VolumeGroupSnapshotIdentitySchema(),
			id:     VolumeGroupSnapshotID{ResourceGroupID: rg, StoragePoolName: "pool1", VolumeGroupName: "vg1", SnapshotName: "snap1"},
			fullID: testPoolFullID + "/volumeGroups/vg1/snapshots/snap1",
			parse:  func(s string) (fullIDer, error) { return ParseVolumeGroupSnapshotID(s) },
		},
		{
			name:   "virtual machine extension",
			schema: VirtualMachineExtensionIdentitySchema(),
			id:     VirtualMachineExtensionID{ResourceGroupID: rg, VirtualMachineName: "vm1", ExtensionName: "ext1"},
			fullID: "/subscriptions/" + testSub + "/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1/extensions/ext1",
			parse:  func(s string) (fullIDer, error) { return ParseVirtualMachineExtensionID(s) },
		},
	}
}

// TestIdentitySchemaMatchesIDStruct guards drift between each identity schema and
// the tfsdk tags on the ID struct decoded from it: the framework rejects a
// struct whose tags and schema attributes differ, in either direction.
func TestIdentitySchemaMatchesIDStruct(t *testing.T) {
	ctx := context.Background()
	for _, tc := range identityCases() {
		t.Run(tc.name, func(t *testing.T) {
			identity := &tfsdk.ResourceIdentity{
				Schema: tc.schema,
				Raw:    tftypes.NewValue(tc.schema.Type().TerraformType(ctx), nil),
			}
			if diags := identity.Set(ctx, tc.id); diags.HasError() {
				t.Fatalf("Set: %v", diags)
			}
			got := reflect.New(reflect.TypeOf(tc.id))
			if diags := identity.Get(ctx, got.Interface()); diags.HasError() {
				t.Fatalf("Get: %v", diags)
			}
			if g := got.Elem().Interface(); g != tc.id {
				t.Fatalf("round trip = %+v, want %+v", g, tc.id)
			}
		})
	}
}

func TestIdentityIDGrammar(t *testing.T) {
	t.Run("parsing a full ID yields the identity struct and FullID rebuilds it", func(t *testing.T) {
		for _, tc := range identityCases() {
			parsed, err := tc.parse(tc.fullID)
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.name, err)
			}
			if parsed != tc.id {
				t.Fatalf("%s: parsed = %+v, want %+v", tc.name, parsed, tc.id)
			}
			if got := parsed.FullID(); got != tc.fullID {
				t.Fatalf("%s: FullID() = %q, want %q", tc.name, got, tc.fullID)
			}
		}
	})

	t.Run("an ID of the wrong type is rejected", func(t *testing.T) {
		if _, err := ParseVolumeID(testPoolFullID); err == nil {
			t.Fatal("expected an error parsing a storage pool ID as a volume")
		}
	})

	t.Run("identity schemas mark every attribute required for import", func(t *testing.T) {
		for _, tc := range identityCases() {
			for attr, required := range requiredFlags(tc.schema.Attributes) {
				if !required {
					t.Errorf("%s: attribute %q is not RequiredForImport", tc.name, attr)
				}
			}
		}
	})
}

// TestChildIDConstructors checks each parent-to-child constructor against the
// parser: the ID built from a parsed parent must render the expected full ID, and
// parsing that full ID must give back exactly the struct that was built.
func TestChildIDConstructors(t *testing.T) {
	const vmID = "/subscriptions/" + testSub + "/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1"
	pool, err := ParseStoragePoolID(testPoolFullID)
	if err != nil {
		t.Fatal(err)
	}
	vg, err := ParseVolumeGroupID(testPoolFullID + "/volumeGroups/vg1")
	if err != nil {
		t.Fatal(err)
	}
	vm, err := ParseVirtualMachineID(vmID)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		built fullIDer
		want  string
		parse func(string) (fullIDer, error)
	}{
		{
			"volume group of a pool", pool.VolumeGroup("vg2"), testPoolFullID + "/volumeGroups/vg2",
			func(s string) (fullIDer, error) { return ParseVolumeGroupID(s) },
		},
		{
			"volume of a volume group", vg.Volume("vol2"), testPoolFullID + "/volumeGroups/vg1/volumes/vol2",
			func(s string) (fullIDer, error) { return ParseVolumeID(s) },
		},
		{
			"snapshot of a volume group", vg.Snapshot("snap2"), testPoolFullID + "/volumeGroups/vg1/snapshots/snap2",
			func(s string) (fullIDer, error) { return ParseVolumeGroupSnapshotID(s) },
		},
		{
			"extension of a virtual machine", vm.Extension("ext2"), vmID + "/extensions/ext2",
			func(s string) (fullIDer, error) { return ParseVirtualMachineExtensionID(s) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.built.FullID(); got != tc.want {
				t.Fatalf("FullID() = %q, want %q", got, tc.want)
			}
			parsed, err := tc.parse(tc.want)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if parsed != tc.built {
				t.Fatalf("parsed = %+v, want %+v", parsed, tc.built)
			}
		})
	}
}
