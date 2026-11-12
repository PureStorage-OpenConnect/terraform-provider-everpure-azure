// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

// recoverableVolumeGroupDataSourceItem is one element of the
// everpure_azure_recoverable_volume_groups data source's recoverable_volume_groups
// list: the same shared attributes as an everpure_azure_volume_groups element, minus
// tags and internal_id (the API doesn't return either for a recoverable volume group),
// plus soft_deletion — every element here is, by definition, a destroyed copy.
type recoverableVolumeGroupDataSourceItem struct {
	ID                       armids.IDValue `tfsdk:"id"`
	Name                     types.String   `tfsdk:"name"`
	StoragePoolID            armids.IDValue `tfsdk:"storage_pool_id"`
	BandwidthLimitInMbps     types.Int64    `tfsdk:"bandwidth_limit_in_mbps"`
	IopsLimit                types.Int64    `tfsdk:"iops_limit"`
	SnapshotProtectionPolicy types.List     `tfsdk:"snapshot_protection_policy"`
	SoftDeletion             types.Object   `tfsdk:"soft_deletion"`
}

// recoverableVolumeGroupItemFrom maps an API response onto a fresh
// recoverableVolumeGroupDataSourceItem. Unlike the pool/volume group/volume data
// sources, there is no existing resource model or apply* function to route through —
// RecoverableVolumeGroup isn't a VolumeGroup and no resource manages it — so this reuses
// only the two field-level helpers applyVolumeGroup itself uses (performanceParametersFromAPI,
// protectionParametersFromAPI), which already take the raw SDK pointers rather than a
// whole VolumeGroup.
func recoverableVolumeGroupItemFrom(ctx context.Context, rvg armpurestorageblock.RecoverableVolumeGroup) (recoverableVolumeGroupDataSourceItem, diag.Diagnostics) {
	var diags diag.Diagnostics

	if rvg.Properties == nil {
		diags.Append(unexpectedResponseDiagnostic(newNoPropertiesError(kindRecoverableVolumeGroup)))
		return recoverableVolumeGroupDataSourceItem{}, diags
	}
	props := rvg.Properties

	bandwidthLimitInMbps, iopsLimit := performanceParametersFromAPI(props.PerformanceParameters)

	policy, d := protectionParametersFromAPI(ctx, props.ProtectionParameters)
	diags.Append(d...)

	softDeletion, d := softDeletionDetailsFromAPI(props.SoftDeletion)
	diags.Append(d...)

	return recoverableVolumeGroupDataSourceItem{
		ID:                       armids.IDValue{StringValue: types.StringPointerValue(rvg.ID)},
		Name:                     types.StringPointerValue(rvg.Name),
		BandwidthLimitInMbps:     bandwidthLimitInMbps,
		IopsLimit:                iopsLimit,
		SnapshotProtectionPolicy: policy,
		SoftDeletion:             softDeletion,
	}, diags
}
