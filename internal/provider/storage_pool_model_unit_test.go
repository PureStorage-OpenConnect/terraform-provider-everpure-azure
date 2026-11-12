// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"reflect"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

func TestPlatformConsoleFromAPI(t *testing.T) {
	ctx := context.Background()

	t.Run("returns a null list when settings are nil", func(t *testing.T) {
		got, diags := platformConsoleFromAPI(ctx, nil)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if !got.IsNull() {
			t.Fatalf("got %v, want a null list", got)
		}
	})

	t.Run("returns a null list when enabled is false", func(t *testing.T) {
		got, diags := platformConsoleFromAPI(ctx, &armpurestorageblock.PlatformConsoleSettings{Enabled: to.Ptr(false)})
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if !got.IsNull() {
			t.Fatalf("got %v, want a null list", got)
		}
	})

	t.Run("maps an enabled block, including subnet details", func(t *testing.T) {
		settings := &armpurestorageblock.PlatformConsoleSettings{
			Enabled:         to.Ptr(true),
			Gui:             &armpurestorageblock.PlatformConsoleAccessSettings{Enabled: to.Ptr(true)},
			API:             &armpurestorageblock.PlatformConsoleAccessSettings{Enabled: to.Ptr(true)},
			Cli:             &armpurestorageblock.PlatformConsoleAccessSettings{Enabled: to.Ptr(false)},
			DefaultUsername: to.Ptr("pureuser"),
			Subnets: []*armpurestorageblock.PlatformConsoleSubnet{{
				ID:                  to.Ptr("/subnet/1"),
				ManagementIPAddress: to.Ptr("10.0.0.5"),
				ServiceBackendIPs:   []*string{to.Ptr("10.0.0.6")},
			}},
		}

		list, diags := platformConsoleFromAPI(ctx, settings)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		var got []PlatformConsoleModel
		if d := list.ElementsAs(ctx, &got, false); d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}
		if len(got) != 1 {
			t.Fatalf("got %d blocks, want 1", len(got))
		}
		if !got[0].GUIEnabled.ValueBool() {
			t.Fatal("GUIEnabled = false, want true")
		}
		if !got[0].APIEnabled.ValueBool() {
			t.Fatal("APIEnabled = false, want true")
		}
		if got[0].CLIEnabled.ValueBool() {
			t.Fatal("CLIEnabled = true, want false")
		}
		if got[0].DefaultUsername.ValueString() != "pureuser" {
			t.Fatalf("DefaultUsername = %q, want %q", got[0].DefaultUsername.ValueString(), "pureuser")
		}

		var subnets []PlatformConsoleSubnetModel
		if d := got[0].Subnet.ElementsAs(ctx, &subnets, false); d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}
		if len(subnets) != 1 {
			t.Fatalf("got %d subnets, want 1", len(subnets))
		}
		if subnets[0].ID.ValueString() != "/subnet/1" {
			t.Fatalf("ID = %q, want %q", subnets[0].ID.ValueString(), "/subnet/1")
		}
		if subnets[0].ManagementIPAddress.ValueString() != "10.0.0.5" {
			t.Fatalf("ManagementIPAddress = %q, want %q", subnets[0].ManagementIPAddress.ValueString(), "10.0.0.5")
		}

		var subnetIDs []string
		if d := got[0].SubnetIDs.ElementsAs(ctx, &subnetIDs, false); d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}
		want := []string{"/subnet/1"}
		if !reflect.DeepEqual(subnetIDs, want) {
			t.Fatalf("got %v, want %v", subnetIDs, want)
		}
	})
}

// newUpdatePlan builds a minimal plan for toStoragePoolUpdate, with tags set
// to the given value.
func newUpdatePlan(tags types.Map) StoragePoolResourceModel {
	return StoragePoolResourceModel{
		ProvisionedBandwidthInMbps: types.Int64Value(1024),
		Tags:                       tags,
	}
}

func TestToStoragePoolUpdate(t *testing.T) {
	ctx := context.Background()
	const subnetID = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet/subnets/snet"

	t.Run("sends a non-nil empty tags map when the plan's tags are null, so PATCH clears out-of-band tags", func(t *testing.T) {
		plan := newUpdatePlan(types.MapNull(types.StringType))

		update, diags := toStoragePoolUpdate(ctx, plan, types.ListNull(platformConsoleObjectType), subnetID)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if update.Tags == nil {
			t.Fatal("got nil tags, want a non-nil empty map so the PATCH body includes an explicit clear")
		}
		if len(update.Tags) != 0 {
			t.Fatalf("got %v, want empty", update.Tags)
		}
	})

	t.Run("passes a populated tags map through unchanged", func(t *testing.T) {
		tags, d := types.MapValueFrom(ctx, types.StringType, map[string]string{"environment": "prod"})
		if d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}
		plan := newUpdatePlan(tags)

		update, diags := toStoragePoolUpdate(ctx, plan, types.ListNull(platformConsoleObjectType), subnetID)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if update.Tags["environment"] == nil || *update.Tags["environment"] != "prod" {
			t.Fatalf("got %v, want environment=prod", update.Tags)
		}
	})
}

