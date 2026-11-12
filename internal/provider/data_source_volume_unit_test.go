// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

func TestIsVolumeSoftDeleted(t *testing.T) {
	tests := []struct {
		name string
		v    armpurestorageblock.Volume
		want bool
	}{
		{
			name: "destroyed true",
			v: armpurestorageblock.Volume{Properties: &armpurestorageblock.AzureVolumeProperties{
				SoftDeletion: &armpurestorageblock.DestroyedStateProperties{Destroyed: to.Ptr(true)},
			}},
			want: true,
		},
		{
			name: "destroyed false",
			v: armpurestorageblock.Volume{Properties: &armpurestorageblock.AzureVolumeProperties{
				SoftDeletion: &armpurestorageblock.DestroyedStateProperties{Destroyed: to.Ptr(false)},
			}},
			want: false,
		},
		{
			name: "nil SoftDeletion",
			v: armpurestorageblock.Volume{Properties: &armpurestorageblock.AzureVolumeProperties{
				SoftDeletion: nil,
			}},
			want: false,
		},
		{
			name: "nil Destroyed",
			v: armpurestorageblock.Volume{Properties: &armpurestorageblock.AzureVolumeProperties{
				SoftDeletion: &armpurestorageblock.DestroyedStateProperties{Destroyed: nil},
			}},
			want: false,
		},
		{
			name: "nil Properties",
			v:    armpurestorageblock.Volume{Properties: nil},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isVolumeSoftDeleted(tt.v); got != tt.want {
				t.Errorf("isVolumeSoftDeleted() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSoftDeletedVolume(t *testing.T) {
	vg, err := armids.ParseVolumeGroupID("/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool/volumeGroups/vg")
	if err != nil {
		t.Fatalf("parsing volume group ID: %v", err)
	}
	const name, previous, id = "vol1-20260930120000", "vol1", "/id/vol1-20260930120000"
	eradicatedAt := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		id           *string
		softDeletion *armpurestorageblock.DestroyedStateProperties
		want         detailError
	}{
		{
			name:         "names the previous name and the recoverable ID, with the eradication timestamp when present",
			id:           to.Ptr(id),
			softDeletion: &armpurestorageblock.DestroyedStateProperties{PreviousName: to.Ptr(previous), EradicationTimestamp: to.Ptr(eradicatedAt)},
			want:         newVolumeSoftDeletedError(name, previous, id, "2026-10-30T00:00:00Z"),
		},
		{
			name:         "omits the eradication timestamp when absent",
			id:           to.Ptr(id),
			softDeletion: &armpurestorageblock.DestroyedStateProperties{PreviousName: to.Ptr(previous)},
			want:         newVolumeSoftDeletedError(name, previous, id, ""),
		},
		{
			name:         "falls back to the looked-up name and a composed ID when PreviousName or the ID is absent",
			softDeletion: &armpurestorageblock.DestroyedStateProperties{},
			want:         newVolumeSoftDeletedError(name, name, vg.FullID()+"/volumes/"+name, ""),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := softDeletedVolumeCause(vg, name, tt.id, tt.softDeletion); got != tt.want {
				t.Errorf("softDeletedVolumeCause() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVolumeItemFromSpace(t *testing.T) {
	tests := []struct {
		name  string
		space *armpurestorageblock.Space
	}{
		{name: "present Space", space: &armpurestorageblock.Space{TotalUsed: to.Ptr(int64(10)), Unique: to.Ptr(int64(4)), Snapshots: to.Ptr(int64(3)), Shared: to.Ptr(int64(2))}},
		{name: "nil Space"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item, diags := volumeItemFrom(armpurestorageblock.Volume{Properties: &armpurestorageblock.AzureVolumeProperties{Space: tt.space}})
			if diags.HasError() {
				t.Fatalf("unexpected diagnostics: %v", diags)
			}
			want, _ := spaceFromAPI(tt.space)
			if !item.Space.Equal(want) {
				t.Errorf("Space = %v, want %v", item.Space, want)
			}
		})
	}
}
