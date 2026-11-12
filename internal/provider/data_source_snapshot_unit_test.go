// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

func TestSnapshotLookupNamePattern(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"snap1", true},
		{"17", true},
		{"9", true},
		{"a", true},
		{"17-20260827141136", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{"", false},
		{"-snap", false},
		{"snap-", false},
		{"snap_1", false},
		{"snap.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := snapshotLookupNamePattern.MatchString(tt.name); got != tt.want {
				t.Errorf("MatchString(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}

	t.Run("wider than the resource's create rule: digit-leading names are accepted only by the data source", func(t *testing.T) {
		if storageObjectNamePattern.MatchString("17") || !snapshotLookupNamePattern.MatchString("17") {
			t.Fatal("expected \"17\" to be rejected by the resource's rule and accepted by the data source's")
		}
	})
}

func TestSoftDeletedSnapshot(t *testing.T) {
	vg, err := armids.ParseVolumeGroupID("/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool/volumeGroups/vg")
	if err != nil {
		t.Fatalf("parsing volume group ID: %v", err)
	}
	const name, previous, id = "snap1-20260930120000", "snap1", "/id/snap1-20260930120000"
	eradicatedAt := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	const eradicatedAtText = "2026-10-30T00:00:00Z"

	tests := []struct {
		name            string
		lookup          string
		id              *string
		createdByPolicy bool
		softDeletion    *armpurestorageblock.DestroyedStateProperties
		want            detailError
	}{
		{
			name:   "names the previous name and the recoverable ID, with the eradication timestamp when present",
			lookup: name, id: to.Ptr(id),
			softDeletion: &armpurestorageblock.DestroyedStateProperties{PreviousName: to.Ptr(previous), EradicationTimestamp: to.Ptr(eradicatedAt)},
			want:         newSnapshotSoftDeletedError(name, previous, id, eradicatedAtText, false),
		},
		{
			name:   "omits the eradication timestamp when absent",
			lookup: name, id: to.Ptr(id),
			softDeletion: &armpurestorageblock.DestroyedStateProperties{PreviousName: to.Ptr(previous)},
			want:         newSnapshotSoftDeletedError(name, previous, id, "", false),
		},
		{
			name:   "falls back to the looked-up name and a composed ID when PreviousName or the ID is absent",
			lookup: name, softDeletion: &armpurestorageblock.DestroyedStateProperties{},
			want: newSnapshotSoftDeletedError(name, name, vg.FullID()+"/snapshots/"+name, "", false),
		},
		{
			name:   "a snapshot created by the schedule is not offered for Terraform recovery",
			lookup: "17-20260930120000", id: to.Ptr("/id"), createdByPolicy: true,
			softDeletion: &armpurestorageblock.DestroyedStateProperties{PreviousName: to.Ptr("17")},
			want:         newSnapshotSoftDeletedError("17-20260930120000", "17", "/id", "", true),
		},
		{
			name:   "a snapshot created by the schedule keeps the eradication timestamp",
			lookup: "17-20260930120000", id: to.Ptr("/id"), createdByPolicy: true,
			softDeletion: &armpurestorageblock.DestroyedStateProperties{PreviousName: to.Ptr("17"), EradicationTimestamp: to.Ptr(eradicatedAt)},
			want:         newSnapshotSoftDeletedError("17-20260930120000", "17", "/id", eradicatedAtText, true),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := softDeletedSnapshotCause(vg, tt.lookup, tt.id, tt.createdByPolicy, tt.softDeletion); got != tt.want {
				t.Errorf("softDeletedSnapshotCause() = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("the cause carries the previous name and the recoverable ID", func(t *testing.T) {
		got := newSnapshotSoftDeletedError(name, previous, id, "", false).Error()
		for _, want := range []string{name, `"snap1"`, id} {
			requireContains(t, got, want)
		}
	})
}

func TestSnapshotItemFromSpace(t *testing.T) {
	tests := []struct {
		name  string
		space *armpurestorageblock.Space
	}{
		{name: "present Space", space: &armpurestorageblock.Space{TotalUsed: to.Ptr(int64(10)), Unique: to.Ptr(int64(4)), Snapshots: to.Ptr(int64(3)), Shared: to.Ptr(int64(2))}},
		{name: "nil Space"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item, diags := snapshotItemFrom(armpurestorageblock.VolumeGroupSnapshot{Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{Space: tt.space}})
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

func TestVolumeSnapshotsDataSourceFromAPI(t *testing.T) {
	infos := []*armpurestorageblock.VolumeSnapshotInfo{
		nil,
		{Name: to.Ptr("a"), Space: &armpurestorageblock.Space{TotalUsed: to.Ptr(int64(10)), Unique: to.Ptr(int64(4)), Snapshots: to.Ptr(int64(3)), Shared: to.Ptr(int64(2))}},
		{Name: to.Ptr("b")},
	}
	list, diags := volumeSnapshotsDataSourceFromAPI(t.Context(), infos)
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	elems := list.Elements()
	if len(elems) != 2 {
		t.Fatalf("got %d elements, want 2 (nil entry skipped)", len(elems))
	}
	memberSpace := func(i int) types.Object {
		member, ok := elems[i].(types.Object)
		if !ok {
			t.Fatalf("element %d is %T, want types.Object", i, elems[i])
		}
		space, ok := member.Attributes()["space"].(types.Object)
		if !ok {
			t.Fatalf("element %d space is %T, want types.Object", i, member.Attributes()["space"])
		}
		return space
	}
	first := memberSpace(0).Attributes()
	want := map[string]int64{"total_used_in_bytes": 10, "unique_in_bytes": 4, "snapshots_in_bytes": 3, "shared_in_bytes": 2}
	for name, w := range want {
		if got, ok := first[name].(types.Int64); !ok || got.ValueInt64() != w {
			t.Errorf("space.%s = %v, want %d", name, first[name], w)
		}
	}
	if !memberSpace(1).IsNull() {
		t.Error("member without space: want null space")
	}
}
