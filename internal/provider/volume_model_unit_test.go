// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/PureStorage-OpenConnect/terraform-provider-everpure-azure/internal/armids"
)

// Shared ARM ID fixtures for every test in this file (and, since Go tests in one
// package share a namespace, resource_volume_unit_test.go too): two volume groups
// under the same storage pool (built via the integration suite's own testVolumeGroupID
// helper, so these fixtures stay under the one shared subscription/resource
// group/pool), one volume group under a different pool, and volumes/recoverable
// volumes positioned relative to them for the cross-pool and cross-group guards.
var (
	testVolumeGroupIDFixture  = testVolumeGroupID("vg1")
	testVolumeGroupID2Fixture = testVolumeGroupID("vg2") // same pool as testVolumeGroupIDFixture, different group
	testVolumeIDSamePool      = testVolumeSourceID("vg2", "vol0")
	testVolumeIDOtherPool     = armids.VolumeGroupID{
		ResourceGroupID: armids.ResourceGroupID{
			SubscriptionID:    testSubscriptionID,
			ResourceGroupName: testResourceGroup,
		},
		StoragePoolName: "pool2",
		VolumeGroupName: "vg2",
	}.FullID() + "/volumes/vol0"
	testVolumeIDInVG = testVolumeSourceID("vg1", "vol0") // within testVolumeGroupIDFixture itself
	// testSnapshotIDOtherPool is snapshot-shaped, unlike testVolumeIDOtherPool: source_snapshot.id
	// is checked against armids.TypeVolumeGroupSnapshot specifically now, so a stand-in for it has to
	// actually match that type's own pattern, not just carry a well-formed pool prefix.
	testSnapshotIDOtherPool = armids.VolumeGroupID{
		ResourceGroupID: armids.ResourceGroupID{
			SubscriptionID:    testSubscriptionID,
			ResourceGroupName: testResourceGroup,
		},
		StoragePoolName: "pool2",
		VolumeGroupName: "vg2",
	}.FullID() + "/snapshots/snap0"
)

// volumeSourceSnapshotObjectType is source_snapshot's element type, matching
// VolumeSourceSnapshotModel's tfsdk tags. volume_model.go never needs this as a named
// constant of its own — it only ever decodes the block via blockElements, which infers
// the type from the target Go struct — so it exists only here, for building test
// fixtures with types.ListValueFrom/ListNull/ListUnknown.
var volumeSourceSnapshotObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"id":          armids.IDType{},
	"volume_name": types.StringType,
}}

// snapshotList builds a source_snapshot block value with the given elements. Called
// with no arguments it returns a known, empty list — how Terraform represents a block
// that appears zero times, as distinct from a null one.
func snapshotList(t *testing.T, ctx context.Context, snapshots ...VolumeSourceSnapshotModel) types.List {
	t.Helper()
	list, diags := types.ListValueFrom(ctx, volumeSourceSnapshotObjectType, snapshots)
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics building a source_snapshot list: %v", diags)
	}
	return list
}

// requireAttributeError asserts that diags carries exactly one error, that it is
// attached to attrPath, and that its detail names each of substrs. The path matters as
// much as the text: a diagnostic on the wrong attribute still fails a plan, but points
// the reader at the wrong line of their configuration.
func requireAttributeError(t *testing.T, diags diag.Diagnostics, attrPath path.Path, substrs ...string) {
	t.Helper()
	errs := diags.Errors()
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want 1: %v", len(errs), diags)
	}
	withPath, ok := errs[0].(diag.DiagnosticWithPath)
	if !ok {
		t.Fatalf("diagnostic %v carries no attribute path, want one at %s", errs[0], attrPath)
	}
	if !withPath.Path().Equal(attrPath) {
		t.Fatalf("diagnostic is at %s, want %s", withPath.Path(), attrPath)
	}
	for _, substr := range substrs {
		requireContains(t, errs[0].Detail(), substr)
	}
}

func TestIsDefinitelySet(t *testing.T) {
	ctx := context.Background()

	t.Run("string", func(t *testing.T) {
		tests := []struct {
			name string
			v    types.String
			want bool
		}{
			{"null is not set", types.StringNull(), false},
			{"unknown is not set", types.StringUnknown(), false},
			{"a set value is set", types.StringValue("x"), true},
			{"an empty string is still set — it isn't a list, so emptiness carries no meaning", types.StringValue(""), true},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if got := isDefinitelySet(tt.v); got != tt.want {
					t.Fatalf("isDefinitelySet(%v) = %v, want %v", tt.v, got, tt.want)
				}
			})
		}
	})

	t.Run("list (how Terraform represents a block)", func(t *testing.T) {
		populated := snapshotList(t, ctx, VolumeSourceSnapshotModel{
			ID: armids.IDValue{StringValue: types.StringValue("/id/snap")}, VolumeName: types.StringValue("vol0"),
		})
		tests := []struct {
			name string
			v    types.List
			want bool
		}{
			{"a null list (the block was never configured) is not set", types.ListNull(volumeSourceSnapshotObjectType), false},
			{"a known, empty list (the block appears zero times) is not set", snapshotList(t, ctx), false},
			{"an unknown list is not set — it hasn't been proven present yet", types.ListUnknown(volumeSourceSnapshotObjectType), false},
			{"a one-element list is set", populated, true},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if got := isDefinitelySet(tt.v); got != tt.want {
					t.Fatalf("isDefinitelySet(%v) = %v, want %v", tt.v, got, tt.want)
				}
			})
		}
	})
}