func idValue(s string) armids.IDValue {
	return armids.IDValue{StringValue: types.StringValue(s)}
}

func TestApplyStoragePool(t *testing.T) {
	ctx := context.Background()

	basePool := func() armpurestorageblock.StoragePool {
		return armpurestorageblock.StoragePool{
			ID:       to.Ptr("/subscriptions/sub/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1"),
			Name:     to.Ptr("pool1"),
			Location: to.Ptr("eastus2"),
			Properties: &armpurestorageblock.StoragePoolProperties{
				AvailabilityZone:             to.Ptr("1"),
				ProvisionedBandwidthMbPerSec: to.Ptr(int64(1024)),
				ReservationResourceID: to.Ptr(
					"/subscriptions/SUB/resourceGroups/RG/providers/PureStorage.Block/reservations/RES1"),
				VnetInjection: &armpurestorageblock.VnetInjection{
					SubnetID: to.Ptr("/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.Network/" +
						"virtualNetworks/VNET1/subnets/SNET1"),
					VnetID: to.Ptr("/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.Network/virtualNetworks/VNET1"),
				},
			},
		}
	}

	t.Run("maps a display-name location to the canonical short name", func(t *testing.T) {
		pool := basePool()
		pool.Location = to.Ptr("East US")
		data := &StoragePoolResourceModel{}

		if diags := applyStoragePool(ctx, data, pool); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if got := data.Location.ValueString(); got != "eastus" {
			t.Fatalf("Location = %q, want %q", got, "eastus")
		}
	})

	t.Run("returns an error instead of panicking when the API response has no properties", func(t *testing.T) {
		data := &StoragePoolResourceModel{}
		diags := applyStoragePool(ctx, data, armpurestorageblock.StoragePool{Properties: nil})
		if !diags.HasError() {
			t.Fatal("expected an error diagnostic")
		}
	})

	// applyStoragePool always maps reservation_id and vnet_injection straight from the
	// API response, whatever data already held: armids.IDType's own StringSemanticEquals
	// is what keeps a Create/Read/Update caller's prior casing when the API's differs only
	// by case (see armids.IDValueStringSemanticEquals and its test), one layer up in the
	// framework's own RPC handling, not in this mapping function.
	t.Run("maps reservation_id and vnet_injection straight from the API response", func(t *testing.T) {
		data := &StoragePoolResourceModel{
			ReservationID: idValue(
				"/subscriptions/sub/resourceGroups/rg/providers/PureStorage.Block/reservations/res1"),
		}

		diags := applyStoragePool(ctx, data, basePool())
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}

		if got := data.ReservationID.ValueString(); got != "/subscriptions/SUB/resourceGroups/RG/providers/PureStorage.Block/reservations/RES1" {
			t.Fatalf("ReservationID = %q, want the API's own casing", got)
		}
		vnetInjection, d := blockElements[VnetInjectionModel](ctx, data.VnetInjection)
		if d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}
		if len(vnetInjection) != 1 {
			t.Fatalf("got %d vnet_injection blocks, want 1", len(vnetInjection))
		}
		if got := vnetInjection[0].SubnetID.ValueString(); got != "/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.Network/virtualNetworks/VNET1/subnets/SNET1" {
			t.Fatalf("SubnetID = %q, want the API's own casing", got)
		}
		if got := vnetInjection[0].VirtualNetworkID.ValueString(); got != "/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.Network/virtualNetworks/VNET1" {
			t.Fatalf("VirtualNetworkID = %q, want the API's own casing", got)
		}
	})

	t.Run("never touches name, so a response spelled in another letter case cannot change it", func(t *testing.T) {
		data := &StoragePoolResourceModel{Name: types.StringValue("pool1")}
		pool := basePool()
		pool.Name = to.Ptr("POOL1")

		if diags := applyStoragePool(ctx, data, pool); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if got := data.Name.ValueString(); got != "pool1" {
			t.Fatalf("got name %q, want %q", got, "pool1")
		}
	})

	t.Run("never touches tags, even when the response carries tags beyond the plan", func(t *testing.T) {
		planned, d := types.MapValueFrom(ctx, types.StringType, map[string]string{"environment": "test"})
		if d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}
		data := &StoragePoolResourceModel{Tags: planned}

		pool := basePool()
		// Simulates an environment's own Azure Policy, or the resource provider
		// layer, appending extra tags to the create/update response beyond what
		// the plan sent.
		pool.Tags = map[string]*string{
			"environment":   to.Ptr("test"),
			"stamped-by-rp": to.Ptr("true"),
		}

		diags := applyStoragePool(ctx, data, pool)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}

		var got map[string]string
		if d := data.Tags.ElementsAs(ctx, &got, false); d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}
		want := map[string]string{"environment": "test"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v (the planned value, unchanged by the API response)", got, want)
		}
	})
}

