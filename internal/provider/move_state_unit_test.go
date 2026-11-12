// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v9"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

// moveTestTarget is one resource a moved block can target.
type moveTestTarget struct {
	name     string // Terraform resource type name
	kind     kind
	armType  string
	id       string // a valid ID of this kind
	otherID  string // a valid ID of another kind
	identity func() (tftypes.Type, map[string]string)
	// seeded lists the attributes a move writes, which must be exactly the ones ImportState writes.
	seeded []string
	// acceptsAzurerm is true for the target that also takes an azurerm_virtual_machine_extension.
	acceptsAzurerm bool
}

// acceptedSources is what the target's not-supported error must name.
func (target moveTestTarget) acceptedSources() []string {
	accepted := []string{azapiResourceSource.describe()}
	if target.acceptsAzurerm {
		accepted = append(accepted, azurermVirtualMachineExtensionSource.describe())
	}
	return accepted
}

const (
	azapiProviderAddress   = "registry.terraform.io/azure/azapi"
	azurermProviderAddress = "registry.terraform.io/hashicorp/azurerm"
)

func moveTestTargets() []moveTestTarget {
	ctx := context.Background()
	identityType := func(s interface {
		TerraformType(context.Context) tftypes.Type
	},
	) tftypes.Type {
		return s.TerraformType(ctx)
	}
	return []moveTestTarget{
		{
			name: "everpure_azure_reservation", kind: kindReservation, armType: "PureStorage.Block/reservations",
			id: testReservationID(), otherID: testStoragePoolIDForVG(),
			identity: func() (tftypes.Type, map[string]string) {
				return identityType(armids.ReservationIdentitySchema().Type()), map[string]string{
					"subscription_id": testSubscriptionID, "resource_group_name": testResourceGroup, "name": "res1",
				}
			},
			seeded: []string{"id", "name", "resource_group_name"},
		},
		{
			name: "everpure_azure_storage_pool", kind: kindStoragePool, armType: "PureStorage.Block/storagePools",
			id: testStoragePoolIDForVG(), otherID: testVolumeGroupID("vg1"),
			identity: func() (tftypes.Type, map[string]string) {
				return identityType(armids.StoragePoolIdentitySchema().Type()), map[string]string{
					"subscription_id": testSubscriptionID, "resource_group_name": testResourceGroup, "name": testStoragePoolName,
				}
			},
			seeded: []string{"id", "name", "resource_group_name"},
		},
		{
			name: "everpure_azure_volume_group", kind: kindVolumeGroup, armType: "PureStorage.Block/storagePools/volumeGroups",
			id: testVolumeGroupID("vg1"), otherID: testStoragePoolIDForVG(),
			identity: func() (tftypes.Type, map[string]string) {
				return identityType(armids.VolumeGroupIdentitySchema().Type()), map[string]string{
					"subscription_id": testSubscriptionID, "resource_group_name": testResourceGroup, "storage_pool_name": testStoragePoolName, "name": "vg1",
				}
			},
			seeded: []string{"id", "name", "storage_pool_id", "source_type", "deletion_protection", "eradicate_on_destroy"},
		},
		{
			name: "everpure_azure_volume", kind: kindVolume, armType: "PureStorage.Block/storagePools/volumeGroups/volumes",
			id: testVolumeSourceID("vg1", "vol1"), otherID: testVolumeGroupID("vg1"),
			identity: func() (tftypes.Type, map[string]string) {
				return identityType(armids.VolumeIdentitySchema().Type()), map[string]string{
					"subscription_id": testSubscriptionID, "resource_group_name": testResourceGroup, "storage_pool_name": testStoragePoolName,
					"volume_group_name": "vg1", "name": "vol1",
				}
			},
			seeded: []string{"id", "name", "volume_group_id", "source_type", "deletion_protection", "eradicate_on_destroy"},
		},
		{
			name: "everpure_azure_snapshot", kind: kindSnapshot, armType: "PureStorage.Block/storagePools/volumeGroups/snapshots",
			id: testVolumeGroupSnapshotID("vg1", "snap1"), otherID: testVolumeGroupID("vg1"),
			identity: func() (tftypes.Type, map[string]string) {
				return identityType(armids.VolumeGroupSnapshotIdentitySchema().Type()), map[string]string{
					"subscription_id": testSubscriptionID, "resource_group_name": testResourceGroup, "storage_pool_name": testStoragePoolName,
					"volume_group_name": "vg1", "name": "snap1",
				}
			},
			seeded: []string{"id", "name", "volume_group_id", "deletion_protection", "eradicate_on_destroy"},
		},
		{
			name: "everpure_azure_virtual_machine_connection", kind: kindVirtualMachineConnection, armType: "Microsoft.Compute/virtualMachines/extensions",
			id: testExtensionID("vm1", "PureStorageMountIscsi"), otherID: testVMID(testSubscriptionID, "vm1"),
			identity: func() (tftypes.Type, map[string]string) {
				return identityType(armids.VirtualMachineExtensionIdentitySchema().Type()), map[string]string{
					"subscription_id": testSubscriptionID, "resource_group_name": testResourceGroup,
					"virtual_machine_name": "vm1", "name": "PureStorageMountIscsi",
				}
			},
			seeded:         []string{"id", "virtual_machine_id", "extension_name"},
			acceptsAzurerm: true,
		},
	}
}