func TestIsDefinitelyAbsent(t *testing.T) {
	ctx := context.Background()

	t.Run("string", func(t *testing.T) {
		tests := []struct {
			name string
			v    types.String
			want bool
		}{
			{"null is absent", types.StringNull(), true},
			{"unknown is not (yet) definitely absent — it may still resolve to a value", types.StringUnknown(), false},
			{"a set value is not absent", types.StringValue("x"), false},
			{"an empty string is not absent — a real value was configured", types.StringValue(""), false},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if got := isDefinitelyAbsent(tt.v); got != tt.want {
					t.Fatalf("isDefinitelyAbsent(%v) = %v, want %v", tt.v, got, tt.want)
				}
			})
		}
	})

	t.Run("list (how Terraform represents a block)", func(t *testing.T) {
		populated := snapshotList(t, ctx, VolumeSourceSnapshotModel{
			ID: armids.IDValue{StringValue: types.StringValue("/id/snap")}, VolumeName: types.StringValue("vol0"),
		})
		tests := []struct {
			name string
			v    types.List
			want bool
		}{
			{"a null list is absent", types.ListNull(volumeSourceSnapshotObjectType), true},
			{"a known, empty list (the block appears zero times) is absent", snapshotList(t, ctx), true},
			{"an unknown list is not definitely absent", types.ListUnknown(volumeSourceSnapshotObjectType), false},
			{"a one-element list is not absent", populated, false},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if got := isDefinitelyAbsent(tt.v); got != tt.want {
					t.Fatalf("isDefinitelyAbsent(%v) = %v, want %v", tt.v, got, tt.want)
				}
			})
		}
	})

	t.Run("an unknown list satisfies neither predicate, so a required-input check waits rather than rejecting it", func(t *testing.T) {
		v := types.ListUnknown(volumeSourceSnapshotObjectType)
		if isDefinitelySet(v) {
			t.Fatal("isDefinitelySet(unknown list) = true, want false")
		}
		if isDefinitelyAbsent(v) {
			t.Fatal("isDefinitelyAbsent(unknown list) = true, want false")
		}
	})
}

func TestVolumeSourceTypeOrDefault(t *testing.T) {
	tests := []struct {
		name string
		v    types.String
		want string
	}{
		{"null resolves to none", types.StringNull(), volumeSourceTypeNone},
		{"an empty string resolves to none", types.StringValue(""), volumeSourceTypeNone},
		{"an explicit none stays none", types.StringValue(volumeSourceTypeNone), volumeSourceTypeNone},
		{"a real source type passes through unchanged", types.StringValue(volumeSourceTypeVolume), volumeSourceTypeVolume},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := volumeSourceTypeOrDefault(tt.v); got != tt.want {
				t.Fatalf("volumeSourceTypeOrDefault(%v) = %q, want %q", tt.v, got, tt.want)
			}
		})
	}
}

func TestVolumeSourceRowFor(t *testing.T) {
	t.Run("none names no row", func(t *testing.T) {
		if _, ok := volumeSourceRowFor(volumeSourceTypeNone); ok {
			t.Fatal("expected ok = false for \"none\"")
		}
	})

	t.Run("an unrecognized source type names no row (the schema's OneOf validator reports it instead)", func(t *testing.T) {
		if _, ok := volumeSourceRowFor("bogus"); ok {
			t.Fatal("expected ok = false for an unrecognized source type")
		}
	})

	tests := []struct {
		sourceType string
		wantPath   string
	}{
		{volumeSourceTypeVolume, "source_volume_id"},
		{volumeSourceTypeSerialNumber, "source_serial_number"},
		{volumeSourceTypeSnapshot, "source_snapshot"},
		{volumeSourceTypeRecoverableVolume, "source_recoverable_volume_id"},
	}
	for _, tt := range tests {
		t.Run(tt.sourceType, func(t *testing.T) {
			row, ok := volumeSourceRowFor(tt.sourceType)
			if !ok {
				t.Fatalf("expected a row for %q", tt.sourceType)
			}
			if row.attribute.String() != tt.wantPath {
				t.Fatalf("attribute = %v, want %v", row.attribute, tt.wantPath)
			}
		})
	}
}

func TestVolumeSourceTypeValues(t *testing.T) {
	got := volumeSourceTypeValues()
	want := []string{
		volumeSourceTypeNone, volumeSourceTypeVolume, volumeSourceTypeSerialNumber,
		volumeSourceTypeSnapshot, volumeSourceTypeRecoverableVolume,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (order matters: \"none\" must come first)", got, want)
		}
	}
}

func TestMissingVolumeSourceAttribute(t *testing.T) {
	ctx := context.Background()

	t.Run("source_type none is never missing, regardless of the model", func(t *testing.T) {
		_, missing := missingVolumeSourceAttribute(volumeSourceTypeNone, VolumeResourceModel{})
		if missing {
			t.Fatal("expected missing = false")
		}
	})

	t.Run("an invalid source type is never missing — the OneOf validator reports it instead", func(t *testing.T) {
		_, missing := missingVolumeSourceAttribute("bogus", VolumeResourceModel{})
		if missing {
			t.Fatal("expected missing = false")
		}
	})

	t.Run("a null source_volume_id is missing", func(t *testing.T) {
		data := VolumeResourceModel{SourceVolumeID: armids.IDValue{StringValue: types.StringNull()}}
		wantPath, missing := missingVolumeSourceAttribute(volumeSourceTypeVolume, data)
		if !missing {
			t.Fatal("expected missing = true")
		}
		if wantPath.String() != "source_volume_id" {
			t.Fatalf("wantPath = %v, want source_volume_id", wantPath)
		}
	})

	t.Run("an unknown source_volume_id is not missing yet — Create rechecks once it resolves", func(t *testing.T) {
		data := VolumeResourceModel{SourceVolumeID: armids.IDValue{StringValue: types.StringUnknown()}}
		_, missing := missingVolumeSourceAttribute(volumeSourceTypeVolume, data)
		if missing {
			t.Fatal("expected missing = false")
		}
	})

	t.Run("a set source_volume_id is not missing", func(t *testing.T) {
		data := VolumeResourceModel{SourceVolumeID: armids.IDValue{StringValue: types.StringValue("/id/vol0")}}
		_, missing := missingVolumeSourceAttribute(volumeSourceTypeVolume, data)
		if missing {
			t.Fatal("expected missing = false")
		}
	})

	t.Run("a null source_snapshot block is missing", func(t *testing.T) {
		data := VolumeResourceModel{SourceSnapshot: types.ListNull(volumeSourceSnapshotObjectType)}
		wantPath, missing := missingVolumeSourceAttribute(volumeSourceTypeSnapshot, data)
		if !missing || wantPath.String() != "source_snapshot" {
			t.Fatalf("got missing=%v path=%v, want missing=true path=source_snapshot", missing, wantPath)
		}
	})

	t.Run("a known but empty source_snapshot block is missing — a block appearing zero times is absent", func(t *testing.T) {
		data := VolumeResourceModel{SourceSnapshot: snapshotList(t, ctx)}
		_, missing := missingVolumeSourceAttribute(volumeSourceTypeSnapshot, data)
		if !missing {
			t.Fatal("expected missing = true for a zero-element block")
		}
	})

	t.Run("a populated source_snapshot block is not missing", func(t *testing.T) {
		data := VolumeResourceModel{SourceSnapshot: snapshotList(t, ctx, VolumeSourceSnapshotModel{
			ID: armids.IDValue{StringValue: types.StringValue("/id/snap")}, VolumeName: types.StringValue("vol0"),
		})}
		_, missing := missingVolumeSourceAttribute(volumeSourceTypeSnapshot, data)
		if missing {
			t.Fatal("expected missing = false")
		}
	})
}

