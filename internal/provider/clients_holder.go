// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// clientsHolder supplies the resource.ResourceWithConfigure implementation and the
// per-service client getters every resource in this provider needs. Embed it (as an
// anonymous field) in a resource struct to get Configure, storagePoolsClient,
// volumeGroupsClient, recoverableVolumeGroupsClient, volumesClient and
// volumeGroupSnapshotsClient for free via method and field promotion — a future
// resource adds zero copies of this boilerplate.
//
// clients is kept for its subscriptionID (ImportState checks it against an import ID
// before any client call) and for apiFailure, which needs this run's correlation ID.
// Individual service clients (StoragePoolsClient, etc.) aren't cached here: they're
// cheap to build, so each call gets its own via the getters below
// rather than one shared, mutable field per service.
type clientsHolder struct {
	clients *Clients
}

func (h *clientsHolder) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	h.configure(req.ProviderData, &resp.Diagnostics)
}

// configure is Configure's provider-data-type-agnostic body, shared with
// dataSourceClientsHolder.Configure below: a resource.ConfigureRequest and a
// datasource.ConfigureRequest carry the same ProviderData (the *Clients set in
// EverpureAzureProvider.Configure) under otherwise-unrelated request types, so this is
// the one place that type-asserts it.
func (h *clientsHolder) configure(providerData any, diags *diag.Diagnostics) {
	if providerData == nil {
		return
	}
	clients, ok := providerData.(*Clients)
	if !ok {
		diags.Append(providerInternalDiagnostic(newProviderBugError(fmt.Sprintf("expected *Clients, got %T", providerData))))
		return
	}

	h.clients = clients
}

// dataSourceClientsHolder adapts clientsHolder to datasource.DataSourceWithConfigure:
// its own Configure shadows the embedded clientsHolder's resource-typed one (Go
// resolves the shallower method on an ambiguous embedding), while every client getter
// (storagePoolsClient, volumeGroupsClient, volumesClient, …) is promoted unchanged.
// Embed it (as an anonymous field) in a data source struct the same way resources embed
// clientsHolder itself.
type dataSourceClientsHolder struct {
	clientsHolder
}

func (h *dataSourceClientsHolder) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	h.configure(req.ProviderData, &resp.Diagnostics)
}

// configured reports whether Configure has handed this resource, data source,
// ephemeral resource or action its *Clients, appending the provider-not-configured
// error when it hasn't. h.clients stays nil when the provider deferred its own Configure
// (a provider config value was still unknown on the last call); every getter below
// checks this first, so that case degrades to a diagnostic instead of a nil-pointer
// panic on the h.clients method call.
func (h *clientsHolder) configured(diags *diag.Diagnostics) bool {
	if h.clients != nil {
		return true
	}
	diags.Append(operationConfigureProvider.fail(newProviderNotConfiguredError()))
	return false
}

// buildClient is the shared body of the client getters below; a free function because
// Go methods can't have type parameters.
func buildClient[T any](h *clientsHolder, diags *diag.Diagnostics, k kind, build func(*Clients) (*T, error)) (*T, bool) {
	if !h.configured(diags) {
		return nil, false
	}
	client, err := build(h.clients)
	if err != nil {
		diags.Append(operationConfigureProvider.fail(newClientCreationFailedError(k, h.clients.apiFailure(err))))
		return nil, false
	}
	return client, true
}

func (h *clientsHolder) storagePoolsClient(diags *diag.Diagnostics) (*armpurestorageblock.StoragePoolsClient, bool) {
	return buildClient(h, diags, kindStoragePools, (*Clients).StoragePools)
}

func (h *clientsHolder) reservationsClient(diags *diag.Diagnostics) (*armpurestorageblock.ReservationsClient, bool) {
	return buildClient(h, diags, kindReservations, (*Clients).Reservations)
}

func (h *clientsHolder) volumeGroupsClient(diags *diag.Diagnostics) (*armpurestorageblock.VolumeGroupsClient, bool) {
	return buildClient(h, diags, kindVolumeGroups, (*Clients).VolumeGroups)
}

func (h *clientsHolder) recoverableVolumeGroupsClient(diags *diag.Diagnostics) (*armpurestorageblock.RecoverableVolumeGroupsClient, bool) {
	return buildClient(h, diags, kindRecoverableVolumeGroups, (*Clients).RecoverableVolumeGroups)
}

func (h *clientsHolder) volumesClient(diags *diag.Diagnostics) (*armpurestorageblock.VolumesClient, bool) {
	return buildClient(h, diags, kindVolumes, (*Clients).Volumes)
}

func (h *clientsHolder) volumeGroupSnapshotsClient(diags *diag.Diagnostics) (*armpurestorageblock.VolumeGroupSnapshotsClient, bool) {
	return buildClient(h, diags, kindSnapshots, (*Clients).VolumeGroupSnapshots)
}

// virtualMachinesClient and virtualMachineExtensionsClient are the Microsoft.Compute
// counterparts of the wrappers above, for the VM connection resource: subscriptionID
// is the VM's own (parsed from its full ARM ID), not h.clients.subscriptionID, since
// the VM may live in a different subscription than the one this provider instance is
// configured for.
func (h *clientsHolder) virtualMachinesClient(subscriptionID string, diags *diag.Diagnostics) (*armcompute.VirtualMachinesClient, bool) {
	return buildClient(h, diags, kindVirtualMachines, func(c *Clients) (*armcompute.VirtualMachinesClient, error) {
		return c.virtualMachines(subscriptionID)
	})
}

func (h *clientsHolder) virtualMachineExtensionsClient(subscriptionID string, diags *diag.Diagnostics) (*armcompute.VirtualMachineExtensionsClient, bool) {
	return buildClient(h, diags, kindVirtualMachineExtensions, func(c *Clients) (*armcompute.VirtualMachineExtensionsClient, error) {
		return c.virtualMachineExtensions(subscriptionID)
	})
}

// ephemeralClientsHolder adapts clientsHolder to ephemeral.EphemeralResourceWithConfigure
// the same way dataSourceClientsHolder does for data sources: its own Configure shadows
// the embedded resource-typed one, and every client getter is promoted unchanged.
type ephemeralClientsHolder struct {
	clientsHolder
}

func (h *ephemeralClientsHolder) Configure(_ context.Context, req ephemeral.ConfigureRequest, resp *ephemeral.ConfigureResponse) {
	h.configure(req.ProviderData, &resp.Diagnostics)
}

// actionClientsHolder adapts clientsHolder to action.ActionWithConfigure the same way
// dataSourceClientsHolder does for data sources: its own Configure shadows the embedded
// resource-typed one, and every client getter is promoted unchanged.
type actionClientsHolder struct {
	clientsHolder
}

func (h *actionClientsHolder) Configure(_ context.Context, req action.ConfigureRequest, resp *action.ConfigureResponse) {
	h.configure(req.ProviderData, &resp.Diagnostics)
}
