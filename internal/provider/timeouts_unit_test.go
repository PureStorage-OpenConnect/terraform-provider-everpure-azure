// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestStoragePoolTimeoutDefaults(t *testing.T) {
	t.Run("resolves to the resource's own default when unset", func(t *testing.T) {
		var unset timeouts.Value

		create, diags := unset.Create(context.Background(), defaultStoragePoolCreateTimeout)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if create != 95*time.Minute {
			t.Fatalf("create = %v, want %v", create, 95*time.Minute)
		}

		read, diags := unset.Read(context.Background(), defaultStoragePoolReadTimeout)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if read != 5*time.Minute {
			t.Fatalf("read = %v, want %v", read, 5*time.Minute)
		}

		update, diags := unset.Update(context.Background(), defaultStoragePoolUpdateTimeout)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if update != 30*time.Minute {
			t.Fatalf("update = %v, want %v", update, 30*time.Minute)
		}

		deleteTimeout, diags := unset.Delete(context.Background(), defaultStoragePoolDeleteTimeout)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if deleteTimeout != 95*time.Minute {
			t.Fatalf("delete = %v, want %v", deleteTimeout, 95*time.Minute)
		}
	})

	t.Run("resolves to the configured value when set, overriding the default, independently per operation", func(t *testing.T) {
		attrTypes := map[string]attr.Type{
			"create": types.StringType,
			"read":   types.StringType,
			"update": types.StringType,
			"delete": types.StringType,
		}
		obj := types.ObjectValueMust(attrTypes, map[string]attr.Value{
			"create": types.StringValue("45m"),
			"read":   types.StringNull(),
			"update": types.StringNull(),
			"delete": types.StringNull(),
		})
		configured := timeouts.Value{Object: obj}

		create, diags := configured.Create(context.Background(), defaultStoragePoolCreateTimeout)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if create != 45*time.Minute {
			t.Fatalf("create = %v, want %v", create, 45*time.Minute)
		}

		// read was left null in config: falls back to this resource's own
		// read default, not to create's configured override.
		read, diags := configured.Read(context.Background(), defaultStoragePoolReadTimeout)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if read != 5*time.Minute {
			t.Fatalf("read = %v, want %v", read, 5*time.Minute)
		}
	})
}

func TestHumanMinutes(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"singular minute", 1 * time.Minute, "1 minute"},
		{"a few minutes", 5 * time.Minute, "5 minutes"},
		{"storage pool create/delete default", defaultStoragePoolCreateTimeout, "95 minutes"},
		{"storage pool update default", defaultStoragePoolUpdateTimeout, "30 minutes"},
		{"volume group create/update default", defaultVolumeGroupCreateTimeout, "20 minutes"},
		{"volume group delete default", defaultVolumeGroupDeleteTimeout, "35 minutes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := humanMinutes(tt.d); got != tt.want {
				t.Fatalf("humanMinutes(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

// timeoutOpDescription pulls a single operation's Description string out of the
// schema.Block timeoutsBlock returns, so tests can check it without
// duplicating knowledge of the underlying library's SingleNestedBlock shape.
func timeoutOpDescription(t *testing.T, attribute schema.Block, op string) string {
	t.Helper()
	nested, ok := attribute.(schema.SingleNestedBlock)
	if !ok {
		t.Fatalf("got %T, want schema.SingleNestedBlock", attribute)
	}
	opAttr, ok := nested.Attributes[op].(schema.StringAttribute)
	if !ok {
		t.Fatalf("missing %q attribute", op)
	}
	return opAttr.Description
}

func TestTimeoutsBlockDescriptions(t *testing.T) {
	ctx := context.Background()

	t.Run("storage pool: each operation's description states this resource's own default, not the library's generic text", func(t *testing.T) {
		attribute := timeoutsBlock(ctx, "the pool", timeoutsDefaults{
			Create: defaultStoragePoolCreateTimeout,
			Read:   defaultStoragePoolReadTimeout,
			Update: defaultStoragePoolUpdateTimeout,
			Delete: defaultStoragePoolDeleteTimeout,
		}, "")

		create := timeoutOpDescription(t, attribute, "create")
		requireContains(t, create, "Defaults to "+humanMinutes(defaultStoragePoolCreateTimeout))
		requireContains(t, create, "creating the pool")

		read := timeoutOpDescription(t, attribute, "read")
		requireContains(t, read, "Defaults to "+humanMinutes(defaultStoragePoolReadTimeout))
		requireContains(t, read, "reading the pool")

		update := timeoutOpDescription(t, attribute, "update")
		requireContains(t, update, "Defaults to "+humanMinutes(defaultStoragePoolUpdateTimeout))
		requireContains(t, update, "updating the pool")

		deleteDesc := timeoutOpDescription(t, attribute, "delete")
		requireContains(t, deleteDesc, "Defaults to "+humanMinutes(defaultStoragePoolDeleteTimeout))
		requireContains(t, deleteDesc, "destroying the pool")
	})

	t.Run("volume group: delete description also names eradication of the recoverable copy", func(t *testing.T) {
		attribute := timeoutsBlock(ctx, "the volume group", timeoutsDefaults{
			Create: defaultVolumeGroupCreateTimeout,
			Read:   defaultVolumeGroupReadTimeout,
			Update: defaultVolumeGroupUpdateTimeout,
			Delete: defaultVolumeGroupDeleteTimeout,
		}, ", including eradicating its recoverable copy when eradicate_on_destroy is set")

		create := timeoutOpDescription(t, attribute, "create")
		requireContains(t, create, "Defaults to "+humanMinutes(defaultVolumeGroupCreateTimeout))
		requireContains(t, create, "creating the volume group")

		deleteDesc := timeoutOpDescription(t, attribute, "delete")
		requireContains(t, deleteDesc, "Defaults to "+humanMinutes(defaultVolumeGroupDeleteTimeout))
		requireContains(t, deleteDesc, "destroying the volume group")
		requireContains(t, deleteDesc, "eradicating its recoverable copy")
		requireContains(t, deleteDesc, "eradicate_on_destroy")
	})
}

func TestTimeoutsBlockOmitsUpdateWhenNoDefault(t *testing.T) {
	ctx := context.Background()
	got, ok := timeoutsBlock(ctx, "the snapshot", timeoutsDefaults{
		Create: defaultSnapshotCreateTimeout,
		Read:   defaultSnapshotReadTimeout,
		Delete: defaultSnapshotDeleteTimeout,
	}, "").(schema.SingleNestedBlock)
	if !ok {
		t.Fatal("timeouts block is not a schema.SingleNestedBlock")
	}

	for _, want := range []string{"create", "read", "delete"} {
		if _, ok := got.Attributes[want]; !ok {
			t.Errorf("timeouts is missing %q", want)
		}
	}
	if _, ok := got.Attributes["update"]; ok {
		t.Error("timeouts has an update entry, want none")
	}
}