// TestResolveVolumeSizeInBytes exercises the single definition of how the two size
// attributes combine — the most dangerous function in this resource, since a wrong
// answer either destroys data (a false shrink-to-zero) or silently skips a resize the
// user asked for.
func TestResolveVolumeSizeInBytes(t *testing.T) {
	t.Run("a known, non-null gib always wins, whatever the byte side holds", func(t *testing.T) {
		tests := []struct {
			name  string
			bytes types.Int64
		}{
			{"bytes null", types.Int64Null()},
			{"bytes set to an unrelated value", types.Int64Value(999)},
			{"bytes unknown", types.Int64Unknown()},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				data := VolumeResourceModel{ProvisionedSizeInGiB: types.Int64Value(1), ProvisionedSizeInBytes: tt.bytes}
				got := resolveVolumeSizeInBytes(data)
				if got.IsUnknown() || got.IsNull() || got.ValueInt64() != bytesPerGiB {
					t.Fatalf("resolveVolumeSizeInBytes() = %v, want a known %d", got, int64(bytesPerGiB))
				}
			})
		}
	})

	t.Run("multiplies out to the exact byte counts a whole-GiB config must produce", func(t *testing.T) {
		tests := []struct {
			gib  int64
			want int64
		}{
			{1, 1073741824},
			{100, 107374182400},
			{4096, 4398046511104}, // a large value, still exact
		}
		for _, tt := range tests {
			data := VolumeResourceModel{ProvisionedSizeInGiB: types.Int64Value(tt.gib), ProvisionedSizeInBytes: types.Int64Null()}
			got := resolveVolumeSizeInBytes(data)
			if got.ValueInt64() != tt.want {
				t.Fatalf("resolveVolumeSizeInBytes(gib=%d) = %d, want %d", tt.gib, got.ValueInt64(), tt.want)
			}
		}
	})

	t.Run("a null gib leaves provisioned_size_in_bytes exactly as it stands", func(t *testing.T) {
		// The regression this pins: without this exact behavior, deleting a
		// "provisioned_size_in_gib = N" line from config would read the resulting
		// null as 0 and plan a shrink to zero, destroying the volume's data with
		// nothing more than a one-line config edit.
		tests := []struct {
			name  string
			bytes types.Int64
		}{
			{"a carried-forward byte value passes through untouched", types.Int64Value(5 * bytesPerGiB)},
			{"a null byte value stays null, not zero", types.Int64Null()},
			{"an unknown byte value stays unknown", types.Int64Unknown()},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				data := VolumeResourceModel{ProvisionedSizeInGiB: types.Int64Null(), ProvisionedSizeInBytes: tt.bytes}
				got := resolveVolumeSizeInBytes(data)
				if !got.Equal(tt.bytes) {
					t.Fatalf("resolveVolumeSizeInBytes() = %v, want the byte value unchanged (%v)", got, tt.bytes)
				}
			})
		}
	})

	t.Run("an unknown gib resolves to an unknown size, never to the prior byte value", func(t *testing.T) {
		// The regression this guards: pinning an unresolved gib to the old size
		// would let a plan claim "no change" and then skip a resize the apply-time
		// value actually calls for.
		tests := []struct {
			name  string
			bytes types.Int64
		}{
			{"bytes null", types.Int64Null()},
			{"bytes set to the prior state's size", types.Int64Value(5 * bytesPerGiB)},
			{"bytes also unknown", types.Int64Unknown()},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				data := VolumeResourceModel{ProvisionedSizeInGiB: types.Int64Unknown(), ProvisionedSizeInBytes: tt.bytes}
				got := resolveVolumeSizeInBytes(data)
				if !got.IsUnknown() {
					t.Fatalf("resolveVolumeSizeInBytes() = %v, want unknown — not the prior byte value %v", got, tt.bytes)
				}
			})
		}
	})
}