// azurermExtensionState builds the JSON of an azurerm_virtual_machine_extension state. Only
// id, publisher and type matter to the move; the rest is here to show it does not depend on them.
func azurermExtensionState(t *testing.T, id string, publisher any, extensionType any) *tfprotov6.RawState {
	t.Helper()
	return azapiState(t, map[string]any{
		"id": id, "publisher": publisher, "type": extensionType, "name": "PureStorageMountIscsi",
		"virtual_machine_id": "/subscriptions/s", "settings": `{"targets":[]}`, "type_handler_version": "1.0",
	})
}

// requireMovedState checks a successful move response: the non-null attributes of the state are
// exactly the target's seeded ones, the id is wantID and the identity holds the target's identity.
func requireMovedState(t *testing.T, resp *tfprotov6.MoveResourceStateResponse, target moveTestTarget, stateType tftypes.Type, wantID string) {
	t.Helper()
	identityType, wantIdentity := target.identity()
	if len(resp.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}

	state, err := resp.TargetState.Unmarshal(stateType)
	if err != nil {
		t.Fatal(err)
	}
	var attrs map[string]tftypes.Value
	if err := state.As(&attrs); err != nil {
		t.Fatal(err)
	}
	var nonNull []string
	for name, v := range attrs {
		if v.IsNull() {
			continue
		}
		// A block that was never set reads back as an empty list, not as null.
		var elements []tftypes.Value
		if v.Type().Is(tftypes.List{}) && v.As(&elements) == nil && len(elements) == 0 {
			continue
		}
		nonNull = append(nonNull, name)
	}
	sort.Strings(nonNull)
	want := append([]string(nil), target.seeded...)
	sort.Strings(want)
	if fmt.Sprint(nonNull) != fmt.Sprint(want) {
		t.Fatalf("non-null attributes = %v, want %v", nonNull, want)
	}
	var gotID string
	if err := attrs["id"].As(&gotID); err != nil {
		t.Fatal(err)
	}
	if gotID != wantID {
		t.Fatalf("id = %q, want the source id %q", gotID, wantID)
	}

	if resp.TargetIdentity == nil || resp.TargetIdentity.IdentityData == nil {
		t.Fatal("no target identity")
	}
	identity, err := resp.TargetIdentity.IdentityData.Unmarshal(identityType)
	if err != nil {
		t.Fatal(err)
	}
	var identityAttrs map[string]tftypes.Value
	if err := identity.As(&identityAttrs); err != nil {
		t.Fatal(err)
	}
	for name, want := range wantIdentity {
		var got string
		if err := identityAttrs[name].As(&got); err != nil {
			t.Fatal(err)
		}
		// An ID in another case parses to identity components in that case.
		if !strings.EqualFold(got, want) {
			t.Fatalf("identity %s = %q, want %q", name, got, want)
		}
	}
}

// azapiState builds the JSON of an azapi_resource state the way azapi writes it. Only id and
// type matter to the move; body, name and parent_id are what differs between its schema
// versions, and are here to show the move does not depend on them.
func azapiState(t *testing.T, fields map[string]any) *tfprotov6.RawState {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return &tfprotov6.RawState{JSON: b}
}

