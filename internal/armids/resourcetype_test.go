// SPDX-License-Identifier: Apache-2.0

package armids

import (
	"regexp"
	"strings"
	"testing"
)

// Golden literal ID regex source strings, captured verbatim from this
// package's predecessor file before the ResourceType table existed. TestIDPatternMatchesGolden
// compares every derived idPattern() against these, so the table-driven
// rewrite is provably behaviour-preserving: composing a regex by
// concatenation is exactly where a subtle break would hide, and these are
// what would catch it. wantFormat has no goldens — it renders a human-read
// example, checked by shape in TestWantFormatShape instead.
const (
	goldenReservationIDPattern = `(?i)^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/PureStorage\.Block/reservations/[^/]+$`

	goldenSubnetIDPattern = `(?i)^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\.Network/virtualNetworks/[^/]+/subnets/[^/]+$`

	goldenStoragePoolIDPattern = `(?i)^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/PureStorage\.Block/storagePools/[^/]+$`

	goldenVolumeGroupIDPattern = `(?i)^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/PureStorage\.Block/storagePools/[^/]+/volumeGroups/[^/]+$`

	goldenVolumeGroupSnapshotIDPattern = `(?i)^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/PureStorage\.Block/storagePools/[^/]+/volumeGroups/[^/]+/snapshots/[^/]+$`

	goldenRecoverableVolumeGroupIDPattern = `(?i)^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/PureStorage\.Block/storagePools/[^/]+/recoverableVolumeGroups/[^/]+$`

	goldenVolumeIDPattern = `(?i)^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/PureStorage\.Block/storagePools/[^/]+/volumeGroups/[^/]+/volumes/[^/]+$`
)

// namedGroupPattern strips a idPattern()-style "(?P<name>X)" back down to
// just "X", recovering the plain, ungrouped shape of the golden literal
// patterns above. idPattern() always adds named capture groups (that's the
// whole reason resourcetype.go composes patterns instead of hand-writing them), so
// it can never be byte-identical to a golden literal without this — comparing
// after stripping is what actually tests the composition logic (escaping,
// anchoring, where (?i) sits) rather than the cosmetic presence of names.
var namedGroupPattern = regexp.MustCompile(`\(\?P<[A-Za-z0-9_]+>([^()]*)\)`)

func stripNamedGroups(pattern string) string {
	return namedGroupPattern.ReplaceAllString(pattern, "$1")
}

// TestIDPatternMatchesGolden asserts byte-for-byte equality (after stripping
// idPattern's named groups) for all seven composed ID patterns: every name
// segment matches the catch-all `[^/]+` (ResourceType's own doc comment explains why
// an ID's grammar must never be tightened to a resource's creation-time name
// grammar), so the table-driven rewrite reproduces every one of these
// literals exactly, with no exceptions.
func TestIDPatternMatchesGolden(t *testing.T) {
	tests := []struct {
		name   string
		typ    *ResourceType
		golden string
	}{
		{"reservation", &TypeReservation, goldenReservationIDPattern},
		{"subnet", &TypeSubnet, goldenSubnetIDPattern},
		{"storagePool", &TypeStoragePool, goldenStoragePoolIDPattern},
		{"volumeGroup", &TypeVolumeGroup, goldenVolumeGroupIDPattern},
		{"volumeGroupSnapshot", &TypeVolumeGroupSnapshot, goldenVolumeGroupSnapshotIDPattern},
		{"recoverableVolumeGroup", &TypeRecoverableVolumeGroup, goldenRecoverableVolumeGroupIDPattern},
		{"volume", &TypeVolume, goldenVolumeIDPattern},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripNamedGroups(tt.typ.idPattern().String())
			if got != tt.golden {
				t.Fatalf("idPattern (named groups stripped) =\n%s\nwant (golden)\n%s", got, tt.golden)
			}
		})
	}
}

// TestWantFormatShape checks the example ID exampleID() renders, by shape
// rather than against a frozen string: it must name every segment of the
// type's chain in order, render the type's own leaf as <name>, render each
// ancestor as that ancestor's noun, and carry the provider namespace once.
//
// The old hand-written example constants are deliberately not asserted
// verbatim. They disagreed with each other — subnet's own leaf read <subnet>
// where every other read <name>, and volumeGroupSnapshot showed the
// volumeGroup ancestor as <volume group> where volumeWantFormat showed that
// same ancestor as <vg> — so no single rule could reproduce them all. Pinning
// one rendering would also freeze wording that is only ever read by a human,
// while this test still fails if the derivation drops or misorders a segment.
func TestWantFormatShape(t *testing.T) {
	for _, typ := range []*ResourceType{
		&TypeReservation, &TypeSubnet, &TypeStoragePool,
		&TypeVolumeGroup, &TypeVolumeGroupSnapshot, &TypeRecoverableVolumeGroup, &TypeVolume,
	} {
		t.Run(typ.noun, func(t *testing.T) {
			got := typ.exampleID()

			chain := typ.chain()
			if want := "/providers/" + chain[0].namespace + "/"; !strings.Contains(got, want) {
				t.Errorf("exampleID() does not carry %q: %s", want, got)
			}

			// Each segment appears in chain order, every ancestor labelled by its
			// own noun and the leaf by <name>.
			rest := got
			for i, a := range chain {
				placeholder := "name"
				if i < len(chain)-1 {
					placeholder = a.noun
				}
				want := "/" + a.segment + "/<" + placeholder + ">"
				idx := strings.Index(rest, want)
				if idx < 0 {
					t.Fatalf("exampleID() missing %q in order: %s", want, got)
				}
				rest = rest[idx+len(want):]
			}
			if !strings.HasSuffix(got, "<name>\"") {
				t.Errorf("exampleID() must end with the leaf placeholder: %s", got)
			}
		})
	}
}