func TestCheckVolumeSizePresence(t *testing.T) {
	base := func() VolumeResourceModel {
		return VolumeResourceModel{
			SourceType:             types.StringValue(volumeSourceTypeNone),
			ProvisionedSizeInGiB:   types.Int64Null(),
			ProvisionedSizeInBytes: types.Int64Null(),
		}
	}

	t.Run("none with neither size set is an error", func(t *testing.T) {
		if diags := checkVolumeSizePresence(base()); !diags.HasError() {
			t.Fatal("expected an error")
		}
	})

	t.Run("none with only gib set is accepted", func(t *testing.T) {
		data := base()
		data.ProvisionedSizeInGiB = types.Int64Value(10)
		if diags := checkVolumeSizePresence(data); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
	})

	t.Run("none with only bytes set is accepted", func(t *testing.T) {
		data := base()
		data.ProvisionedSizeInBytes = types.Int64Value(1048576)
		if diags := checkVolumeSizePresence(data); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
	})

	t.Run("none with both set is left to the schema's ConflictsWith validator, not flagged here", func(t *testing.T) {
		data := base()
		data.ProvisionedSizeInGiB = types.Int64Value(10)
		data.ProvisionedSizeInBytes = types.Int64Value(1048576)
		if diags := checkVolumeSizePresence(data); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
	})

	for _, sourceType := range []string{
		volumeSourceTypeVolume, volumeSourceTypeSerialNumber, volumeSourceTypeSnapshot, volumeSourceTypeRecoverableVolume,
	} {
		t.Run(sourceType+": a gib value is an error", func(t *testing.T) {
			data := base()
			data.SourceType = types.StringValue(sourceType)
			data.ProvisionedSizeInGiB = types.Int64Value(10)
			if diags := checkVolumeSizePresence(data); !diags.HasError() {
				t.Fatalf("expected an error for source_type = %q with a size set", sourceType)
			}
		})

		t.Run(sourceType+": a bytes value is an error", func(t *testing.T) {
			data := base()
			data.SourceType = types.StringValue(sourceType)
			data.ProvisionedSizeInBytes = types.Int64Value(1048576)
			if diags := checkVolumeSizePresence(data); !diags.HasError() {
				t.Fatalf("expected an error for source_type = %q with a size set", sourceType)
			}
		})

		t.Run(sourceType+": neither size set is accepted", func(t *testing.T) {
			data := base()
			data.SourceType = types.StringValue(sourceType)
			if diags := checkVolumeSizePresence(data); diags.HasError() {
				t.Fatalf("unexpected diagnostics: %v", diags)
			}
		})
	}

	t.Run("an unknown source_type defers to Create's own recheck", func(t *testing.T) {
		data := base()
		data.SourceType = types.StringUnknown()
		if diags := checkVolumeSizePresence(data); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
	})

	t.Run("an unknown gib defers to Create's own recheck, even for source_type none with no bytes set", func(t *testing.T) {
		data := base()
		data.ProvisionedSizeInGiB = types.Int64Unknown()
		if diags := checkVolumeSizePresence(data); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
	})
}

func TestCheckVolumeSourcePool(t *testing.T) {
	sourcePath := path.Root("source_volume_id")

	t.Run("the same pool is accepted", func(t *testing.T) {
		if diags := checkVolumeSourcePool(testVolumeGroupIDFixture, &armids.TypeVolume, testVolumeIDSamePool, sourcePath); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
	})

	t.Run("the same pool, different casing is accepted — ARM treats resource IDs case-insensitively", func(t *testing.T) {
		if diags := checkVolumeSourcePool(strings.ToUpper(testVolumeGroupIDFixture), &armids.TypeVolume, testVolumeIDSamePool, sourcePath); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
	})

	t.Run("a different pool is rejected, naming both pools and the given attribute", func(t *testing.T) {
		diags := checkVolumeSourcePool(testVolumeGroupIDFixture, &armids.TypeVolume, testVolumeIDOtherPool, sourcePath)
		if !diags.HasError() {
			t.Fatal("expected an error")
		}
		requireAttributeError(t, diags, sourcePath, "source_volume_id", "pool2", "pool1")
	})

	t.Run("a malformed volume_group_id is reported against volume_group_id itself", func(t *testing.T) {
		diags := checkVolumeSourcePool("not-an-id", &armids.TypeVolume, testVolumeIDSamePool, sourcePath)
		if !diags.HasError() {
			t.Fatal("expected an error")
		}
		requireAttributeError(t, diags, path.Root("volume_group_id"), "not-an-id")
	})

	// The source ID's own schema validator never sees a value that was unknown when
	// the config was validated, and nothing revalidates it once apply resolves it, so
	// this check is the only place a source ID read from another resource's output is
	// ever parsed. Reporting it here is what makes that case an error rather than a
	// silently skipped check.
	t.Run("a malformed source ID is reported against the source attribute", func(t *testing.T) {
		diags := checkVolumeSourcePool(testVolumeGroupIDFixture, &armids.TypeVolume, "not-an-id", sourcePath)
		if !diags.HasError() {
			t.Fatal("expected an error")
		}
		requireAttributeError(t, diags, sourcePath, "not-an-id")
	})
}

func TestCheckVolumeSourceGroup(t *testing.T) {
	sourcePath := path.Root("source_recoverable_volume_id")

	t.Run("the same volume group is accepted", func(t *testing.T) {
		if diags := checkVolumeSourceGroup(testVolumeGroupIDFixture, testVolumeIDInVG); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
	})

	t.Run("the same volume group, different casing is accepted", func(t *testing.T) {
		if diags := checkVolumeSourceGroup(strings.ToUpper(testVolumeGroupIDFixture), testVolumeIDInVG); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
	})

	t.Run("a different volume group is rejected, naming both groups", func(t *testing.T) {
		diags := checkVolumeSourceGroup(testVolumeGroupIDFixture, testVolumeIDSamePool)
		if !diags.HasError() {
			t.Fatal("expected an error")
		}
		requireAttributeError(t, diags, sourcePath, testVolumeGroupID2Fixture, testVolumeGroupIDFixture)
	})

	// Nothing else on the create path reads source_recoverable_volume_id, so this is
	// the only check that can report a malformed one that the schema validator never
	// saw because it was still unknown at validate time.
	t.Run("a malformed source ID is reported against the source attribute", func(t *testing.T) {
		diags := checkVolumeSourceGroup(testVolumeGroupIDFixture, "not-an-id")
		if !diags.HasError() {
			t.Fatal("expected an error")
		}
		requireAttributeError(t, diags, sourcePath, "not-an-id")
	})
}

