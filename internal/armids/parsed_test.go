// SPDX-License-Identifier: Apache-2.0

package armids

import (
	"errors"
	"testing"
)

// TestParsedIDParentID replaces the old TestParentResourceIDKindSubnet,
// TestParentResourceIDKindVolumeGroupSnapshot and TestParentResourceIDKindVolume:
// ParentID() cuts the last "/<type>/<name>" segment pair off a parsed ID to
// yield its parent's ID, the same behaviour parentResourceID(id, kind) used to
// provide, now reached via typ.Parse(id) instead of a hand-picked armIDKind.
func TestParsedIDParentID(t *testing.T) {
	t.Run("a subnet ID to its virtual network ID", func(t *testing.T) {
		p, err := TypeSubnet.Parse(
			"/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/" +
				"Microsoft.Network/virtualNetworks/vnet1/subnets/snet1",
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, err := p.ParentID()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/" +
			"Microsoft.Network/virtualNetworks/vnet1"
		if got != want {
			t.Fatalf("ParentID() = %q, want %q", got, want)
		}
	})

	t.Run("is case-insensitive and preserves the caller's casing", func(t *testing.T) {
		p, err := TypeSubnet.Parse(
			"/Subscriptions/00000000-0000-0000-0000-000000000000/ResourceGroups/rg/Providers/" +
				"Microsoft.Network/VirtualNetworks/vnet1/SUBNETS/snet1",
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, err := p.ParentID()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "/Subscriptions/00000000-0000-0000-0000-000000000000/ResourceGroups/rg/Providers/" +
			"Microsoft.Network/VirtualNetworks/vnet1"
		if got != want {
			t.Fatalf("ParentID() = %q, want %q", got, want)
		}
	})

	t.Run("a volume ID to its volume group ID", func(t *testing.T) {
		p, err := TypeVolume.Parse(
			"/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/" +
				"PureStorage.Block/storagePools/pool1/volumeGroups/vg1/volumes/vol1",
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, err := p.ParentID()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/" +
			"PureStorage.Block/storagePools/pool1/volumeGroups/vg1"
		if got != want {
			t.Fatalf("ParentID() = %q, want %q", got, want)
		}
	})

	t.Run("preserves the original casing of the remaining prefix", func(t *testing.T) {
		id := "/Subscriptions/11111111-1111-1111-1111-111111111111/ResourceGroups/RG/providers/" +
			"PureStorage.Block/StoragePools/Pool1/VolumeGroups/VG1/Volumes/Vol1"
		p, err := TypeVolume.Parse(id)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, err := p.ParentID()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "/Subscriptions/11111111-1111-1111-1111-111111111111/ResourceGroups/RG/providers/" +
			"PureStorage.Block/StoragePools/Pool1/VolumeGroups/VG1"
		if got != want {
			t.Fatalf("ParentID() = %q, want %q", got, want)
		}
	})

	t.Run("a snapshot ID to its volume group ID", func(t *testing.T) {
		const snapshotID = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/" +
			"PureStorage.Block/storagePools/pool1/volumeGroups/vg1/snapshots/snap1"
		p, err := TypeVolumeGroupSnapshot.Parse(snapshotID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, err := p.ParentID()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/" +
			"PureStorage.Block/storagePools/pool1/volumeGroups/vg1"
		if got != want {
			t.Fatalf("ParentID() = %q, want %q", got, want)
		}
	})

	t.Run("a storage pool ID has no parent", func(t *testing.T) {
		p, err := TypeStoragePool.Parse(
			"/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1",
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_, err = p.ParentID()
		var noParent *NoParentError
		if !errors.As(err, &noParent) {
			t.Fatalf("ParentID() error = %v, want a *NoParentError", err)
		}
		if noParent.Noun != "storage pool" || noParent.Input != p.raw {
			t.Fatalf("NoParentError = %+v, want the storage pool ID and noun", noParent)
		}
	})

	t.Run("a malformed ID never parses", func(t *testing.T) {
		_, err := TypeVolume.Parse("not-an-id")
		var invalid *InvalidIDError
		if !errors.As(err, &invalid) {
			t.Fatalf("Parse() error = %v, want an *InvalidIDError", err)
		}
		if invalid.Input != "not-an-id" || invalid.Noun != "volume" || invalid.WantFormat != TypeVolume.exampleID() {
			t.Fatalf("InvalidIDError = %+v, want the input, noun and example of a volume ID", invalid)
		}
	})
}

// TestParsedIDAncestorID covers cutting an ID down to one of its ancestors.
// Callers always know which ResourceType they hold and pass it explicitly, so
// an ID of the wrong type is rejected rather than cut to a well-formed but
// wrong ancestor. Most reach this through AncestorIDOf below, which adds the
// parse step; the cases here hold the parse and the cut apart so a failure
// points at whichever of the two is broken.
func TestParsedIDAncestorID(t *testing.T) {
	const wantPoolID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1"

	t.Run("a volume ID's storage pool ancestor", func(t *testing.T) {
		p, err := TypeVolume.Parse(wantPoolID + "/volumeGroups/vg1/volumes/vol1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, err := p.AncestorID(&TypeStoragePool)
		if err != nil || got != wantPoolID {
			t.Fatalf("AncestorID() = (%q, %v), want (%q, nil)", got, err, wantPoolID)
		}
	})

	t.Run("a snapshot ID's storage pool ancestor", func(t *testing.T) {
		p, err := TypeVolumeGroupSnapshot.Parse(wantPoolID + "/volumeGroups/vg1/snapshots/snap1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, err := p.AncestorID(&TypeStoragePool)
		if err != nil || got != wantPoolID {
			t.Fatalf("AncestorID() = (%q, %v), want (%q, nil)", got, err, wantPoolID)
		}
	})

	t.Run("a volume group ID's storage pool ancestor", func(t *testing.T) {
		p, err := TypeVolumeGroup.Parse(wantPoolID + "/volumeGroups/vg1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, err := p.AncestorID(&TypeStoragePool)
		if err != nil || got != wantPoolID {
			t.Fatalf("AncestorID() = (%q, %v), want (%q, nil)", got, err, wantPoolID)
		}
	})

	t.Run("preserves the original casing", func(t *testing.T) {
		id := "/Subscriptions/11111111-1111-1111-1111-111111111111/ResourceGroups/RG/providers/PureStorage.Block/StoragePools/Pool1/VolumeGroups/VG1"
		want := "/Subscriptions/11111111-1111-1111-1111-111111111111/ResourceGroups/RG/providers/PureStorage.Block/StoragePools/Pool1"
		p, err := TypeVolumeGroup.Parse(id)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, err := p.AncestorID(&TypeStoragePool)
		if err != nil || got != want {
			t.Fatalf("AncestorID() = (%q, %v), want (%q, nil)", got, err, want)
		}
	})

	t.Run("an unrelated type has no storage pool ancestor", func(t *testing.T) {
		p, err := TypeSubnet.Parse(
			"/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet1/subnets/snet1",
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_, err = p.AncestorID(&TypeStoragePool)
		var notNested *NotNestedError
		if !errors.As(err, &notNested) {
			t.Fatalf("AncestorID() error = %v, want a *NotNestedError", err)
		}
		if notNested.Noun != "subnet" || notNested.WantNoun != "storage pool" || notNested.Input != p.raw {
			t.Fatalf("NotNestedError = %+v, want the subnet ID, its noun and the storage pool noun", notNested)
		}
	})
}

// TestResourceTypeAncestorIDOf and TestResourceTypeParentIDOf cover the
// parse-and-cut shorthands. Both are thin, so these check the two things
// folding the steps together could get wrong: that a parse failure still
// surfaces as an error, and that the cut is the same one the two-step form
// makes above.
func TestResourceTypeAncestorIDOf(t *testing.T) {
	const wantPoolID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1"

	t.Run("a volume ID's storage pool ancestor", func(t *testing.T) {
		got, err := TypeVolume.AncestorIDOf(wantPoolID+"/volumeGroups/vg1/volumes/vol1", &TypeStoragePool)
		if err != nil || got != wantPoolID {
			t.Fatalf("AncestorIDOf() = (%q, %v), want (%q, nil)", got, err, wantPoolID)
		}
	})

	t.Run("a malformed ID is an error, not a silent miss", func(t *testing.T) {
		if _, err := TypeVolume.AncestorIDOf("not-an-id", &TypeStoragePool); err == nil {
			t.Fatal("expected an error, got nil")
		}
	})

	t.Run("an ID of the wrong type is an error", func(t *testing.T) {
		vgID := wantPoolID + "/volumeGroups/vg1"
		if _, err := TypeVolume.AncestorIDOf(vgID, &TypeStoragePool); err == nil {
			t.Fatal("expected an error, got nil")
		}
	})
}

func TestResourceTypeParentIDOf(t *testing.T) {
	const wantVolumeGroupID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg1"

	t.Run("a volume ID's volume group parent", func(t *testing.T) {
		got, err := TypeVolume.ParentIDOf(wantVolumeGroupID + "/volumes/vol1")
		if err != nil || got != wantVolumeGroupID {
			t.Fatalf("ParentIDOf() = (%q, %v), want (%q, nil)", got, err, wantVolumeGroupID)
		}
	})

	t.Run("a snapshot ID's volume group parent", func(t *testing.T) {
		got, err := TypeVolumeGroupSnapshot.ParentIDOf(wantVolumeGroupID + "/snapshots/snap1")
		if err != nil || got != wantVolumeGroupID {
			t.Fatalf("ParentIDOf() = (%q, %v), want (%q, nil)", got, err, wantVolumeGroupID)
		}
	})

	t.Run("a malformed ID is an error, not a silent miss", func(t *testing.T) {
		if _, err := TypeVolume.ParentIDOf("not-an-id"); err == nil {
			t.Fatal("expected an error, got nil")
		}
	})

	t.Run("a chain root has no parent to cut", func(t *testing.T) {
		poolID := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1"
		if _, err := TypeStoragePool.ParentIDOf(poolID); err == nil {
			t.Fatal("expected an error, got nil")
		}
	})
}
