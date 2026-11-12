// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestSpaceFromAPI(t *testing.T) {
	tests := []struct {
		name string
		in   *armpurestorageblock.Space
		want types.Object
	}{
		{
			name: "nil Space is a null object",
			in:   nil,
			want: types.ObjectNull(spaceObjectType.AttrTypes),
		},
		{
			name: "full Space maps each field to its own attribute",
			in:   &armpurestorageblock.Space{TotalUsed: to.Ptr(int64(10)), Unique: to.Ptr(int64(4)), Snapshots: to.Ptr(int64(3)), Shared: to.Ptr(int64(2))},
			want: types.ObjectValueMust(spaceObjectType.AttrTypes, map[string]attr.Value{
				"total_used_in_bytes": types.Int64Value(10),
				"unique_in_bytes":     types.Int64Value(4),
				"snapshots_in_bytes":  types.Int64Value(3),
				"shared_in_bytes":     types.Int64Value(2),
			}),
		},
		{
			name: "nil fields inside a present Space are null attributes",
			in:   &armpurestorageblock.Space{TotalUsed: to.Ptr(int64(10)), Shared: to.Ptr(int64(2))},
			want: types.ObjectValueMust(spaceObjectType.AttrTypes, map[string]attr.Value{
				"total_used_in_bytes": types.Int64Value(10),
				"unique_in_bytes":     types.Int64Null(),
				"snapshots_in_bytes":  types.Int64Null(),
				"shared_in_bytes":     types.Int64Value(2),
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, diags := spaceFromAPI(tt.in)
			if diags.HasError() {
				t.Fatalf("unexpected diagnostics: %v", diags)
			}
			if !got.Equal(tt.want) {
				t.Errorf("spaceFromAPI = %v, want %v", got, tt.want)
			}
		})
	}
}