func TestCheckVolumeSourceRules(t *testing.T) {
	ctx := context.Background()

	t.Run("an unknown volume_group_id skips every check", func(t *testing.T) {
		data := VolumeResourceModel{VolumeGroupID: armids.IDValue{StringValue: types.StringUnknown()}}
		if diags := checkVolumeSourceRules(ctx, volumeSourceTypeVolume, data); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
	})

	t.Run("source_type volume: the same pool passes", func(t *testing.T) {
		data := VolumeResourceModel{VolumeGroupID: armids.IDValue{StringValue: types.StringValue(testVolumeGroupIDFixture)}, SourceVolumeID: armids.IDValue{StringValue: types.StringValue(testVolumeIDSamePool)}}
		if diags := checkVolumeSourceRules(ctx, volumeSourceTypeVolume, data); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
	})

	t.Run("source_type volume: a cross-pool source fails", func(t *testing.T) {
		data := VolumeResourceModel{VolumeGroupID: armids.IDValue{StringValue: types.StringValue(testVolumeGroupIDFixture)}, SourceVolumeID: armids.IDValue{StringValue: types.StringValue(testVolumeIDOtherPool)}}
		if diags := checkVolumeSourceRules(ctx, volumeSourceTypeVolume, data); !diags.HasError() {
			t.Fatal("expected an error")
		}
	})

	// A source ID that was unknown when the config was validated reaches Create having
	// never passed its own schema validator, so these two cases are the only report a
	// malformed one gets.
	t.Run("source_type volume: a malformed source_volume_id is reported, not skipped", func(t *testing.T) {
		data := VolumeResourceModel{VolumeGroupID: armids.IDValue{StringValue: types.StringValue(testVolumeGroupIDFixture)}, SourceVolumeID: armids.IDValue{StringValue: types.StringValue("not-an-id")}}
		diags := checkVolumeSourceRules(ctx, volumeSourceTypeVolume, data)
		requireAttributeError(t, diags, path.Root("source_volume_id"), "not-an-id")
	})

	t.Run("source_type recoverableVolume: a malformed source_recoverable_volume_id is reported, not skipped", func(t *testing.T) {
		data := VolumeResourceModel{
			VolumeGroupID:             armids.IDValue{StringValue: types.StringValue(testVolumeGroupIDFixture)},
			SourceRecoverableVolumeID: armids.IDValue{StringValue: types.StringValue("not-an-id")},
		}
		diags := checkVolumeSourceRules(ctx, volumeSourceTypeRecoverableVolume, data)
		requireAttributeError(t, diags, path.Root("source_recoverable_volume_id"), "not-an-id")
	})

	t.Run("source_type volume: an unknown source_volume_id skips the check", func(t *testing.T) {
		data := VolumeResourceModel{VolumeGroupID: armids.IDValue{StringValue: types.StringValue(testVolumeGroupIDFixture)}, SourceVolumeID: armids.IDValue{StringValue: types.StringUnknown()}}
		if diags := checkVolumeSourceRules(ctx, volumeSourceTypeVolume, data); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
	})

	t.Run("source_type snapshot: the block's id is checked against the same pool", func(t *testing.T) {
		data := VolumeResourceModel{
			VolumeGroupID: armids.IDValue{StringValue: types.StringValue(testVolumeGroupIDFixture)},
			SourceSnapshot: snapshotList(t, ctx, VolumeSourceSnapshotModel{
				ID: armids.IDValue{StringValue: types.StringValue(testSnapshotIDOtherPool)}, VolumeName: types.StringValue("vol0"),
			}),
		}
		if diags := checkVolumeSourceRules(ctx, volumeSourceTypeSnapshot, data); !diags.HasError() {
			t.Fatal("expected an error for a snapshot in a different storage pool")
		}
	})

	t.Run("source_type snapshot: an unknown block skips the check", func(t *testing.T) {
		data := VolumeResourceModel{
			VolumeGroupID:  armids.IDValue{StringValue: types.StringValue(testVolumeGroupIDFixture)},
			SourceSnapshot: types.ListUnknown(volumeSourceSnapshotObjectType),
		}
		if diags := checkVolumeSourceRules(ctx, volumeSourceTypeSnapshot, data); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
	})

	t.Run("source_type recoverableVolume: the same volume group passes", func(t *testing.T) {
		data := VolumeResourceModel{
			VolumeGroupID:             armids.IDValue{StringValue: types.StringValue(testVolumeGroupIDFixture)},
			SourceRecoverableVolumeID: armids.IDValue{StringValue: types.StringValue(testVolumeIDInVG)},
		}
		if diags := checkVolumeSourceRules(ctx, volumeSourceTypeRecoverableVolume, data); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
	})

	t.Run("source_type recoverableVolume: a different volume group fails", func(t *testing.T) {
		data := VolumeResourceModel{
			VolumeGroupID:             armids.IDValue{StringValue: types.StringValue(testVolumeGroupIDFixture)},
			SourceRecoverableVolumeID: armids.IDValue{StringValue: types.StringValue(testVolumeIDSamePool)},
		}
		if diags := checkVolumeSourceRules(ctx, volumeSourceTypeRecoverableVolume, data); !diags.HasError() {
			t.Fatal("expected an error")
		}
	})

	t.Run("source_type none or serialNumber name no source ID to check", func(t *testing.T) {
		for _, st := range []string{volumeSourceTypeNone, volumeSourceTypeSerialNumber} {
			data := VolumeResourceModel{VolumeGroupID: armids.IDValue{StringValue: types.StringValue(testVolumeGroupIDFixture)}}
			if diags := checkVolumeSourceRules(ctx, st, data); diags.HasError() {
				t.Fatalf("unexpected diagnostics for source_type=%q: %v", st, diags)
			}
		}
	})
}