func moveServer(t *testing.T) tfprotov6.ProviderServer {
	t.Helper()
	server, err := providerserver.NewProtocol6WithError(newProvider("test", fixedCredentialFactory(nil), nil)())()
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func moveResourceState(t *testing.T, server tfprotov6.ProviderServer, target string, providerAddress string, typeName string, schemaVersion int64, state *tfprotov6.RawState) *tfprotov6.MoveResourceStateResponse {
	t.Helper()
	resp, err := server.MoveResourceState(context.Background(), &tfprotov6.MoveResourceStateRequest{
		SourceProviderAddress: providerAddress,
		SourceTypeName:        typeName,
		SourceSchemaVersion:   schemaVersion,
		SourceState:           state,
		TargetTypeName:        target,
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func requireMoveError(t *testing.T, resp *tfprotov6.MoveResourceStateResponse, want operationDiagnostic) {
	t.Helper()
	if len(resp.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %v, want exactly %q", resp.Diagnostics, want.Detail())
	}
	got := resp.Diagnostics[0]
	if got.Severity != tfprotov6.DiagnosticSeverityError || got.Summary != want.Summary() || got.Detail != want.Detail() {
		t.Fatalf("diagnostic = %q / %q, want %q / %q", got.Summary, got.Detail, want.Summary(), want.Detail())
	}
	if resp.TargetState != nil {
		t.Fatal("an error must leave no target state")
	}
}

func TestMoveResourceStateFromAzapi(t *testing.T) {
	server := moveServer(t)
	schemas, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}

	for _, target := range moveTestTargets() {
		t.Run(target.name, func(t *testing.T) {
			stateType := schemas.ResourceSchemas[target.name].ValueType()
			op := operation{action: actionMove, kind: target.kind, unnamed: true}
			azapiType := target.armType + "@2026-05-01-preview"

			t.Run("accepted sources seed the state and the identity, and nothing else", func(t *testing.T) {
				tests := []struct {
					name            string
					providerAddress string
					typeName        string
					schemaVersion   int64
					fields          map[string]any
				}{
					{
						"hashicorp-style address", "registry.terraform.io/azure/azapi", "azapi_resource", 2,
						map[string]any{"id": target.id, "type": azapiType, "name": "x", "parent_id": "/subscriptions/s", "body": map[string]any{"properties": map[string]any{}}},
					},
					{"mixed case address", "registry.terraform.io/Azure/azapi", "azapi_resource", 2, map[string]any{"id": target.id, "type": azapiType}},
					{"other hostname", "registry.example.com/azure/AzAPI", "azapi_resource", 2, map[string]any{"id": target.id, "type": azapiType}},
					{"address without hostname", "azure/azapi", "azapi_resource", 2, map[string]any{"id": target.id, "type": azapiType}},
					{
						"schema version 0, body as JSON text", "registry.terraform.io/azure/azapi", "azapi_resource", 0,
						map[string]any{"id": target.id, "type": azapiType, "body": `{"properties":{}}`, "tags": map[string]any{}},
					},
					{
						"schema version 1", "registry.terraform.io/azure/azapi", "azapi_resource", 1,
						map[string]any{"id": target.id, "type": azapiType, "body": map[string]any{"a": []any{1, 2}}, "identity": []any{}},
					},
					// The id alone decides: the type is not read.
					{"no type", "registry.terraform.io/azure/azapi", "azapi_resource", 2, map[string]any{"id": target.id}},
					{"a type that disagrees with the id", "registry.terraform.io/azure/azapi", "azapi_resource", 2, map[string]any{"id": target.id, "type": "Microsoft.Compute/virtualMachines@2024-07-01"}},
					{"id in lower case", "registry.terraform.io/azure/azapi", "azapi_resource", 2, map[string]any{"id": strings.ToLower(target.id), "type": azapiType}},
				}
				for _, tt := range tests {
					t.Run(tt.name, func(t *testing.T) {
						resp := moveResourceState(t, server, target.name, tt.providerAddress, tt.typeName, tt.schemaVersion, azapiState(t, tt.fields))
						wantID, _ := tt.fields["id"].(string)
						requireMovedState(t, resp, target, stateType, wantID)
					})
				}
			})

			t.Run("the private marker is left for the snapshot and the virtual machine connection only", func(t *testing.T) {
				resp := moveResourceState(t, server, target.name, azapiProviderAddress, "azapi_resource", 2,
					azapiState(t, map[string]any{"id": target.id, "type": azapiType}))
				got := strings.Contains(string(resp.TargetPrivate), movedPrivateKey)
				if want := target.kind == kindSnapshot || target.kind == kindVirtualMachineConnection; got != want {
					t.Fatalf("private state %q has the marker = %v, want %v", resp.TargetPrivate, got, want)
				}
			})

			t.Run("rejected sources get this provider's own error", func(t *testing.T) {
				tests := []struct {
					name            string
					providerAddress string
					typeName        string
					state           *tfprotov6.RawState
					want            detailError
				}{
					{
						"another provider", azurermProviderAddress, "azapi_resource",
						azapiState(t, map[string]any{"id": target.id, "type": azapiType}),
						newMoveSourceNotSupportedError(azurermProviderAddress, "azapi_resource", target.acceptedSources()),
					},
					{
						"a provider that only ends in azapi", "registry.terraform.io/other/azapi", "azapi_resource",
						azapiState(t, map[string]any{"id": target.id, "type": azapiType}),
						newMoveSourceNotSupportedError("registry.terraform.io/other/azapi", "azapi_resource", target.acceptedSources()),
					},
					{
						"another azapi resource type", azapiProviderAddress, "azapi_update_resource",
						azapiState(t, map[string]any{"id": target.id, "type": azapiType}),
						newMoveSourceNotSupportedError(azapiProviderAddress, "azapi_update_resource", target.acceptedSources()),
					},
					{
						"another azurerm resource type", azurermProviderAddress, "azurerm_virtual_machine_scale_set_extension",
						azurermExtensionState(t, target.id, extensionPublisher, extensionTypeLinux),
						newMoveSourceNotSupportedError(azurermProviderAddress, "azurerm_virtual_machine_scale_set_extension", target.acceptedSources()),
					},
					{
						"no id", "registry.terraform.io/azure/azapi", "azapi_resource",
						azapiState(t, map[string]any{"type": azapiType}), newMoveSourceIDMissingError("azapi_resource"),
					},
					{
						"empty id", "registry.terraform.io/azure/azapi", "azapi_resource",
						azapiState(t, map[string]any{"id": "", "type": azapiType}), newMoveSourceIDMissingError("azapi_resource"),
					},
					{
						"id that is not an ARM ID", "registry.terraform.io/azure/azapi", "azapi_resource",
						azapiState(t, map[string]any{"id": "not-an-id", "type": azapiType}),
						newInvalidMoveSourceIDError("azapi_resource", idErrorCause(parseErrorFor(t, target, "not-an-id"))),
					},
					{
						"id of another kind", "registry.terraform.io/azure/azapi", "azapi_resource",
						azapiState(t, map[string]any{"id": target.otherID, "type": azapiType}),
						newInvalidMoveSourceIDError("azapi_resource", idErrorCause(parseErrorFor(t, target, target.otherID))),
					},
					{
						"state that is not JSON", "registry.terraform.io/azure/azapi", "azapi_resource",
						&tfprotov6.RawState{JSON: []byte("{")}, newMoveSourceStateUnreadableError("azapi_resource", unmarshalErrorFor("{")),
					},
					{
						"state whose id is not text", "registry.terraform.io/azure/azapi", "azapi_resource",
						azapiState(t, map[string]any{"id": 5, "type": azapiType}), newMoveSourceStateUnreadableError("azapi_resource", unmarshalErrorFor(`{"id":5,"type":"`+azapiType+`"}`)),
					},
				}
				if !target.acceptsAzurerm {
					tests = append(tests, struct {
						name            string
						providerAddress string
						typeName        string
						state           *tfprotov6.RawState
						want            detailError
					}{
						"an azurerm_virtual_machine_extension", azurermProviderAddress, "azurerm_virtual_machine_extension",
						azurermExtensionState(t, target.id, extensionPublisher, extensionTypeLinux),
						newMoveSourceNotSupportedError(azurermProviderAddress, "azurerm_virtual_machine_extension", target.acceptedSources()),
					})
				}
				for _, tt := range tests {
					t.Run(tt.name, func(t *testing.T) {
						resp := moveResourceState(t, server, target.name, tt.providerAddress, tt.typeName, 2, tt.state)
						requireMoveError(t, resp, op.fail(tt.want))
					})
				}
			})
		})
	}
}

// parseErrorFor returns the error the target kind's own ID parser gives for id, so the
// expected message comes from the parser and not from a copy of its wording.
func parseErrorFor(t *testing.T, target moveTestTarget, id string) error {
	t.Helper()
	var err error
	switch target.kind {
	case kindReservation:
		_, err = armids.ParseReservationID(id)
	case kindStoragePool:
		_, err = armids.ParseStoragePoolID(id)
	case kindVolumeGroup:
		_, err = armids.ParseVolumeGroupID(id)
	case kindVolume:
		_, err = armids.ParseVolumeID(id)
	case kindSnapshot:
		_, err = armids.ParseVolumeGroupSnapshotID(id)
	case kindVirtualMachineConnection:
		_, err = armids.ParseVirtualMachineExtensionID(id)
	}
	if err == nil {
		t.Fatalf("%s parsed %q, want an error", target.name, id)
	}
	return err
}

// unmarshalErrorFor returns the error decoding raw as an azapi state gives.
func unmarshalErrorFor(raw string) error {
	var source azapiSource
	return json.Unmarshal([]byte(raw), &source)
}

func TestMoveSourceMatches(t *testing.T) {
	tests := []struct {
		source   moveSource
		address  string
		typeName string
		want     bool
	}{
		{azapiResourceSource, "registry.terraform.io/azure/azapi", "azapi_resource", true},
		{azapiResourceSource, "registry.terraform.io/Azure/azapi", "azapi_resource", true},
		{azapiResourceSource, "registry.opentofu.org/azure/azapi", "azapi_resource", true},
		{azapiResourceSource, "azure/azapi", "azapi_resource", true},
		{azapiResourceSource, "registry.terraform.io/hashicorp/azurerm", "azapi_resource", false},
		{azapiResourceSource, "registry.terraform.io/other/azapi", "azapi_resource", false},
		{azapiResourceSource, "registry.terraform.io/azure/azapi-extra", "azapi_resource", false},
		{azapiResourceSource, "azapi", "azapi_resource", false},
		{azapiResourceSource, "", "azapi_resource", false},
		{azapiResourceSource, "registry.terraform.io/azure/azapi", "azapi_update_resource", false},
		{azurermVirtualMachineExtensionSource, "registry.terraform.io/hashicorp/azurerm", "azurerm_virtual_machine_extension", true},
		{azurermVirtualMachineExtensionSource, "registry.terraform.io/HashiCorp/AzureRM", "azurerm_virtual_machine_extension", true},
		{azurermVirtualMachineExtensionSource, "registry.example.com/hashicorp/azurerm", "azurerm_virtual_machine_extension", true},
		{azurermVirtualMachineExtensionSource, "hashicorp/azurerm", "azurerm_virtual_machine_extension", true},
		{azurermVirtualMachineExtensionSource, "registry.terraform.io/other/azurerm", "azurerm_virtual_machine_extension", false},
		{azurermVirtualMachineExtensionSource, "registry.terraform.io/hashicorp/azurerm", "azurerm_virtual_machine_scale_set_extension", false},
		{azurermVirtualMachineExtensionSource, "registry.terraform.io/azure/azapi", "azurerm_virtual_machine_extension", false},
	}
	for _, tt := range tests {
		t.Run(tt.address+" "+tt.typeName, func(t *testing.T) {
			if got := tt.source.matches(tt.address, tt.typeName); got != tt.want {
				t.Fatalf("%s.matches(%q, %q) = %v, want %v", tt.source.typeName, tt.address, tt.typeName, got, tt.want)
			}
		})
	}
}

func TestMoveResourceStateFromAzurerm(t *testing.T) {
	server := moveServer(t)
	schemas, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}

	var target moveTestTarget
	for _, candidate := range moveTestTargets() {
		if candidate.acceptsAzurerm {
			target = candidate
		}
	}
	stateType := schemas.ResourceSchemas[target.name].ValueType()
	op := operation{action: actionMove, kind: target.kind, unnamed: true}
	const typeName = "azurerm_virtual_machine_extension"

	t.Run("accepted sources seed the state and the identity, and nothing else", func(t *testing.T) {
		tests := []struct {
			name            string
			providerAddress string
			schemaVersion   int64
			id              string
			extensionType   string
		}{
			{"hashicorp-style address, Linux", azurermProviderAddress, 0, target.id, extensionTypeLinux},
			{"Windows", azurermProviderAddress, 0, target.id, extensionTypeWindows},
			{"mixed case address", "registry.terraform.io/HashiCorp/AzureRM", 0, target.id, extensionTypeLinux},
			{"other hostname", "registry.example.com/hashicorp/azurerm", 0, target.id, extensionTypeLinux},
			{"address without hostname", "hashicorp/azurerm", 0, target.id, extensionTypeLinux},
			{"a later schema version", azurermProviderAddress, 1, target.id, extensionTypeLinux},
			{"id in lower case", azurermProviderAddress, 0, strings.ToLower(target.id), extensionTypeLinux},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				resp := moveResourceState(t, server, target.name, tt.providerAddress, typeName, tt.schemaVersion,
					azurermExtensionState(t, tt.id, extensionPublisher, tt.extensionType))
				requireMovedState(t, resp, target, stateType, tt.id)
				if !strings.Contains(string(resp.TargetPrivate), movedPrivateKey) {
					t.Fatalf("private state %q has no marker", resp.TargetPrivate)
				}
			})
		}
	})

	t.Run("rejected sources get this provider's own error", func(t *testing.T) {
		tests := []struct {
			name  string
			state *tfprotov6.RawState
			want  detailError
		}{
			{"no id", azapiState(t, map[string]any{"publisher": extensionPublisher, "type": extensionTypeLinux}), newMoveSourceIDMissingError(typeName)},
			{"empty id", azurermExtensionState(t, "", extensionPublisher, extensionTypeLinux), newMoveSourceIDMissingError(typeName)},
			{"null id", azapiState(t, map[string]any{"id": nil, "publisher": extensionPublisher, "type": extensionTypeLinux}), newMoveSourceIDMissingError(typeName)},
			{
				"id that is not an ARM ID", azurermExtensionState(t, "not-an-id", extensionPublisher, extensionTypeLinux),
				newInvalidMoveSourceIDError(typeName, idErrorCause(parseErrorFor(t, target, "not-an-id"))),
			},
			{
				"id of another kind", azurermExtensionState(t, target.otherID, extensionPublisher, extensionTypeLinux),
				newInvalidMoveSourceIDError(typeName, idErrorCause(parseErrorFor(t, target, target.otherID))),
			},
			{
				"another publisher", azurermExtensionState(t, target.id, "Microsoft.Azure.Extensions", extensionTypeLinux),
				newMoveSourceNotMountExtensionError("Microsoft.Azure.Extensions", extensionTypeLinux),
			},
			{
				"another type", azurermExtensionState(t, target.id, extensionPublisher, "CustomScript"),
				newMoveSourceNotMountExtensionError(extensionPublisher, "CustomScript"),
			},
			{
				"a type in another case", azurermExtensionState(t, target.id, extensionPublisher, strings.ToLower(extensionTypeLinux)),
				newMoveSourceNotMountExtensionError(extensionPublisher, strings.ToLower(extensionTypeLinux)),
			},
			{
				"a publisher without a type", azapiState(t, map[string]any{"id": target.id, "publisher": extensionPublisher}),
				newMoveSourceNotMountExtensionError(extensionPublisher, ""),
			},
			{
				"state that is not JSON", &tfprotov6.RawState{JSON: []byte("{")},
				newMoveSourceStateUnreadableError(typeName, unmarshalErrorFor("{")),
			},
			{
				"state whose publisher is not text", azapiState(t, map[string]any{"id": target.id, "publisher": 5, "type": extensionTypeLinux}),
				newMoveSourceStateUnreadableError(typeName, unmarshalAzurermErrorFor(`{"id":"x","publisher":5,"type":"MountIscsiLinux"}`)),
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				resp := moveResourceState(t, server, target.name, azurermProviderAddress, typeName, 0, tt.state)
				requireMoveError(t, resp, op.fail(tt.want))
			})
		}
	})
}

