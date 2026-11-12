// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
)

func TestFilterAndSortVolumes(t *testing.T) {
	idFor := func(name string) string {
		return "/subscriptions/sub/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool/volumeGroups/vg/volumes/" + name
	}
	live := func(name string) *armpurestorageblock.Volume {
		return &armpurestorageblock.Volume{
			ID:   to.Ptr(idFor(name)),
			Name: to.Ptr(name),
			Properties: &armpurestorageblock.AzureVolumeProperties{
				SoftDeletion: nil,
			},
		}
	}
	destroyed := func(name string) *armpurestorageblock.Volume {
		return &armpurestorageblock.Volume{
			ID:   to.Ptr(idFor(name)),
			Name: to.Ptr(name),
			Properties: &armpurestorageblock.AzureVolumeProperties{
				SoftDeletion: &armpurestorageblock.DestroyedStateProperties{Destroyed: to.Ptr(true)},
			},
		}
	}
	noSoftDeletionField := &armpurestorageblock.Volume{
		ID:         to.Ptr(idFor("nodel")),
		Name:       to.Ptr("nodel"),
		Properties: &armpurestorageblock.AzureVolumeProperties{},
	}

	t.Run("excludes destroyed entries by default", func(t *testing.T) {
		got := filterAndSortVolumes([]*armpurestorageblock.Volume{live("b"), destroyed("a"), noSoftDeletionField}, false)
		if len(got) != 2 {
			t.Fatalf("expected 2 entries, got %d: %v", len(got), got)
		}
		if *got[0].Name != "b" && *got[0].Name != "nodel" {
			t.Errorf("unexpected first entry %q", *got[0].Name)
		}
	})

	t.Run("includes destroyed entries when asked, sorted by id", func(t *testing.T) {
		got := filterAndSortVolumes([]*armpurestorageblock.Volume{live("b"), destroyed("a"), noSoftDeletionField}, true)
		if len(got) != 3 {
			t.Fatalf("expected 3 entries, got %d", len(got))
		}
		var names []string
		for _, v := range got {
			names = append(names, *v.Name)
		}
		want := []string{"a", "b", "nodel"}
		for i, name := range names {
			if name != want[i] {
				t.Errorf("names = %v, want %v", names, want)
				break
			}
		}
	})

	t.Run("sorts case-insensitively by id", func(t *testing.T) {
		got := filterAndSortVolumes([]*armpurestorageblock.Volume{live("Vol-B"), live("vol-a")}, true)
		if len(got) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(got))
		}
		if *got[0].Name != "vol-a" || *got[1].Name != "Vol-B" {
			t.Errorf("got order [%q, %q], want [vol-a, Vol-B]", *got[0].Name, *got[1].Name)
		}
	})

	t.Run("drops nil entries", func(t *testing.T) {
		got := filterAndSortVolumes([]*armpurestorageblock.Volume{nil, live("a")}, true)
		if len(got) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(got))
		}
	})

	t.Run("drops an entry with no id, keeps one with no properties even when excluding destroyed", func(t *testing.T) {
		noID := live("noid")
		noID.ID = nil
		noProperties := &armpurestorageblock.Volume{ID: to.Ptr(idFor("noprops")), Name: to.Ptr("noprops")}
		got := filterAndSortVolumes([]*armpurestorageblock.Volume{noID, noProperties, destroyed("a")}, false)
		if len(got) != 1 || *got[0].Name != "noprops" {
			t.Fatalf("expected only noprops, got %v", got)
		}
	})
}