func TestDeriveSourceVolumeGroupID(t *testing.T) {
	ctx := context.Background()

	t.Run("from a volume ID, casing preserved by cutting rather than parse-and-rebuild", func(t *testing.T) {
		id := "/Subscriptions/11111111-1111-1111-1111-111111111111/ResourceGroups/RG/providers/PureStorage.Block/" +
			"StoragePools/Pool1/VolumeGroups/VG1/Volumes/Vol0"
		want := "/Subscriptions/11111111-1111-1111-1111-111111111111/ResourceGroups/RG/providers/PureStorage.Block/" +
			"StoragePools/Pool1/VolumeGroups/VG1"
		data := VolumeResourceModel{SourceVolumeID: armids.IDValue{StringValue: types.StringValue(id)}}
		got, diags := deriveSourceVolumeGroupID(ctx, volumeSourceTypeVolume, data)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if got.ValueString() != want {
			t.Fatalf("got %q, want %q", got.ValueString(), want)
		}
	})

	t.Run("from a snapshot block's id", func(t *testing.T) {
		snapID := testVolumeGroupIDFixture + "/snapshots/snap1"
		data := VolumeResourceModel{SourceSnapshot: snapshotList(t, ctx, VolumeSourceSnapshotModel{
			ID: armids.IDValue{StringValue: types.StringValue(snapID)}, VolumeName: types.StringValue("vol0"),
		})}
		got, diags := deriveSourceVolumeGroupID(ctx, volumeSourceTypeSnapshot, data)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if got.ValueString() != testVolumeGroupIDFixture {
			t.Fatalf("got %q, want %q", got.ValueString(), testVolumeGroupIDFixture)
		}
	})

	t.Run("a malformed source_volume_id reports an attribute error instead of a garbled parent ID", func(t *testing.T) {
		data := VolumeResourceModel{SourceVolumeID: armids.IDValue{StringValue: types.StringValue("not-an-id")}}
		got, diags := deriveSourceVolumeGroupID(ctx, volumeSourceTypeVolume, data)
		if !diags.HasError() {
			t.Fatal("expected an error diagnostic")
		}
		if !got.IsNull() {
			t.Fatalf("got %v, want null on error", got)
		}
	})

	t.Run("a missing source_snapshot block reports an attribute error", func(t *testing.T) {
		data := VolumeResourceModel{SourceSnapshot: types.ListNull(volumeSourceSnapshotObjectType)}
		if _, diags := deriveSourceVolumeGroupID(ctx, volumeSourceTypeSnapshot, data); !diags.HasError() {
			t.Fatal("expected an error diagnostic")
		}
	})

	t.Run("a malformed source_snapshot id reports an attribute error", func(t *testing.T) {
		data := VolumeResourceModel{SourceSnapshot: snapshotList(t, ctx, VolumeSourceSnapshotModel{
			ID: armids.IDValue{StringValue: types.StringValue("not-an-id")}, VolumeName: types.StringValue("vol0"),
		})}
		if _, diags := deriveSourceVolumeGroupID(ctx, volumeSourceTypeSnapshot, data); !diags.HasError() {
			t.Fatal("expected an error diagnostic")
		}
	})

	for _, st := range []string{volumeSourceTypeNone, volumeSourceTypeSerialNumber, volumeSourceTypeRecoverableVolume} {
		t.Run(st+" has no source volume group of its own, so the result is null", func(t *testing.T) {
			got, diags := deriveSourceVolumeGroupID(ctx, st, VolumeResourceModel{})
			if diags.HasError() {
				t.Fatalf("unexpected diagnostics: %v", diags)
			}
			if !got.IsNull() {
				t.Fatalf("got %v, want null", got)
			}
		})
	}

	t.Run("the value toVolume derives matches deriveSourceVolumeGroupID called directly", func(t *testing.T) {
		data := VolumeResourceModel{
			SourceType:     types.StringValue(volumeSourceTypeVolume),
			SourceVolumeID: armids.IDValue{StringValue: types.StringValue(testVolumeIDSamePool)},
		}
		derived, diags := deriveSourceVolumeGroupID(ctx, volumeSourceTypeVolume, data)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}

		vol, diags := toVolume(ctx, data)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if vol.Properties.SourceVolumeGroupResourceID == nil || *vol.Properties.SourceVolumeGroupResourceID != derived.ValueString() {
			t.Fatalf(
				"SourceVolumeGroupResourceID = %v, want %q (the value deriveSourceVolumeGroupID computes directly)",
				vol.Properties.SourceVolumeGroupResourceID, derived.ValueString(),
			)
		}
	})
}

