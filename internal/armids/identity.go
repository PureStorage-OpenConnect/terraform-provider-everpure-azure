// SPDX-License-Identifier: Apache-2.0

package armids

import (
	"github.com/hashicorp/terraform-plugin-framework/resource/identityschema"
)

// --- Resource identity -------------------------------------------------------
//
// Each resource's Terraform identity is its ARM ID split into one string per
// variable segment: the subscription, the resource group, then one name per
// nested resource, the last of which is always called plain "name". The
// constant segments (resourceGroups, providers, the namespace and type
// keywords) carry no information and are rebuilt by FullID().
//
// The ID structs in resources.go are themselves the identity data: their tfsdk
// tags name each variable segment, so importing by identity and importing by
// full ID go through the same type and the same Parse…ID / FullID() grammar
// and cannot disagree. This file holds only the identity schemas.

func identityString(description string) identityschema.StringAttribute {
	return identityschema.StringAttribute{
		RequiredForImport: true,
		Description:       description,
	}
}

const (
	identitySubscriptionDescription  = "Azure subscription ID the resource lives in."
	identityResourceGroupDescription = "Name of the resource group the resource lives in."
)

func ReservationIdentitySchema() identityschema.Schema {
	return identityschema.Schema{Attributes: map[string]identityschema.Attribute{
		"subscription_id":     identityString(identitySubscriptionDescription),
		"resource_group_name": identityString(identityResourceGroupDescription),
		"name":                identityString("Name of the reservation."),
	}}
}

func StoragePoolIdentitySchema() identityschema.Schema {
	return identityschema.Schema{Attributes: map[string]identityschema.Attribute{
		"subscription_id":     identityString(identitySubscriptionDescription),
		"resource_group_name": identityString(identityResourceGroupDescription),
		"name":                identityString("Name of the storage pool."),
	}}
}

func VolumeGroupIdentitySchema() identityschema.Schema {
	return identityschema.Schema{Attributes: map[string]identityschema.Attribute{
		"subscription_id":     identityString(identitySubscriptionDescription),
		"resource_group_name": identityString(identityResourceGroupDescription),
		"storage_pool_name":   identityString("Name of the storage pool the volume group belongs to."),
		"name":                identityString("Name of the volume group."),
	}}
}

func VolumeIdentitySchema() identityschema.Schema {
	return identityschema.Schema{Attributes: map[string]identityschema.Attribute{
		"subscription_id":     identityString(identitySubscriptionDescription),
		"resource_group_name": identityString(identityResourceGroupDescription),
		"storage_pool_name":   identityString("Name of the storage pool the volume belongs to."),
		"volume_group_name":   identityString("Name of the volume group the volume belongs to."),
		"name":                identityString("Name of the volume."),
	}}
}

func VolumeGroupSnapshotIdentitySchema() identityschema.Schema {
	return identityschema.Schema{Attributes: map[string]identityschema.Attribute{
		"subscription_id":     identityString(identitySubscriptionDescription),
		"resource_group_name": identityString(identityResourceGroupDescription),
		"storage_pool_name":   identityString("Name of the storage pool the snapshot belongs to."),
		"volume_group_name":   identityString("Name of the volume group the snapshot belongs to."),
		"name":                identityString("Name of the snapshot."),
	}}
}

func VirtualMachineExtensionIdentitySchema() identityschema.Schema {
	return identityschema.Schema{Attributes: map[string]identityschema.Attribute{
		"subscription_id":      identityString(identitySubscriptionDescription),
		"resource_group_name":  identityString(identityResourceGroupDescription),
		"virtual_machine_name": identityString("Name of the virtual machine the extension is installed on."),
		"name":                 identityString("Name of the virtual machine extension."),
	}}
}
