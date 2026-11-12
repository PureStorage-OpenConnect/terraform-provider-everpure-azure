// SPDX-License-Identifier: Apache-2.0

package armids

// Package-level ARM types covering every resource shape this provider parses,
// builds or validates a full ID for. All are matched case-insensitively, since
// ARM itself treats resource IDs case-insensitively. Kept as one block, not
// split per resource, because this table is the file's core data and its
// whole value is showing the nesting at a glance.
var (
	TypeReservation = ResourceType{
		namespace: "PureStorage.Block",
		segment:   "reservations",
		noun:      "reservation",
	}

	// TypeVirtualNetwork has no validator or parser of its own — nothing in this
	// provider accepts a bare virtual network ID — but it must exist as a row so
	// TypeSubnet's chain (and pattern) can compose through it.
	TypeVirtualNetwork = ResourceType{
		namespace: "Microsoft.Network",
		segment:   "virtualNetworks",
		noun:      "virtual network",
	}

	TypeSubnet = ResourceType{
		parent:  &TypeVirtualNetwork,
		segment: "subnets",
		noun:    "subnet",
	}

	TypeStoragePool = ResourceType{
		namespace: "PureStorage.Block",
		segment:   "storagePools",
		noun:      "storage pool",
	}

	TypeVolumeGroup = ResourceType{
		parent:  &TypeStoragePool,
		segment: "volumeGroups",
		noun:    "volume group",
	}

	TypeVolume = ResourceType{
		parent:  &TypeVolumeGroup,
		segment: "volumes",
		noun:    "volume",
	}

	TypeVolumeGroupSnapshot = ResourceType{
		parent:  &TypeVolumeGroup,
		segment: "snapshots",
		noun:    "volume group snapshot",
	}

	TypeRecoverableVolumeGroup = ResourceType{
		parent:  &TypeStoragePool,
		segment: "recoverableVolumeGroups",
		noun:    "recoverable volume group",
	}

	TypeVirtualMachine = ResourceType{
		namespace: "Microsoft.Compute",
		segment:   "virtualMachines",
		noun:      "virtual machine",
	}

	TypeVirtualMachineExtension = ResourceType{
		parent:  &TypeVirtualMachine,
		segment: "extensions",
		noun:    "virtual machine extension",
	}
)

// --- Per-resource ID types -------------------------------------------------
//
// One block per ARM type whose full IDs this provider composes or imports.
// Each block is the same four things, in the same order — the component
// struct, FullID(), a parent accessor for a nested type, and ParseXID:
//
//	type VolumeGroupID struct {
//		ResourceGroupID
//		StoragePoolName string
//		VolumeGroupName string
//	}
//
//	func (vg VolumeGroupID) StoragePool() StoragePoolID { ... }
//	func (vg VolumeGroupID) FullID() string             { ... }
//	func ParseVolumeGroupID(id string) (VolumeGroupID, error)
//
// The parent accessor describes the parent from the child's own fields, which
// is how FullID() composes a nested type's ID one segment at a time.
//
// ParseXID deliberately does two things it would be easy to expect more of.
// It does not check which subscription the ID names — that is authorisation,
// not grammar, and belongs to the caller. And it builds its struct inline
// rather than through a shared parsedID-to-struct helper: such a helper would
// accept any parsedID while being correct for only one type, and would read
// this type's name off whatever leaf it was handed.
//
// These structs double as the Terraform identity data: the framework decodes and
// encodes them through the tfsdk tags (the embedded ResourceGroupID stays
// untagged so its tagged fields are promoted). They must therefore stay plain,
// comparable value types holding only exported, tagged string fields: the
// import round-trip check compares a decoded identity to the re-parsed ID with ==.
//
// Adding a resource type means one more row in the ResourceType table above plus a
// block here. Nothing in resourcetype.go or parsed.go changes.

type ReservationID struct {
	ResourceGroupID
	ReservationName string `tfsdk:"name"`
}

func (r ReservationID) FullID() string {
	return TypeReservation.nestedID(r.prefix(TypeReservation.namespace), r.ReservationName)
}