func TestToVolume(t *testing.T) {
	ctx := context.Background()

	t.Run("source_type none sends an explicit sourceType \"none\" plus the size — unlike the volume group's omit-the-field workaround", func(t *testing.T) {
		data := VolumeResourceModel{
			SourceType:             types.StringValue(volumeSourceTypeNone),
			ProvisionedSizeInBytes: types.Int64Value(1073741824),
		}
		vol, diags := toVolume(ctx, data)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if vol.Properties.SourceType == nil || *vol.Properties.SourceType != armpurestorageblock.VolumeSourceTypeNone {
			t.Fatalf("SourceType = %v, want an explicit %q", vol.Properties.SourceType, armpurestorageblock.VolumeSourceTypeNone)
		}
		if vol.Properties.ProvisionedSize == nil || *vol.Properties.ProvisionedSize != 1073741824 {
			t.Fatalf("ProvisionedSize = %v, want 1073741824", vol.Properties.ProvisionedSize)
		}
	})

	t.Run("a null source_type also sends an explicit \"none\", via volumeSourceTypeOrDefault", func(t *testing.T) {
		data := VolumeResourceModel{SourceType: types.StringNull(), ProvisionedSizeInBytes: types.Int64Value(1073741824)}
		vol, diags := toVolume(ctx, data)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if vol.Properties.SourceType == nil || *vol.Properties.SourceType != armpurestorageblock.VolumeSourceTypeNone {
			t.Fatalf("SourceType = %v, want an explicit %q", vol.Properties.SourceType, armpurestorageblock.VolumeSourceTypeNone)
		}
	})

	t.Run("source_type volume: sets sourceVolumeResourceId and derives the group id from it, no size", func(t *testing.T) {
		sourceVolumeID := testVolumeIDSamePool
		wantGroupID, err := armids.TypeVolume.ParentIDOf(sourceVolumeID)
		if err != nil {
			t.Fatalf("test fixture %q is not a valid volume ID", sourceVolumeID)
		}
		data := VolumeResourceModel{
			SourceType:     types.StringValue(volumeSourceTypeVolume),
			SourceVolumeID: armids.IDValue{StringValue: types.StringValue(sourceVolumeID)},
		}
		vol, diags := toVolume(ctx, data)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if vol.Properties.SourceVolumeResourceID == nil || *vol.Properties.SourceVolumeResourceID != sourceVolumeID {
			t.Fatalf("SourceVolumeResourceID = %v", vol.Properties.SourceVolumeResourceID)
		}
		if vol.Properties.SourceVolumeGroupResourceID == nil || *vol.Properties.SourceVolumeGroupResourceID != wantGroupID {
			t.Fatalf("SourceVolumeGroupResourceID = %v, want %q", vol.Properties.SourceVolumeGroupResourceID, wantGroupID)
		}
		if vol.Properties.ProvisionedSize != nil {
			t.Fatalf("ProvisionedSize = %v, want nil — the clone inherits its source's size", vol.Properties.ProvisionedSize)
		}
	})

	t.Run("source_type volume: a malformed source_volume_id surfaces deriveSourceVolumeGroupID's diagnostic", func(t *testing.T) {
		data := VolumeResourceModel{
			SourceType:     types.StringValue(volumeSourceTypeVolume),
			SourceVolumeID: armids.IDValue{StringValue: types.StringValue("not-an-id")},
		}
		if _, diags := toVolume(ctx, data); !diags.HasError() {
			t.Fatal("expected an error diagnostic")
		}
	})

	t.Run("source_type serialNumber: sets sourceSerialNumber, no size", func(t *testing.T) {
		data := VolumeResourceModel{
			SourceType:         types.StringValue(volumeSourceTypeSerialNumber),
			SourceSerialNumber: types.StringValue("SERIAL123"),
		}
		vol, diags := toVolume(ctx, data)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if vol.Properties.SourceSerialNumber == nil || *vol.Properties.SourceSerialNumber != "SERIAL123" {
			t.Fatalf("SourceSerialNumber = %v", vol.Properties.SourceSerialNumber)
		}
		if vol.Properties.ProvisionedSize != nil {
			t.Fatalf("ProvisionedSize = %v, want nil", vol.Properties.ProvisionedSize)
		}
	})

	t.Run("source_type snapshot: sets the block's pair and derives the group id from it, no size", func(t *testing.T) {
		snapID := testVolumeGroupIDFixture + "/snapshots/snap1"
		data := VolumeResourceModel{
			SourceType: types.StringValue(volumeSourceTypeSnapshot),
			SourceSnapshot: snapshotList(t, ctx, VolumeSourceSnapshotModel{
				ID: armids.IDValue{StringValue: types.StringValue(snapID)}, VolumeName: types.StringValue("vol0"),
			}),
		}
		vol, diags := toVolume(ctx, data)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if vol.Properties.SourceVolumeSnapshot == nil {
			t.Fatal("expected a non-nil SourceVolumeSnapshot")
		}
		if vol.Properties.SourceVolumeSnapshot.VolumeGroupSnapshotResourceID == nil ||
			*vol.Properties.SourceVolumeSnapshot.VolumeGroupSnapshotResourceID != snapID {
			t.Fatalf("VolumeGroupSnapshotResourceID = %v", vol.Properties.SourceVolumeSnapshot.VolumeGroupSnapshotResourceID)
		}
		if vol.Properties.SourceVolumeSnapshot.VolumeSnapshotName == nil || *vol.Properties.SourceVolumeSnapshot.VolumeSnapshotName != "vol0" {
			t.Fatalf("VolumeSnapshotName = %v", vol.Properties.SourceVolumeSnapshot.VolumeSnapshotName)
		}
		if vol.Properties.SourceVolumeGroupResourceID == nil || *vol.Properties.SourceVolumeGroupResourceID != testVolumeGroupIDFixture {
			t.Fatalf("SourceVolumeGroupResourceID = %v, want %q", vol.Properties.SourceVolumeGroupResourceID, testVolumeGroupIDFixture)
		}
		if vol.Properties.ProvisionedSize != nil {
			t.Fatalf("ProvisionedSize = %v, want nil", vol.Properties.ProvisionedSize)
		}
	})

	t.Run("source_type snapshot: a missing source_snapshot block surfaces deriveSourceVolumeGroupID's diagnostic", func(t *testing.T) {
		data := VolumeResourceModel{
			SourceType:     types.StringValue(volumeSourceTypeSnapshot),
			SourceSnapshot: types.ListNull(volumeSourceSnapshotObjectType),
		}
		if _, diags := toVolume(ctx, data); !diags.HasError() {
			t.Fatal("expected an error diagnostic")
		}
	})

	t.Run("source_type recoverableVolume: sets sourceRecoverableVolumeResourceId, no size", func(t *testing.T) {
		data := VolumeResourceModel{
			SourceType:                types.StringValue(volumeSourceTypeRecoverableVolume),
			SourceRecoverableVolumeID: armids.IDValue{StringValue: types.StringValue("/id/soft-deleted-vol")},
		}
		vol, diags := toVolume(ctx, data)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if vol.Properties.SourceRecoverableVolumeResourceID == nil || *vol.Properties.SourceRecoverableVolumeResourceID != "/id/soft-deleted-vol" {
			t.Fatalf("SourceRecoverableVolumeResourceID = %v", vol.Properties.SourceRecoverableVolumeResourceID)
		}
		if vol.Properties.ProvisionedSize != nil {
			t.Fatalf("ProvisionedSize = %v, want nil", vol.Properties.ProvisionedSize)
		}
	})
}