// unmarshalAzurermErrorFor returns the error decoding raw as an azurerm extension state gives.
func unmarshalAzurermErrorFor(raw string) error {
	var source azurermExtensionSource
	return json.Unmarshal([]byte(raw), &source)
}

// TestMovedSnapshotIsCheckedByRead drives the move and the Read after it through the
// provider server, because the marker travels in private state, which only the framework
// can build. A snapshot of the volume group's schedule is refused on that first Read,
// where ImportState refuses it with a GET the move cannot make. A manual one passes, and
// the marker is gone from the private state Read returns, so later Reads do not check again.
func TestMovedSnapshotIsCheckedByRead(t *testing.T) {
	ctx := context.Background()
	factories, store := newTestSnapshotProviderFactories()
	createdAt := mustParseTime(t, "2026-09-01T00:00:00Z")
	store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "17", createdAt, true)
	store.seedSnapshot(testResourceGroup, testStoragePoolName, "vg1", "manual", createdAt, false)

	server, schemas := configuredServer(t, factories)

	const resourceName = "everpure_azure_snapshot"
	stateType := schemas.ResourceSchemas[resourceName].ValueType()
	azapiType := "PureStorage.Block/storagePools/volumeGroups/snapshots@2026-05-01-preview"

	move := func(t *testing.T, id string) *tfprotov6.MoveResourceStateResponse {
		t.Helper()
		resp := moveResourceState(t, server, resourceName, "registry.terraform.io/azure/azapi", "azapi_resource", 2,
			azapiState(t, map[string]any{"id": id, "type": azapiType}))
		if len(resp.Diagnostics) != 0 {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		return resp
	}
	read := func(t *testing.T, moved *tfprotov6.MoveResourceStateResponse, private []byte) *tfprotov6.ReadResourceResponse {
		t.Helper()
		resp, err := server.ReadResource(ctx, &tfprotov6.ReadResourceRequest{
			TypeName:        resourceName,
			CurrentState:    moved.TargetState,
			Private:         private,
			CurrentIdentity: moved.TargetIdentity,
		})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	t.Run("a snapshot of the schedule is refused as a move", func(t *testing.T) {
		id := testVolumeGroupSnapshotID("vg1", "17")
		moved := move(t, id)
		resp := read(t, moved, moved.TargetPrivate)
		want := operation{action: actionMove, kind: kindSnapshot, id: id, unnamed: true}.fail(newSnapshotOwnedByScheduleError(id))
		if len(resp.Diagnostics) != 1 || resp.Diagnostics[0].Summary != want.Summary() || resp.Diagnostics[0].Detail != want.Detail() {
			t.Fatalf("diagnostics = %v, want %q / %q", resp.Diagnostics, want.Summary(), want.Detail())
		}
	})

	t.Run("a manual snapshot is read and the marker is cleared", func(t *testing.T) {
		id := testVolumeGroupSnapshotID("vg1", "manual")
		moved := move(t, id)
		if !strings.Contains(string(moved.TargetPrivate), movedPrivateKey) {
			t.Fatalf("private state %q has no marker", moved.TargetPrivate)
		}
		resp := read(t, moved, moved.TargetPrivate)
		if len(resp.Diagnostics) != 0 {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
		if strings.Contains(string(resp.Private), movedPrivateKey) {
			t.Fatalf("private state %q still has the marker", resp.Private)
		}
		state, err := resp.NewState.Unmarshal(stateType)
		if err != nil {
			t.Fatal(err)
		}
		var attrs map[string]tftypes.Value
		if err := state.As(&attrs); err != nil {
			t.Fatal(err)
		}
		if attrs["created_at"].IsNull() {
			t.Fatal("created_at is null: Read did not fill in the state")
		}

		again := read(t, moved, resp.Private)
		if len(again.Diagnostics) != 0 {
			t.Fatalf("unexpected diagnostics on a second read: %v", again.Diagnostics)
		}
	})

	t.Run("without the marker, Read does not check a snapshot of the schedule", func(t *testing.T) {
		id := testVolumeGroupSnapshotID("vg1", "17")
		moved := move(t, id)
		resp := read(t, moved, nil)
		if len(resp.Diagnostics) != 0 {
			t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
		}
	})
}

// configuredServer returns the provider server of factories with the provider configured, as it
// is when Terraform reads a resource, and the server's schemas.
func configuredServer(t *testing.T, factories map[string]func() (tfprotov6.ProviderServer, error)) (tfprotov6.ProviderServer, *tfprotov6.GetProviderSchemaResponse) {
	t.Helper()
	ctx := context.Background()
	server, err := factories["everpure"]()
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}

	providerType, ok := schemas.Provider.ValueType().(tftypes.Object)
	if !ok {
		t.Fatalf("provider schema type = %T, want an object", schemas.Provider.ValueType())
	}
	providerConfig := map[string]tftypes.Value{}
	for name, typ := range providerType.AttributeTypes {
		providerConfig[name] = tftypes.NewValue(typ, nil)
	}
	providerConfig["subscription_id"] = tftypes.NewValue(tftypes.String, testSubscriptionID)
	configValue, err := tfprotov6.NewDynamicValue(providerType, tftypes.NewValue(providerType, providerConfig))
	if err != nil {
		t.Fatal(err)
	}
	configured, err := server.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: &configValue})
	if err != nil || len(configured.Diagnostics) != 0 {
		t.Fatalf("configure: %v, %v", err, configured.Diagnostics)
	}
	return server, schemas
}