func ParseReservationID(id string) (ReservationID, error) {
	p, err := TypeReservation.Parse(id)
	if err != nil {
		return ReservationID{}, err
	}
	return ReservationID{
		ResourceGroupID: p.resourceGroupID(),
		ReservationName: p.name(),
	}, nil
}

type StoragePoolID struct {
	ResourceGroupID
	StoragePoolName string `tfsdk:"name"`
}

// VolumeGroup returns the ID of the volume group named name in this pool.
func (p StoragePoolID) VolumeGroup(name string) VolumeGroupID {
	return VolumeGroupID{ResourceGroupID: p.ResourceGroupID, StoragePoolName: p.StoragePoolName, VolumeGroupName: name}
}

func (p StoragePoolID) FullID() string {
	return TypeStoragePool.nestedID(p.prefix(TypeStoragePool.namespace), p.StoragePoolName)
}

func ParseStoragePoolID(id string) (StoragePoolID, error) {
	p, err := TypeStoragePool.Parse(id)
	if err != nil {
		return StoragePoolID{}, err
	}
	return StoragePoolID{
		ResourceGroupID: p.resourceGroupID(),
		StoragePoolName: p.name(),
	}, nil
}

type VolumeGroupID struct {
	ResourceGroupID
	StoragePoolName string `tfsdk:"storage_pool_name"`
	VolumeGroupName string `tfsdk:"name"`
}

// Volume returns the ID of the volume named name in this volume group.
func (vg VolumeGroupID) Volume(name string) VolumeID {
	return VolumeID{
		ResourceGroupID: vg.ResourceGroupID,
		StoragePoolName: vg.StoragePoolName,
		VolumeGroupName: vg.VolumeGroupName,
		VolumeName:      name,
	}
}

// Snapshot returns the ID of the snapshot named name in this volume group.
func (vg VolumeGroupID) Snapshot(name string) VolumeGroupSnapshotID {
	return VolumeGroupSnapshotID{
		ResourceGroupID: vg.ResourceGroupID,
		StoragePoolName: vg.StoragePoolName,
		VolumeGroupName: vg.VolumeGroupName,
		SnapshotName:    name,
	}
}

func (vg VolumeGroupID) StoragePool() StoragePoolID {
	return StoragePoolID{
		ResourceGroupID: vg.ResourceGroupID,
		StoragePoolName: vg.StoragePoolName,
	}
}

func (vg VolumeGroupID) FullID() string {
	return TypeVolumeGroup.nestedID(vg.StoragePool().FullID(), vg.VolumeGroupName)
}

func ParseVolumeGroupID(id string) (VolumeGroupID, error) {
	p, err := TypeVolumeGroup.Parse(id)
	if err != nil {
		return VolumeGroupID{}, err
	}
	return VolumeGroupID{
		ResourceGroupID: p.resourceGroupID(),
		StoragePoolName: p.nameOf(&TypeStoragePool),
		VolumeGroupName: p.name(),
	}, nil
}

type VolumeID struct {
	ResourceGroupID
	StoragePoolName string `tfsdk:"storage_pool_name"`
	VolumeGroupName string `tfsdk:"volume_group_name"`
	VolumeName      string `tfsdk:"name"`
}

func (v VolumeID) VolumeGroup() VolumeGroupID {
	return VolumeGroupID{
		ResourceGroupID: v.ResourceGroupID,
		StoragePoolName: v.StoragePoolName,
		VolumeGroupName: v.VolumeGroupName,
	}
}

func (v VolumeID) FullID() string {
	return TypeVolume.nestedID(v.VolumeGroup().FullID(), v.VolumeName)
}

func ParseVolumeID(id string) (VolumeID, error) {
	p, err := TypeVolume.Parse(id)
	if err != nil {
		return VolumeID{}, err
	}
	return VolumeID{
		ResourceGroupID: p.resourceGroupID(),
		StoragePoolName: p.nameOf(&TypeStoragePool),
		VolumeGroupName: p.nameOf(&TypeVolumeGroup),
		VolumeName:      p.name(),
	}, nil
}