func TestToVolumeUpdate(t *testing.T) {
	data := VolumeResourceModel{ProvisionedSizeInBytes: types.Int64Value(2147483648)}
	update := toVolumeUpdate(data)
	if update.Properties == nil || update.Properties.ProvisionedSize == nil || *update.Properties.ProvisionedSize != 2147483648 {
		t.Fatalf("ProvisionedSize = %v, want 2147483648", update.Properties)
	}
}

func TestApplyVolume(t *testing.T) {
	t.Run("returns an error instead of panicking when the API response has no properties", func(t *testing.T) {
		data := &VolumeResourceModel{}
		if diags := applyVolume(data, armpurestorageblock.Volume{Properties: nil}); !diags.HasError() {
			t.Fatal("expected an error diagnostic")
		}
	})

	t.Run("maps id, provisioned_size_in_bytes and serial_number from the response", func(t *testing.T) {
		data := &VolumeResourceModel{}
		v := armpurestorageblock.Volume{
			ID: to.Ptr("/id/vol1"),
			Properties: &armpurestorageblock.AzureVolumeProperties{
				ProvisionedSize: to.Ptr(int64(2147483648)),
				SerialNumber:    to.Ptr("SERIAL0000000000000001"),
			},
		}
		if diags := applyVolume(data, v); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if data.ID.ValueString() != "/id/vol1" {
			t.Fatalf("ID = %q, want /id/vol1", data.ID.ValueString())
		}
		if data.ProvisionedSizeInBytes.ValueInt64() != 2147483648 {
			t.Fatalf("ProvisionedSizeInBytes = %v, want 2147483648", data.ProvisionedSizeInBytes)
		}
		if data.SerialNumber.ValueString() != "SERIAL0000000000000001" {
			t.Fatalf("SerialNumber = %q, want SERIAL0000000000000001", data.SerialNumber.ValueString())
		}
	})

	t.Run("never touches name, even when the response carries a different one", func(t *testing.T) {
		// name is Required, so adopting the API's value here would fail the apply
		// with "Provider produced inconsistent result after apply" the moment ARM
		// echoes back a casing, or a name, the practitioner didn't write.
		data := &VolumeResourceModel{Name: types.StringValue("myvolume")}
		v := armpurestorageblock.Volume{
			Name:       to.Ptr("MYVOLUME"),
			Properties: &armpurestorageblock.AzureVolumeProperties{ProvisionedSize: to.Ptr(int64(1073741824))},
		}
		if diags := applyVolume(data, v); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if data.Name.ValueString() != "myvolume" {
			t.Fatalf("Name = %q, want unchanged %q", data.Name.ValueString(), "myvolume")
		}
	})

	t.Run("preserves volume_group_id, every source input, and the deletion attributes", func(t *testing.T) {
		ctx := context.Background()
		snapshot := snapshotList(t, ctx, VolumeSourceSnapshotModel{
			ID: armids.IDValue{StringValue: types.StringValue("/id/snap")}, VolumeName: types.StringValue("vol0"),
		})
		data := &VolumeResourceModel{
			Name:                      types.StringValue("vol1"),
			VolumeGroupID:             armids.IDValue{StringValue: types.StringValue("/id/vg1")},
			ProvisionedSizeInGiB:      types.Int64Value(20),
			SourceType:                types.StringValue(volumeSourceTypeSnapshot),
			SourceVolumeID:            armids.IDValue{StringValue: types.StringValue("/id/unrelated-vol")},
			SourceSerialNumber:        types.StringValue("SOME-SERIAL"),
			SourceRecoverableVolumeID: armids.IDValue{StringValue: types.StringValue("/id/unrelated-recoverable")},
			SourceSnapshot:            snapshot,
			DeletionProtection:        types.BoolValue(true),
			EradicateOnDestroy:        types.BoolValue(true),
		}

		// GET never returns the source* fields (create-only mutability): applyVolume
		// must reproduce every preserved value above without any help from v.
		v := armpurestorageblock.Volume{
			ID:   to.Ptr("/id/vol1"),
			Name: to.Ptr("vol1"),
			Properties: &armpurestorageblock.AzureVolumeProperties{
				ProvisionedSize: to.Ptr(int64(1073741824)),
				SerialNumber:    to.Ptr("SERIAL0000000000000001"),
			},
		}

		if diags := applyVolume(data, v); diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}

		if data.VolumeGroupID.ValueString() != "/id/vg1" {
			t.Fatalf("VolumeGroupID = %q, want unchanged", data.VolumeGroupID.ValueString())
		}
		if data.ProvisionedSizeInGiB.ValueInt64() != 20 {
			t.Fatalf("ProvisionedSizeInGiB = %v, want unchanged (20)", data.ProvisionedSizeInGiB)
		}
		if data.SourceType.ValueString() != volumeSourceTypeSnapshot {
			t.Fatalf("SourceType = %q, want unchanged", data.SourceType.ValueString())
		}
		if data.SourceVolumeID.ValueString() != "/id/unrelated-vol" {
			t.Fatalf("SourceVolumeID = %q, want unchanged", data.SourceVolumeID.ValueString())
		}
		if data.SourceSerialNumber.ValueString() != "SOME-SERIAL" {
			t.Fatalf("SourceSerialNumber = %q, want unchanged", data.SourceSerialNumber.ValueString())
		}
		if data.SourceRecoverableVolumeID.ValueString() != "/id/unrelated-recoverable" {
			t.Fatalf("SourceRecoverableVolumeID = %q, want unchanged", data.SourceRecoverableVolumeID.ValueString())
		}
		gotSnapshots, d := blockElements[VolumeSourceSnapshotModel](ctx, data.SourceSnapshot)
		if d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}
		if len(gotSnapshots) != 1 || gotSnapshots[0].ID.ValueString() != "/id/snap" {
			t.Fatalf("SourceSnapshot = %+v, want unchanged", gotSnapshots)
		}
		if !data.DeletionProtection.ValueBool() || !data.EradicateOnDestroy.ValueBool() {
			t.Fatal("expected deletion_protection and eradicate_on_destroy to stay unchanged")
		}
	})
}
