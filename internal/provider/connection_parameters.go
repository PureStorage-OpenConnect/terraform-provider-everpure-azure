// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"slices"
	"sort"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

// iscsiTarget is one resolved iSCSI portal endpoint from a volume group's connection
// parameters, with json tags matching the extension's settings schema.
type iscsiTarget struct {
	IP   string `json:"ip"`
	Port int32  `json:"port"`
	IQN  string `json:"iqn"`
}

// listVolumeGroupConnectionEndpoints calls ListConnectionParameters for one volume
// group and returns its iSCSI endpoints. It returns the API's own error unchanged;
// each caller parses vg's ID and checks its subscription itself before calling this,
// and classifies the error itself after.
func listVolumeGroupConnectionEndpoints(ctx context.Context, client *armpurestorageblock.VolumeGroupsClient, vg armids.VolumeGroupID) ([]iscsiTarget, error) {
	resp, err := client.ListConnectionParameters(ctx, vg.ResourceGroupName, vg.StoragePoolName, vg.VolumeGroupName, nil)
	if err != nil {
		return nil, err
	}

	var endpoints []iscsiTarget
	if resp.Iscsi != nil {
		for _, e := range resp.Iscsi.Endpoints {
			if e == nil {
				continue
			}
			var t iscsiTarget
			if e.Iqn != nil {
				t.IQN = *e.Iqn
			}
			if e.IP != nil {
				t.IP = *e.IP
			}
			if e.Port != nil {
				t.Port = *e.Port
			}
			endpoints = append(endpoints, t)
		}
	}
	return endpoints, nil
}

// sortISCSITargets sorts targets in place by (iqn, ip, port). Every endpoint of one
// volume group shares one IQN, so across a single group's own endpoints this is
// effectively (ip, port); the IQN comparison only matters once targets from more than
// one group are combined, keeping one group's targets from interleaving with another's.
func sortISCSITargets(targets []iscsiTarget) {
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].IQN != targets[j].IQN {
			return targets[i].IQN < targets[j].IQN
		}
		if targets[i].IP != targets[j].IP {
			return targets[i].IP < targets[j].IP
		}
		return targets[i].Port < targets[j].Port
	})
}

// resolveVMExtensionTargets resolves vgIDs into the sorted, de-duplicated iSCSI targets
// sent to the VM extension. Volume groups on one pool share portal IPs, so targets are
// keyed by (iqn, ip, port). op is the operation the targets are resolved for.
func resolveVMExtensionTargets(ctx context.Context, clients *clientsHolder, op operation, vgIDs []string) ([]iscsiTarget, diag.Diagnostics) {
	var diags diag.Diagnostics

	client, ok := clients.volumeGroupsClient(&diags)
	if !ok {
		return nil, diags
	}

	var all []iscsiTarget
	for _, vgID := range vgIDs {
		vg, idErr := parseIDInSubscription(armids.ParseVolumeGroupID, vgID, kindVolumeGroup, clients.clients.subscriptionID)
		if idErr != nil {
			diags.Append(diag.WithPath(path.Root("volume_group_ids"), op.fail(*idErr)))
			continue
		}

		endpoints, err := listVolumeGroupConnectionEndpoints(ctx, client, vg)
		if err != nil {
			if isNotFound(err) {
				diags.Append(diag.WithPath(path.Root("volume_group_ids"), op.fail(newNotFoundError(kindVolumeGroup, vgID, nil, clients.clients.apiFailure(err)))))
				continue
			}
			failedStep := step{action: actionList, kind: kindVolumeGroupConnectionParameters, id: vgID}
			diags.Append(op.fail(newApiStepFailedError(op, failedStep, clients.clients.apiFailure(err))))
			continue
		}

		if len(endpoints) == 0 {
			diags.Append(diag.WithPath(path.Root("volume_group_ids"), op.fail(newNoConnectionEndpointsError(vgID))))
			continue
		}
		all = append(all, endpoints...)
	}
	if diags.HasError() {
		return nil, diags
	}

	sortISCSITargets(all)
	return slices.Compact(all), diags
}
