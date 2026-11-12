// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
)

func TestApplyVolumeGroupStatus(t *testing.T) {
	t.Run("maps every field", func(t *testing.T) {
		var data VolumeGroupStatusDataSourceModel
		status := armpurestorageblock.VolumeGroupStatus{
			ConnectedHostCount: to.Ptr(int32(3)),
			Space:              &armpurestorageblock.Space{TotalUsed: to.Ptr(int64(10)), Unique: to.Ptr(int64(4)), Snapshots: to.Ptr(int64(3)), Shared: to.Ptr(int64(3))},
		}
		if diags := applyVolumeGroupStatus(&data, status); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}

		if got := data.MaxConnectedHostsPerVolume.ValueInt64(); got != 3 {
			t.Errorf("max_connected_hosts_per_volume = %d, want 3", got)
		}
		space := data.Space.Attributes()
		for name, want := range map[string]int64{
			"total_used_in_bytes": 10, "unique_in_bytes": 4, "snapshots_in_bytes": 3, "shared_in_bytes": 3,
		} {
			if got := int64Attr(t, space, name); got != want {
				t.Errorf("space.%s = %d, want %d", name, got, want)
			}
		}
	})

	t.Run("nil space maps to a null object", func(t *testing.T) {
		var data VolumeGroupStatusDataSourceModel
		status := armpurestorageblock.VolumeGroupStatus{ConnectedHostCount: to.Ptr(int32(0))}
		if diags := applyVolumeGroupStatus(&data, status); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if !data.Space.IsNull() {
			t.Errorf("space = %v, want null", data.Space)
		}
		if data.MaxConnectedHostsPerVolume.IsNull() || data.MaxConnectedHostsPerVolume.ValueInt64() != 0 {
			t.Errorf("max_connected_hosts_per_volume = %v, want 0", data.MaxConnectedHostsPerVolume)
		}
	})

	t.Run("nil host count maps to null", func(t *testing.T) {
		var data VolumeGroupStatusDataSourceModel
		status := armpurestorageblock.VolumeGroupStatus{Space: &armpurestorageblock.Space{TotalUsed: to.Ptr(int64(1))}}
		if diags := applyVolumeGroupStatus(&data, status); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if !data.MaxConnectedHostsPerVolume.IsNull() {
			t.Errorf("max_connected_hosts_per_volume = %v, want null", data.MaxConnectedHostsPerVolume)
		}
		if data.Space.IsNull() {
			t.Errorf("space is null, want the present object")
		}
	})
}