type VolumeGroupSnapshotID struct {
	ResourceGroupID
	StoragePoolName string `tfsdk:"storage_pool_name"`
	VolumeGroupName string `tfsdk:"volume_group_name"`
	SnapshotName    string `tfsdk:"name"`
}

func (s VolumeGroupSnapshotID) VolumeGroup() VolumeGroupID {
	return VolumeGroupID{
		ResourceGroupID: s.ResourceGroupID,
		StoragePoolName: s.StoragePoolName,
		VolumeGroupName: s.VolumeGroupName,
	}
}

func (s VolumeGroupSnapshotID) FullID() string {
	return TypeVolumeGroupSnapshot.nestedID(s.VolumeGroup().FullID(), s.SnapshotName)
}

func ParseVolumeGroupSnapshotID(id string) (VolumeGroupSnapshotID, error) {
	p, err := TypeVolumeGroupSnapshot.Parse(id)
	if err != nil {
		return VolumeGroupSnapshotID{}, err
	}
	return VolumeGroupSnapshotID{
		ResourceGroupID: p.resourceGroupID(),
		StoragePoolName: p.nameOf(&TypeStoragePool),
		VolumeGroupName: p.nameOf(&TypeVolumeGroup),
		SnapshotName:    p.name(),
	}, nil
}

// VirtualMachineID identifies a Microsoft.Compute/virtualMachines resource. Unlike
// every other type in this file, its resources are never created by this provider —
// only referenced by full ID — so it exists here purely as a chain root for
// VirtualMachineExtensionID and for parsing a practitioner-supplied ID.
type VirtualMachineID struct {
	ResourceGroupID
	VirtualMachineName string `tfsdk:"virtual_machine_name"`
}

// Extension returns the ID of the extension named name on this virtual machine.
func (vm VirtualMachineID) Extension(name string) VirtualMachineExtensionID {
	return VirtualMachineExtensionID{ResourceGroupID: vm.ResourceGroupID, VirtualMachineName: vm.VirtualMachineName, ExtensionName: name}
}

func (vm VirtualMachineID) FullID() string {
	return TypeVirtualMachine.nestedID(vm.prefix(TypeVirtualMachine.namespace), vm.VirtualMachineName)
}

func ParseVirtualMachineID(id string) (VirtualMachineID, error) {
	p, err := TypeVirtualMachine.Parse(id)
	if err != nil {
		return VirtualMachineID{}, err
	}
	return VirtualMachineID{
		ResourceGroupID:    p.resourceGroupID(),
		VirtualMachineName: p.name(),
	}, nil
}

// VirtualMachineExtensionID identifies a Microsoft.Compute/virtualMachines/extensions
// resource — the ARM shape of a VM extension instance (e.g. the iSCSI mount
// extension), nested one level under its VM.
type VirtualMachineExtensionID struct {
	ResourceGroupID
	VirtualMachineName string `tfsdk:"virtual_machine_name"`
	// ExtensionName is "name" in the identity, like every other identity's last
	// segment, even though the resource's own attribute for it is extension_name.
	ExtensionName string `tfsdk:"name"`
}

func (e VirtualMachineExtensionID) VirtualMachine() VirtualMachineID {
	return VirtualMachineID{
		ResourceGroupID:    e.ResourceGroupID,
		VirtualMachineName: e.VirtualMachineName,
	}
}

func (e VirtualMachineExtensionID) FullID() string {
	return TypeVirtualMachineExtension.nestedID(e.VirtualMachine().FullID(), e.ExtensionName)
}

func ParseVirtualMachineExtensionID(id string) (VirtualMachineExtensionID, error) {
	p, err := TypeVirtualMachineExtension.Parse(id)
	if err != nil {
		return VirtualMachineExtensionID{}, err
	}
	return VirtualMachineExtensionID{
		ResourceGroupID:    p.resourceGroupID(),
		VirtualMachineName: p.nameOf(&TypeVirtualMachine),
		ExtensionName:      p.name(),
	}, nil
}