func TestApplyStoragePoolTags(t *testing.T) {
	ctx := context.Background()

	t.Run("maps the API's tags, including ones beyond the prior value — this is Read's job", func(t *testing.T) {
		prior, d := types.MapValueFrom(ctx, types.StringType, map[string]string{"environment": "test"})
		if d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}
		data := &StoragePoolResourceModel{Tags: prior}

		apiTags := map[string]*string{
			"environment":   to.Ptr("test"),
			"stamped-by-rp": to.Ptr("true"),
		}

		diags := applyStoragePoolTags(ctx, data, apiTags)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}

		var got map[string]string
		if d := data.Tags.ElementsAs(ctx, &got, false); d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}
		want := map[string]string{"environment": "test", "stamped-by-rp": "true"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
}

func TestStoragePoolItemFromName(t *testing.T) {
	pool := armpurestorageblock.StoragePool{
		ID:         to.Ptr("/subscriptions/sub/resourceGroups/rg/providers/PureStorage.Block/storagePools/Pool1"),
		Name:       to.Ptr("Pool1"),
		Properties: &armpurestorageblock.StoragePoolProperties{},
	}

	item, diags := storagePoolItemFrom(context.Background(), pool)
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if got := item.Name.ValueString(); got != "Pool1" {
		t.Fatalf("got name %q, want %q (the API's spelling)", got, "Pool1")
	}
}

// consoleList builds a platform_console list holding one block, with the computed-only
// attributes either carrying values (as state holds them) or unknown (as a plan holds them).
func consoleList(t *testing.T, gui, api, cli types.Bool, computedUnknown bool) types.List {
	t.Helper()
	m := PlatformConsoleModel{
		GUIEnabled:      gui,
		APIEnabled:      api,
		CLIEnabled:      cli,
		SubnetIDs:       types.ListValueMust(types.StringType, []attr.Value{types.StringValue("/subnet/1")}),
		DefaultUsername: types.StringValue("pureuser"),
		Subnet:          types.ListValueMust(platformConsoleSubnetObjectType, nil),
	}
	if computedUnknown {
		m.SubnetIDs = types.ListUnknown(types.StringType)
		m.DefaultUsername = types.StringUnknown()
		m.Subnet = types.ListUnknown(platformConsoleSubnetObjectType)
	}
	list, d := types.ListValueFrom(context.Background(), platformConsoleObjectType, []PlatformConsoleModel{m})
	if d.HasError() {
		t.Fatalf("unexpected diagnostics: %v", d)
	}
	return list
}

func TestPlatformConsoleConfigChanged(t *testing.T) {
	yes, no := types.BoolValue(true), types.BoolValue(false)
	absent := types.ListNull(platformConsoleObjectType)

	tests := []struct {
		name        string
		prior, plan types.List
		want        bool
	}{
		{"both absent", absent, absent, false},
		{"added", absent, consoleList(t, yes, yes, no, false), true},
		{"removed", consoleList(t, yes, yes, no, false), absent, true},
		{"same settings", consoleList(t, yes, yes, no, false), consoleList(t, yes, yes, no, false), false},
		{"one setting changed", consoleList(t, yes, yes, no, false), consoleList(t, yes, yes, yes, false), true},
		{"same settings with computed fields unknown in plan", consoleList(t, yes, yes, no, false), consoleList(t, yes, yes, no, true), false},
		{"unknown setting in plan", consoleList(t, yes, yes, no, false), consoleList(t, yes, yes, types.BoolUnknown(), false), true},
		{"unknown list in plan", consoleList(t, yes, yes, no, false), types.ListUnknown(platformConsoleObjectType), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, diags := platformConsoleConfigChanged(context.Background(), tt.prior, tt.plan)
			if diags.HasError() {
				t.Fatalf("unexpected diagnostics: %v", diags)
			}
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestToStoragePoolUpdatePlatformConsole(t *testing.T) {
	ctx := context.Background()
	const subnetID = "/subnet/1"
	yes, no := types.BoolValue(true), types.BoolValue(false)
	absent := types.ListNull(platformConsoleObjectType)

	tests := []struct {
		name        string
		prior, plan types.List
		wantSent    bool
		wantEnabled bool
	}{
		{"unchanged console is not sent", consoleList(t, yes, yes, no, false), consoleList(t, yes, yes, no, true), false, false},
		{"both absent is not sent", absent, absent, false, false},
		{"changed setting is sent", consoleList(t, yes, yes, no, false), consoleList(t, yes, yes, yes, true), true, true},
		{"added block is sent enabled", absent, consoleList(t, yes, yes, no, true), true, true},
		{"removed block is sent disabled", consoleList(t, yes, yes, no, false), absent, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := newUpdatePlan(types.MapNull(types.StringType))
			plan.PlatformConsole = tt.plan

			update, diags := toStoragePoolUpdate(ctx, plan, tt.prior, subnetID)
			if diags.HasError() {
				t.Fatalf("unexpected diagnostics: %v", diags)
			}
			settings := update.Properties.PlatformConsoleSettings
			if (settings != nil) != tt.wantSent {
				t.Fatalf("got settings %v, want sent = %v", settings, tt.wantSent)
			}
			if settings != nil && (settings.Enabled == nil || *settings.Enabled != tt.wantEnabled) {
				t.Fatalf("got Enabled %v, want %v", settings.Enabled, tt.wantEnabled)
			}
		})
	}
}
