// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
)

// movedPrivateKey is the private-state key a move leaves for the first Read, for a
// resource that has a check only Read can make. The move cannot make it: Terraform runs
// the move on a provider that was never configured, so there are no API clients.
const movedPrivateKey = "moved"

// moveSource is one kind of resource a moved block can take over: the resource type name
// and the provider that owns it, as namespace and type of the provider address.
type moveSource struct {
	typeName          string
	providerNamespace string
	providerType      string
}

var (
	azapiResourceSource = moveSource{typeName: "azapi_resource", providerNamespace: "azure", providerType: "azapi"}
	// azurermVirtualMachineExtensionSource is the only azurerm resource a move accepts:
	// azurerm has no PureStorage.Block resources.
	azurermVirtualMachineExtensionSource = moveSource{
		typeName: "azurerm_virtual_machine_extension", providerNamespace: "hashicorp", providerType: "azurerm",
	}
)

func (s moveSource) matches(providerAddress string, typeName string) bool {
	return s.typeName == typeName && providerAddressIs(providerAddress, s.providerNamespace, s.providerType)
}

// describe is how an error names the source: its resource type and the provider's short address.
func (s moveSource) describe() string {
	return s.typeName + " of the " + s.providerNamespace + "/" + s.providerType + " provider"
}

// resourceMove describes how one resource takes over the resources of its sources
// through a moved block. It is the single mover every resource uses, so the rules for
// what a move accepts, and what it answers when it does not, live here once.
//
// The framework stops at the first mover that returns a state or an error, so a resource
// has one mover that dispatches on the source. The mover always answers: a state, or an
// error of its own. A mover that answers nothing is "skipped" by the framework, which then
// reports a generic error that does not tell the practitioner what is supported.
type resourceMove[I any] struct {
	kind kind
	// sources are the resources this one accepts. azapi_resource is the only source for
	// most resources; a source is read only for the fields that name the object, never
	// the rest of its state, whose shape differs between schema versions.
	sources []moveSource
	parse   func(string) (I, error)
	// seed writes the attributes known from the ID alone, the same ones ImportState writes,
	// so the Read after the move fills in the rest exactly as it does after an import.
	seed func(ctx context.Context, state *tfsdk.State, id string, parsed I) diag.Diagnostics
	// markMoved leaves movedPrivateKey in private state for Read.
	markMoved bool
}

func (m resourceMove[I]) stateMovers() []resource.StateMover {
	return []resource.StateMover{{StateMover: m.move}}
}

// azapiSource is the part of an azapi_resource's state a move reads. Its type attribute
// is not read: azapi builds the id from the type, so the target's own ID parser already
// rejects an azapi_resource of another type.
type azapiSource struct {
	ID *string `json:"id"`
}

// azurermExtensionSource is the part of an azurerm_virtual_machine_extension's state a
// move reads. Publisher and Type are the extension's own publisher and type, as Azure
// reports them.
type azurermExtensionSource struct {
	ID        *string `json:"id"`
	Publisher *string `json:"publisher"`
	Type      *string `json:"type"`
}

func (m resourceMove[I]) move(ctx context.Context, req resource.MoveStateRequest, resp *resource.MoveStateResponse) {
	op := operation{action: actionMove, kind: m.kind, unnamed: true}
	fail := func(c detailError) { resp.Diagnostics.Append(op.fail(c)) }

	var source *moveSource
	for _, s := range m.sources {
		if s.matches(req.SourceProviderAddress, req.SourceTypeName) {
			source = &s
			break
		}
	}
	if source == nil {
		accepted := make([]string, 0, len(m.sources))
		for _, s := range m.sources {
			accepted = append(accepted, s.describe())
		}
		fail(newMoveSourceNotSupportedError(req.SourceProviderAddress, req.SourceTypeName, accepted))
		return
	}

	if req.SourceRawState == nil {
		fail(newMoveSourceStateUnreadableError(source.typeName, errors.New("the move request carried no state")))
		return
	}

	var id string
	var cause *detailError
	switch *source {
	case azapiResourceSource:
		id, cause = readAzapiSource(req.SourceRawState.JSON)
	case azurermVirtualMachineExtensionSource:
		id, cause = readAzurermExtensionSource(req.SourceRawState.JSON)
	}
	if cause != nil {
		fail(*cause)
		return
	}

	parsed, err := m.parse(id)
	if err != nil {
		fail(newInvalidMoveSourceIDError(source.typeName, idErrorCause(err)))
		return
	}

	if resp.TargetIdentity != nil {
		if resp.Diagnostics.Append(resp.TargetIdentity.Set(ctx, parsed)...); resp.Diagnostics.HasError() {
			return
		}
	}
	if resp.Diagnostics.Append(m.seed(ctx, &resp.TargetState, id, parsed)...); resp.Diagnostics.HasError() {
		return
	}
	if m.markMoved {
		resp.Diagnostics.Append(resp.TargetPrivate.SetKey(ctx, movedPrivateKey, []byte("true"))...)
	}
}

// readAzapiSource returns the id of an azapi_resource state, or the cause the move is
// refused for.
func readAzapiSource(raw []byte) (string, *detailError) {
	var source azapiSource
	if err := json.Unmarshal(raw, &source); err != nil {
		cause := newMoveSourceStateUnreadableError(azapiResourceSource.typeName, err)
		return "", &cause
	}

	if source.ID == nil || *source.ID == "" {
		cause := newMoveSourceIDMissingError(azapiResourceSource.typeName)
		return "", &cause
	}
	return *source.ID, nil
}

// readAzurermExtensionSource returns the id of an azurerm_virtual_machine_extension
// state, or the cause the move is refused for. A resource of azurerm_virtual_machine_extension
// is always a Microsoft.Compute extension, so the ARM type needs no check; what can differ
// is which extension it installs, which the publisher and type say.
func readAzurermExtensionSource(raw []byte) (string, *detailError) {
	var source azurermExtensionSource
	if err := json.Unmarshal(raw, &source); err != nil {
		cause := newMoveSourceStateUnreadableError(azurermVirtualMachineExtensionSource.typeName, err)
		return "", &cause
	}

	if source.ID == nil || *source.ID == "" {
		cause := newMoveSourceIDMissingError(azurermVirtualMachineExtensionSource.typeName)
		return "", &cause
	}
	if source.Publisher != nil || source.Type != nil {
		publisher, extensionType := derefString(source.Publisher), derefString(source.Type)
		if !isMountExtension(publisher, extensionType) {
			cause := newMoveSourceNotMountExtensionError(publisher, extensionType)
			return "", &cause
		}
	}
	return *source.ID, nil
}

// providerAddressIs matches a provider address by namespace and type, ignoring the
// hostname and the case: Terraform sends the address as it holds it in state, while a
// provider declares itself with a different spelling.
func providerAddressIs(address string, namespace string, providerType string) bool {
	parts := strings.Split(address, "/")
	if len(parts) < 2 {
		return false
	}
	return strings.EqualFold(parts[len(parts)-2], namespace) && strings.EqualFold(parts[len(parts)-1], providerType)
}
