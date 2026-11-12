// SPDX-License-Identifier: Apache-2.0

package armids

import (
	"errors"
	"strings"
	"testing"
)

// requireInvalidID fails unless err is an *InvalidIDError for an ID of the given noun.
func requireInvalidID(t *testing.T, err error, noun string) {
	t.Helper()
	var invalid *InvalidIDError
	if !errors.As(err, &invalid) || invalid.Noun != noun {
		t.Fatalf("error = %v, want an *InvalidIDError for a %s ID", err, noun)
	}
}

func TestParseStoragePoolID(t *testing.T) {
	const validID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1"

	t.Run("parses a valid ID", func(t *testing.T) {
		got, err := ParseStoragePoolID(validID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.StoragePoolName != "pool1" {
			t.Fatalf("StoragePoolName = %q, want %q", got.StoragePoolName, "pool1")
		}
		if got.ResourceGroupName != "rg" {
			t.Fatalf("ResourceGroupName = %q, want %q", got.ResourceGroupName, "rg")
		}
		if got.SubscriptionID != "11111111-1111-1111-1111-111111111111" {
			t.Fatalf("SubscriptionID = %q, want %q", got.SubscriptionID, "11111111-1111-1111-1111-111111111111")
		}
	})

	t.Run("is case-insensitive on namespace and resource type", func(t *testing.T) {
		id := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/purestorage.block/storagepools/pool1"
		got, err := ParseStoragePoolID(id)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.StoragePoolName != "pool1" {
			t.Fatalf("StoragePoolName = %q, want %q", got.StoragePoolName, "pool1")
		}
	})

	t.Run("rejects a malformed ID", func(t *testing.T) {
		_, err := ParseStoragePoolID("not-an-id")
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
	})

	t.Run("rejects the wrong resource type", func(t *testing.T) {
		id := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/volumeGroups/vg1"
		_, err := ParseStoragePoolID(id)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		requireInvalidID(t, err, "storage pool")
	})
}

func TestStoragePoolIDCompose(t *testing.T) {
	got := StoragePoolID{
		ResourceGroupID: ResourceGroupID{
			SubscriptionID:    "11111111-1111-1111-1111-111111111111",
			ResourceGroupName: "rg",
		},
		StoragePoolName: "pool1",
	}.FullID()
	want := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1"
	if got != want {
		t.Fatalf("StoragePoolID.FullID() = %q, want %q", got, want)
	}
}

// TestStoragePoolIDRoundTrip asserts ParseStoragePoolID(x.FullID()) == x
// for a StoragePoolID built from all-distinct, non-interchangeable component
// values — so a swapped pair (e.g. subscription and resource group) would
// actually fail this test rather than coincidentally pass it.
func TestStoragePoolIDRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		id   StoragePoolID
	}{
		{
			name: "distinct components",
			id: StoragePoolID{
				ResourceGroupID: ResourceGroupID{
					SubscriptionID:    "11111111-1111-1111-1111-111111111111",
					ResourceGroupName: "rg1",
				},
				StoragePoolName: "pool1",
			},
		},
		{
			name: "mixed-case components",
			id: StoragePoolID{
				ResourceGroupID: ResourceGroupID{
					SubscriptionID:    "22222222-2222-2222-2222-222222222222",
					ResourceGroupName: "RG-Two",
				},
				StoragePoolName: "Pool-Two",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseStoragePoolID(tt.id.FullID())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.id {
				t.Fatalf("ParseStoragePoolID(x.FullID()) = %+v, want %+v", got, tt.id)
			}
		})
	}
}