// TestMovedVirtualMachineConnectionIsCheckedByRead drives the move and the Read after it through
// the provider server, because the marker travels in private state, which only the framework can
// build. The move cannot ask the API which extension the id names, so the first Read does: a
// foreign extension is refused as a move, and not as an out-of-band replacement, which is what
// it is without the marker. For both sources a move leaves the marker, and a successful Read
// clears it.
func TestMovedVirtualMachineConnectionIsCheckedByRead(t *testing.T) {
	ctx := context.Background()
	factories, _, _, computeStore := newTestVMConnectionProviderFactories()
	computeStore.seedVirtualMachine(testResourceGroup, "vm1", armcompute.OperatingSystemTypesLinux, "eastus2")
	computeStore.seedExtension(testResourceGroup, "vm1", "ours", extensionPublisher, extensionTypeLinux, map[string]any{})
	computeStore.seedExtension(testResourceGroup, "vm1", "foreign", "Microsoft.Azure.Extensions", "CustomScript", map[string]any{})
	server, schemas := configuredServer(t, factories)

	const resourceName = "everpure_azure_virtual_machine_connection"
	stateType := schemas.ResourceSchemas[resourceName].ValueType()
	sources := []struct {
		name  string
		state func(t *testing.T, id string) *tfprotov6.MoveResourceStateResponse
	}{
		{"azapi_resource", func(t *testing.T, id string) *tfprotov6.MoveResourceStateResponse {
			return moveResourceState(t, server, resourceName, azapiProviderAddress, "azapi_resource", 2,
				azapiState(t, map[string]any{"id": id, "type": "Microsoft.Compute/virtualMachines/extensions@2024-07-01"}))
		}},
		// The extension's own publisher and type are what the extension is; the foreign one is
		// described as ours here so that only Read, which asks the API, can tell.
		{"azurerm_virtual_machine_extension", func(t *testing.T, id string) *tfprotov6.MoveResourceStateResponse {
			return moveResourceState(t, server, resourceName, azurermProviderAddress, "azurerm_virtual_machine_extension", 0,
				azurermExtensionState(t, id, extensionPublisher, extensionTypeLinux))
		}},
	}
	read := func(t *testing.T, moved *tfprotov6.MoveResourceStateResponse, private []byte) *tfprotov6.ReadResourceResponse {
		t.Helper()
		resp, err := server.ReadResource(ctx, &tfprotov6.ReadResourceRequest{
			TypeName:        resourceName,
			CurrentState:    moved.TargetState,
			Private:         private,
			CurrentIdentity: moved.TargetIdentity,
		})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	requireError := func(t *testing.T, resp *tfprotov6.ReadResourceResponse, want operationDiagnostic) {
		t.Helper()
		if len(resp.Diagnostics) != 1 || resp.Diagnostics[0].Summary != want.Summary() || resp.Diagnostics[0].Detail != want.Detail() {
			t.Fatalf("diagnostics = %v, want %q / %q", resp.Diagnostics, want.Summary(), want.Detail())
		}
	}

	for _, source := range sources {
		t.Run(source.name, func(t *testing.T) {
			t.Run("a foreign extension is refused as a move", func(t *testing.T) {
				id := testExtensionID("vm1", "foreign")
				moved := source.state(t, id)
				if len(moved.Diagnostics) != 0 {
					t.Fatalf("unexpected diagnostics: %v", moved.Diagnostics)
				}
				if !strings.Contains(string(moved.TargetPrivate), movedPrivateKey) {
					t.Fatalf("private state %q has no marker", moved.TargetPrivate)
				}
				want := operation{action: actionMove, kind: kindVirtualMachineConnection, id: id, unnamed: true}.fail(newMovedExtensionNotMountExtensionError(id))
				requireError(t, read(t, moved, moved.TargetPrivate), want)
			})

			t.Run("this resource's extension is read and the marker is cleared", func(t *testing.T) {
				id := testExtensionID("vm1", "ours")
				moved := source.state(t, id)
				resp := read(t, moved, moved.TargetPrivate)
				if len(resp.Diagnostics) != 0 {
					t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
				}
				if strings.Contains(string(resp.Private), movedPrivateKey) {
					t.Fatalf("private state %q still has the marker", resp.Private)
				}
				state, err := resp.NewState.Unmarshal(stateType)
				if err != nil {
					t.Fatal(err)
				}
				var attrs map[string]tftypes.Value
				if err := state.As(&attrs); err != nil {
					t.Fatal(err)
				}
				if attrs["iscsi_session_count"].IsNull() {
					t.Fatal("iscsi_session_count is null: Read did not fill in the state")
				}

				again := read(t, moved, resp.Private)
				if len(again.Diagnostics) != 0 {
					t.Fatalf("unexpected diagnostics on a second read: %v", again.Diagnostics)
				}
			})

			t.Run("without the marker, a foreign extension is an out-of-band replacement", func(t *testing.T) {
				id := testExtensionID("vm1", "foreign")
				moved := source.state(t, id)
				want := operation{action: actionRetrieve, kind: kindVirtualMachineConnection, id: id, unnamed: true}.fail(newExtensionReplacedOutOfBandError(id))
				requireError(t, read(t, moved, nil), want)
			})
		})
	}
}