// TestARMIDPatterns is the pre-existing behavioural safety net for
// reservation and subnet's full-ID matching, now pointed at TypeReservation
// and TypeSubnet's own idPattern() instead of the deleted standalone vars.
func TestARMIDPatterns(t *testing.T) {
	t.Run("reservation", func(t *testing.T) {
		tests := []struct {
			name string
			id   string
			want bool
		}{
			{"valid", "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/PureStorage.Block/reservations/res1", true},
			{"valid, mixed case namespace", "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/purestorage.block/reservations/res1", true},
			{"wrong resource type", "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1", false},
			{"not a resource ID", "res1", false},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if got := TypeReservation.idPattern().MatchString(tt.id); got != tt.want {
					t.Fatalf("MatchString(%q) = %v, want %v", tt.id, got, tt.want)
				}
			})
		}
	})

	t.Run("subnet", func(t *testing.T) {
		tests := []struct {
			name string
			id   string
			want bool
		}{
			{"valid", "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet1/subnets/snet1", true},
			{"valid, mixed case", "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/microsoft.network/virtualnetworks/vnet1/subnets/snet1", true},
			{"missing subnet segment", "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet1", false},
			{"wrong namespace", "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.Compute/virtualNetworks/vnet1/subnets/snet1", false},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if got := TypeSubnet.idPattern().MatchString(tt.id); got != tt.want {
					t.Fatalf("MatchString(%q) = %v, want %v", tt.id, got, tt.want)
				}
			})
		}
	})
}

// TestVolumeGroupARMIDPatterns is the pre-existing behavioural safety net for
// storage pool, volume group, volume group snapshot and recoverable volume
// group full-ID matching, now pointed at each type's own idPattern().
func TestVolumeGroupARMIDPatterns(t *testing.T) {
	tests := []struct {
		name    string
		pattern *regexp.Regexp
		id      string
		want    bool
	}{
		{
			"storagePool valid", TypeStoragePool.idPattern(),
			"/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1",
			true,
		},
		{
			"storagePool rejects a nested type", TypeStoragePool.idPattern(),
			"/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg1",
			false,
		},
		{
			"volumeGroup valid", TypeVolumeGroup.idPattern(),
			"/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg1",
			true,
		},
		{
			"volumeGroup rejects the pool-only ID", TypeVolumeGroup.idPattern(),
			"/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1",
			false,
		},
		{
			"volumeGroupSnapshot valid", TypeVolumeGroupSnapshot.idPattern(),
			"/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg1/snapshots/snap1",
			true,
		},
		{
			"recoverableVolumeGroup valid", TypeRecoverableVolumeGroup.idPattern(),
			"/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1/recoverableVolumeGroups/vg1-20260101120000",
			true,
		},
		{
			"recoverableVolumeGroup rejects a volumeGroups ID", TypeRecoverableVolumeGroup.idPattern(),
			"/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg1",
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.pattern.MatchString(tt.id); got != tt.want {
				t.Fatalf("MatchString(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

// TestVolumeIDPattern is the pre-existing behavioural safety net for volume
// full-ID matching, now pointed at TypeVolume's own idPattern().
func TestVolumeIDPattern(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{
			"valid",
			"/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg1/volumes/vol1",
			true,
		},
		{
			"valid, mixed case",
			"/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/purestorage.block/storagepools/pool1/volumegroups/vg1/volumes/vol1",
			true,
		},
		{
			"rejects the volume group ID (missing /volumes/<name>)",
			"/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg1",
			false,
		},
		{
			"rejects a snapshot ID",
			"/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/PureStorage.Block/storagePools/pool1/volumeGroups/vg1/snapshots/snap1",
			false,
		},
		{"rejects a non-resource-ID string", "vol1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TypeVolume.idPattern().MatchString(tt.id); got != tt.want {
				t.Fatalf("MatchString(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

// TestVolumeIDPatternAcceptsSoftDeleteTwinName pins down that a volume ID's
// own name segment must accept a name longer than create ever allows. A
// soft-deleted volume is renamed with a timestamp suffix, so a valid volume
// ID can carry a name longer than create accepts: a 40-character volume
// becomes a 55-character "{name}-{YYYYMMDDHHmmss}" twin, and GET/DELETE both
// address it by that longer name.
func TestVolumeIDPatternAcceptsSoftDeleteTwinName(t *testing.T) {
	name := strings.Repeat("a", 40) + "-20260923120000"
	if len(name) != 55 {
		t.Fatalf("test fixture name is %d characters, want 55", len(name))
	}
	id := "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/" +
		"PureStorage.Block/storagePools/pool1/volumeGroups/vg1/volumes/" + name
	if !TypeVolume.idPattern().MatchString(id) {
		t.Fatalf("idPattern() did not match a volume ID with a soft-delete timestamp-suffixed name: %q", id)
	}
}