func TestParseVolumeGroupID(t *testing.T) {
	const validID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg1"

	t.Run("parses a valid ID", func(t *testing.T) {
		got, err := ParseVolumeGroupID(validID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.VolumeGroupName != "vg1" || got.StoragePoolName != "pool1" || got.ResourceGroupName != "rg" ||
			got.SubscriptionID != "11111111-1111-1111-1111-111111111111" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("is case-insensitive on namespace and resource type", func(t *testing.T) {
		id := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/purestorage.block/storagepools/pool1/volumegroups/vg1"
		got, err := ParseVolumeGroupID(id)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.VolumeGroupName != "vg1" || got.StoragePoolName != "pool1" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("rejects a malformed ID", func(t *testing.T) {
		if _, err := ParseVolumeGroupID("not-an-id"); err == nil {
			t.Fatal("expected an error, got nil")
		}
	})

	t.Run("rejects the wrong resource type", func(t *testing.T) {
		id := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1"
		_, err := ParseVolumeGroupID(id)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		requireInvalidID(t, err, "volume group")
	})
}

func TestVolumeGroupIDCompose(t *testing.T) {
	got := VolumeGroupID{
		ResourceGroupID: ResourceGroupID{
			SubscriptionID:    "11111111-1111-1111-1111-111111111111",
			ResourceGroupName: "rg",
		},
		StoragePoolName: "pool1",
		VolumeGroupName: "vg1",
	}.FullID()
	want := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg1"
	if got != want {
		t.Fatalf("VolumeGroupID.FullID() = %q, want %q", got, want)
	}
}

// TestVolumeGroupIDRoundTrip asserts ParseVolumeGroupID(x.FullID()) == x
// for a VolumeGroupID built from all-distinct, non-interchangeable component
// values — so a swapped pair (e.g. resource group and storage pool name)
// would actually fail this test rather than coincidentally pass it.
func TestVolumeGroupIDRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		id   VolumeGroupID
	}{
		{
			name: "distinct components",
			id: VolumeGroupID{
				ResourceGroupID: ResourceGroupID{
					SubscriptionID:    "11111111-1111-1111-1111-111111111111",
					ResourceGroupName: "rg1",
				},
				StoragePoolName: "pool1",
				VolumeGroupName: "vg1",
			},
		},
		{
			name: "mixed-case components",
			id: VolumeGroupID{
				ResourceGroupID: ResourceGroupID{
					SubscriptionID:    "22222222-2222-2222-2222-222222222222",
					ResourceGroupName: "RG-Two",
				},
				StoragePoolName: "Pool-Two",
				VolumeGroupName: "VG-Two",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseVolumeGroupID(tt.id.FullID())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.id {
				t.Fatalf("ParseVolumeGroupID(x.FullID()) = %+v, want %+v", got, tt.id)
			}
		})
	}
}

// TestVolumeIDRoundTrip asserts ParseVolumeID(x.FullID()) == x for a
// VolumeID built from all-distinct, non-interchangeable component values —
// so a swapped pair (e.g. storage pool and volume group name) would actually
// fail this test rather than coincidentally pass it.
func TestVolumeIDRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		id   VolumeID
	}{
		{
			name: "distinct components",
			id: VolumeID{
				ResourceGroupID: ResourceGroupID{
					SubscriptionID:    "11111111-1111-1111-1111-111111111111",
					ResourceGroupName: "rg1",
				},
				StoragePoolName: "pool1",
				VolumeGroupName: "vg1",
				VolumeName:      "vol1",
			},
		},
		{
			name: "mixed-case components",
			id: VolumeID{
				ResourceGroupID: ResourceGroupID{
					SubscriptionID:    "22222222-2222-2222-2222-222222222222",
					ResourceGroupName: "RG-Two",
				},
				StoragePoolName: "Pool-Two",
				VolumeGroupName: "VG-Two",
				VolumeName:      "Vol-Two",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseVolumeID(tt.id.FullID())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.id {
				t.Fatalf("ParseVolumeID(x.FullID()) = %+v, want %+v", got, tt.id)
			}
		})
	}
}

func TestParseVolumeID(t *testing.T) {
	const validID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg1/volumes/vol1"

	t.Run("parses a valid ID", func(t *testing.T) {
		got, err := ParseVolumeID(validID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.VolumeName != "vol1" || got.VolumeGroupName != "vg1" || got.StoragePoolName != "pool1" || got.ResourceGroupName != "rg" {
			t.Fatalf("got %+v, want Name=vol1 VolumeGroupName=vg1 StoragePoolName=pool1 ResourceGroupName=rg", got)
		}
	})

	t.Run("is case-insensitive on namespace and resource type", func(t *testing.T) {
		id := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/purestorage.block/storagepools/pool1/volumegroups/vg1/volumes/vol1"
		got, err := ParseVolumeID(id)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.VolumeName != "vol1" || got.VolumeGroupName != "vg1" || got.StoragePoolName != "pool1" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("rejects a malformed ID", func(t *testing.T) {
		if _, err := ParseVolumeID("not-an-id"); err == nil {
			t.Fatal("expected an error, got nil")
		}
	})

	t.Run("rejects the wrong resource type", func(t *testing.T) {
		id := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg1"
		_, err := ParseVolumeID(id)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		requireInvalidID(t, err, "volume")
	})

	t.Run("rejects an ID missing a parent level (only the storage pool)", func(t *testing.T) {
		id := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1"
		_, err := ParseVolumeID(id)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		requireInvalidID(t, err, "volume")
	})
}

func TestParseVirtualMachineID(t *testing.T) {
	const validID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1"

	t.Run("parses a valid ID", func(t *testing.T) {
		got, err := ParseVirtualMachineID(validID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.VirtualMachineName != "vm1" || got.ResourceGroupName != "rg" ||
			got.SubscriptionID != "11111111-1111-1111-1111-111111111111" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("is case-insensitive on namespace and resource type", func(t *testing.T) {
		id := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/microsoft.compute/virtualmachines/vm1"
		got, err := ParseVirtualMachineID(id)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.VirtualMachineName != "vm1" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("rejects a malformed ID", func(t *testing.T) {
		if _, err := ParseVirtualMachineID("not-an-id"); err == nil {
			t.Fatal("expected an error, got nil")
		}
	})

	t.Run("rejects the wrong resource type", func(t *testing.T) {
		id := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1"
		_, err := ParseVirtualMachineID(id)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		requireInvalidID(t, err, "virtual machine")
	})

	t.Run("rejects a VM's own extension ID", func(t *testing.T) {
		id := validID + "/extensions/PureStorageMountIscsi"
		_, err := ParseVirtualMachineID(id)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		requireInvalidID(t, err, "virtual machine")
	})
}

func TestVirtualMachineIDCompose(t *testing.T) {
	got := VirtualMachineID{
		ResourceGroupID: ResourceGroupID{
			SubscriptionID:    "11111111-1111-1111-1111-111111111111",
			ResourceGroupName: "rg",
		},
		VirtualMachineName: "vm1",
	}.FullID()
	want := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1"
	if got != want {
		t.Fatalf("VirtualMachineID.FullID() = %q, want %q", got, want)
	}
}

func TestVirtualMachineIDRoundTrip(t *testing.T) {
	id := VirtualMachineID{
		ResourceGroupID: ResourceGroupID{
			SubscriptionID:    "22222222-2222-2222-2222-222222222222",
			ResourceGroupName: "RG-Two",
		},
		VirtualMachineName: "VM-Two",
	}
	got, err := ParseVirtualMachineID(id.FullID())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != id {
		t.Fatalf("ParseVirtualMachineID(x.FullID()) = %+v, want %+v", got, id)
	}
}

func TestParseVirtualMachineExtensionID(t *testing.T) {
	const validID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1/extensions/ext1"

	t.Run("parses a valid ID", func(t *testing.T) {
		got, err := ParseVirtualMachineExtensionID(validID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ExtensionName != "ext1" || got.VirtualMachineName != "vm1" || got.ResourceGroupName != "rg" ||
			got.SubscriptionID != "11111111-1111-1111-1111-111111111111" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("is case-insensitive on namespace and resource type", func(t *testing.T) {
		id := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/microsoft.compute/virtualmachines/vm1/extensions/ext1"
		got, err := ParseVirtualMachineExtensionID(id)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ExtensionName != "ext1" || got.VirtualMachineName != "vm1" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("rejects a malformed ID", func(t *testing.T) {
		if _, err := ParseVirtualMachineExtensionID("not-an-id"); err == nil {
			t.Fatal("expected an error, got nil")
		}
	})

	t.Run("rejects the wrong resource type", func(t *testing.T) {
		id := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1"
		_, err := ParseVirtualMachineExtensionID(id)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		requireInvalidID(t, err, "virtual machine extension")
	})
}

func TestVirtualMachineExtensionIDCompose(t *testing.T) {
	got := VirtualMachineExtensionID{
		ResourceGroupID: ResourceGroupID{
			SubscriptionID:    "11111111-1111-1111-1111-111111111111",
			ResourceGroupName: "rg",
		},
		VirtualMachineName: "vm1",
		ExtensionName:      "ext1",
	}.FullID()
	want := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1/extensions/ext1"
	if got != want {
		t.Fatalf("VirtualMachineExtensionID.FullID() = %q, want %q", got, want)
	}
}

func TestVirtualMachineExtensionIDRoundTrip(t *testing.T) {
	id := VirtualMachineExtensionID{
		ResourceGroupID: ResourceGroupID{
			SubscriptionID:    "22222222-2222-2222-2222-222222222222",
			ResourceGroupName: "RG-Two",
		},
		VirtualMachineName: "VM-Two",
		ExtensionName:      "Ext-Two",
	}
	got, err := ParseVirtualMachineExtensionID(id.FullID())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != id {
		t.Fatalf("ParseVirtualMachineExtensionID(x.FullID()) = %+v, want %+v", got, id)
	}

	t.Run("VirtualMachine derives the parent VM ID", func(t *testing.T) {
		vm := id.VirtualMachine()
		want := VirtualMachineID{ResourceGroupID: id.ResourceGroupID, VirtualMachineName: id.VirtualMachineName}
		if vm != want {
			t.Fatalf("VirtualMachine() = %+v, want %+v", vm, want)
		}
	})
}

func TestParseVolumeGroupSnapshotID(t *testing.T) {
	const validID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg1/snapshots/snap1"

	t.Run("parses a valid ID", func(t *testing.T) {
		got, err := ParseVolumeGroupSnapshotID(validID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := VolumeGroupSnapshotID{
			ResourceGroupID: ResourceGroupID{SubscriptionID: "11111111-1111-1111-1111-111111111111", ResourceGroupName: "rg"},
			StoragePoolName: "pool1",
			VolumeGroupName: "vg1",
			SnapshotName:    "snap1",
		}
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("round-trips through FullID and exposes its volume group", func(t *testing.T) {
		parsed, err := ParseVolumeGroupSnapshotID(strings.ToLower(validID))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := parsed.FullID(); got != validID {
			t.Fatalf("FullID() = %q, want the canonical spelling %q", got, validID)
		}
		wantVG := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg1"
		if got := parsed.VolumeGroup().FullID(); got != wantVG {
			t.Fatalf("VolumeGroup().FullID() = %q, want %q", got, wantVG)
		}
	})

	t.Run("rejects the wrong resource type", func(t *testing.T) {
		id := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg1/volumes/vol1"
		_, err := ParseVolumeGroupSnapshotID(id)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		requireInvalidID(t, err, "volume group snapshot")
	})
}

func TestParseReservationID(t *testing.T) {
	const validID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/reservations/res1"

	t.Run("parses a valid ID and rebuilds it", func(t *testing.T) {
		got, err := ParseReservationID(validID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ReservationName != "res1" || got.ResourceGroupName != "rg" ||
			got.SubscriptionID != "11111111-1111-1111-1111-111111111111" {
			t.Fatalf("got %+v", got)
		}
		if got.FullID() != validID {
			t.Fatalf("FullID() = %q, want %q", got.FullID(), validID)
		}
	})

	t.Run("is case-insensitive on namespace and resource type", func(t *testing.T) {
		got, err := ParseReservationID("/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/purestorage.block/Reservations/res1")
		if err != nil || got.ReservationName != "res1" {
			t.Fatalf("got %+v, err %v", got, err)
		}
	})

	t.Run("rejects the wrong resource type", func(t *testing.T) {
		_, err := ParseReservationID("/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1")
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		requireInvalidID(t, err, "reservation")
	})
}
