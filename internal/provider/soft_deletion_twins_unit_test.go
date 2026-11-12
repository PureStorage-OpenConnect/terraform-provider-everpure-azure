// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// twinKind bundles, for one soft-deletable object type, the accessors under test and
// constructors for that type's bare-named and soft-deleted entries. Every test below
// runs once per kind, so the volume and snapshot instantiations of the generic helpers
// are held to exactly the same cases.
type twinKind[T any] struct {
	name      string
	accessors softDeletionTwinAccessors[T]
	// named builds an entry carrying only a name.
	named func(name string) *T
	// twin builds a destroyed entry with a previousName and destroyedAt.
	twin func(previousName, name string, destroyedAt time.Time) *T
	// noProperties, noSoftDeletion and noPreviousName build entries missing the
	// successive levels the accessors must nil-check.
	noProperties, noSoftDeletion, noPreviousName *T
}

func volumeTwinKind() twinKind[armpurestorageblock.Volume] {
	return twinKind[armpurestorageblock.Volume]{
		name:      "volume",
		accessors: volumeTwins,
		named:     func(name string) *armpurestorageblock.Volume { return &armpurestorageblock.Volume{Name: to.Ptr(name)} },
		twin: func(previousName, name string, destroyedAt time.Time) *armpurestorageblock.Volume {
			return &armpurestorageblock.Volume{
				Name: to.Ptr(name),
				Properties: &armpurestorageblock.AzureVolumeProperties{
					SoftDeletion: &armpurestorageblock.DestroyedStateProperties{
						PreviousName: to.Ptr(previousName),
						DestroyedAt:  to.Ptr(destroyedAt),
					},
				},
			}
		},
		noProperties:   &armpurestorageblock.Volume{},
		noSoftDeletion: &armpurestorageblock.Volume{Properties: &armpurestorageblock.AzureVolumeProperties{}},
		noPreviousName: &armpurestorageblock.Volume{Properties: &armpurestorageblock.AzureVolumeProperties{
			SoftDeletion: &armpurestorageblock.DestroyedStateProperties{Destroyed: to.Ptr(true)},
		}},
	}
}

func snapshotTwinKind() twinKind[armpurestorageblock.VolumeGroupSnapshot] {
	return twinKind[armpurestorageblock.VolumeGroupSnapshot]{
		name:      "snapshot",
		accessors: snapshotTwins,
		named: func(name string) *armpurestorageblock.VolumeGroupSnapshot {
			return &armpurestorageblock.VolumeGroupSnapshot{Name: to.Ptr(name)}
		},
		twin: func(previousName, name string, destroyedAt time.Time) *armpurestorageblock.VolumeGroupSnapshot {
			return &armpurestorageblock.VolumeGroupSnapshot{
				Name: to.Ptr(name),
				Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{
					SoftDeletion: &armpurestorageblock.DestroyedStateProperties{
						PreviousName: to.Ptr(previousName),
						DestroyedAt:  to.Ptr(destroyedAt),
					},
				},
			}
		},
		noProperties:   &armpurestorageblock.VolumeGroupSnapshot{},
		noSoftDeletion: &armpurestorageblock.VolumeGroupSnapshot{Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{}},
		noPreviousName: &armpurestorageblock.VolumeGroupSnapshot{Properties: &armpurestorageblock.VolumeGroupSnapshotProperties{
			SoftDeletion: &armpurestorageblock.DestroyedStateProperties{Destroyed: to.Ptr(true)},
		}},
	}
}

func TestSoftDeletionTwins(t *testing.T) {
	t.Run("volume", func(t *testing.T) { runSoftDeletionTwinTests(t, volumeTwinKind()) })
	t.Run("snapshot", func(t *testing.T) { runSoftDeletionTwinTests(t, snapshotTwinKind()) })
}

func runSoftDeletionTwinTests[T any](t *testing.T, k twinKind[T]) {
	t.Helper()
	a := k.accessors
	nameOf := func(item *T) string {
		n := a.name(item)
		if n == nil {
			return "<nil>"
		}
		return *n
	}

	t.Run("newTwins", func(t *testing.T) {
		t.Run("returns entries present after but not before", func(t *testing.T) {
			before := []*T{k.named("a1")}
			after := []*T{k.named("a1"), k.named("a1-20260917145432")}

			got := a.newTwins(before, after)
			if len(got) != 1 || nameOf(got[0]) != "a1-20260917145432" {
				t.Fatalf("got %d new twins (first %v), want just the new twin", len(got), got)
			}
		})

		t.Run("no new twins returns an empty result", func(t *testing.T) {
			if got := a.newTwins([]*T{k.named("a1")}, []*T{k.named("a1")}); len(got) != 0 {
				t.Fatalf("got %d new twins, want 0", len(got))
			}
		})

		t.Run("empty before returns every entry in after", func(t *testing.T) {
			if got := a.newTwins(nil, []*T{k.named("a1")}); len(got) != 1 {
				t.Fatalf("got %d new twins, want 1", len(got))
			}
		})

		t.Run("entries missing a name are skipped, not a panic", func(t *testing.T) {
			after := []*T{nil, new(T), k.named("a1")}
			if got := a.newTwins(nil, after); len(got) != 1 {
				t.Fatalf("got %d new twins, want 1", len(got))
			}
		})
	})

	t.Run("removed", func(t *testing.T) {
		t.Run("returns entries present before but not after", func(t *testing.T) {
			before := []*T{k.named("a1"), k.named("a2")}
			after := []*T{k.named("a2"), k.named("a1-20260917145432")}

			got := a.removed(before, after)
			if len(got) != 1 || nameOf(got[0]) != "a1" {
				t.Fatalf("got %d removed entries, want just a1", len(got))
			}
		})

		t.Run("nothing removed returns an empty result", func(t *testing.T) {
			if got := a.removed([]*T{k.named("a1")}, []*T{k.named("a1")}); len(got) != 0 {
				t.Fatalf("got %d removed entries, want 0", len(got))
			}
		})

		t.Run("empty after returns every entry in before", func(t *testing.T) {
			if got := a.removed([]*T{k.named("a1")}, nil); len(got) != 1 {
				t.Fatalf("got %d removed entries, want 1", len(got))
			}
		})

		t.Run("entries missing a name are skipped, not a panic", func(t *testing.T) {
			before := []*T{nil, new(T), k.named("a1")}
			if got := a.removed(before, nil); len(got) != 1 {
				t.Fatalf("got %d removed entries, want 1", len(got))
			}
		})
	})

	t.Run("removedName", func(t *testing.T) {
		t.Run("a single removed entry is returned outright, whatever its case", func(t *testing.T) {
			name, ok := a.removedName([]*T{k.named("A1")}, "a1")
			if !ok || name != "A1" {
				t.Fatalf("got (%q, %v), want (\"A1\", true)", name, ok)
			}
		})

		t.Run("no removed entries: not found", func(t *testing.T) {
			if _, ok := a.removedName(nil, "a1"); ok {
				t.Fatal("expected ok=false with nothing removed")
			}
		})

		t.Run("several removed entries narrowed to one by a case-insensitive match against state's name", func(t *testing.T) {
			// Simulates a concurrent destroy in the same parent removing an unrelated
			// object between the two listings.
			name, ok := a.removedName([]*T{k.named("a2"), k.named("A1")}, "a1")
			if !ok || name != "A1" {
				t.Fatalf("got (%q, %v), want (\"A1\", true)", name, ok)
			}
		})

		t.Run("several removed entries, none matching state's name: ambiguous, not found", func(t *testing.T) {
			if _, ok := a.removedName([]*T{k.named("a2"), k.named("a3")}, "a1"); ok {
				t.Fatal("expected ok=false when no removed entry matches")
			}
		})

		t.Run("several removed entries, more than one matching state's name: ambiguous, not found", func(t *testing.T) {
			// Two removed entries both fold-matching state's name isn't a shape the
			// real backend can produce (case-insensitive uniqueness), but the guard
			// must still refuse to guess rather than eradicate on unproven ground.
			if _, ok := a.removedName([]*T{k.named("a1"), k.named("A1")}, "a1"); ok {
				t.Fatal("expected ok=false when more than one removed entry matches")
			}
		})

		t.Run("a nil entry among several is skipped, not a panic", func(t *testing.T) {
			name, ok := a.removedName([]*T{nil, k.named("A1")}, "a1")
			if !ok || name != "A1" {
				t.Fatalf("got (%q, %v), want (\"A1\", true)", name, ok)
			}
		})
	})

	t.Run("selectTarget", func(t *testing.T) {
		t.Run("empty list returns nil", func(t *testing.T) {
			if got := a.selectTarget(nil, "a1"); got != nil {
				t.Fatalf("got %+v, want nil", got)
			}
		})

		t.Run("a new twin with a matching previousName is selected", func(t *testing.T) {
			want := k.twin("a1", "a1-20260917145432", time.Now())
			if got := a.selectTarget([]*T{want}, "a1"); got != want {
				t.Fatalf("got %+v, want the matching new twin", got)
			}
		})

		t.Run("a decoy with a different previousName is ignored", func(t *testing.T) {
			decoy := k.twin("other", "other-20260917145432", time.Now())
			if got := a.selectTarget([]*T{decoy}, "a1"); got != nil {
				t.Fatalf("got %+v, want nil", got)
			}
		})

		t.Run("several qualifying twins: newest destroyedAt wins", func(t *testing.T) {
			older := k.twin("a1", "a1-20260101000000", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			newer := k.twin("a1", "a1-20260601000000", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
			// A decoy for an unrelated object, destroyed later than every matching
			// twin: the target must be selected by previousName first and destroyedAt
			// only among those matches, not by a global max that would pick the decoy.
			decoy := k.twin("other", "other-20261231000000", time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC))

			if got := a.selectTarget([]*T{older, newer, decoy}, "a1"); got != newer {
				t.Fatalf("got the wrong twin: %+v", got)
			}
		})

		t.Run("a twin missing Properties, SoftDeletion or PreviousName is skipped, not a panic", func(t *testing.T) {
			want := k.twin("a1", "a1-20260917145432", time.Now())
			newTwins := []*T{nil, k.noProperties, k.noSoftDeletion, k.noPreviousName, want}
			if got := a.selectTarget(newTwins, "a1"); got != want {
				t.Fatalf("got %+v, want the one real match", got)
			}
		})

		t.Run("a twin whose previousName differs from name only by case is not selected", func(t *testing.T) {
			// Exact, not case-insensitive: two twins can coexist whose previousName
			// values differ only by case, and folding could eradicate the wrong one.
			twin := k.twin("A1", "A1-20260917145432", time.Now())
			if got := a.selectTarget([]*T{twin}, "a1"); got != nil {
				t.Fatalf("got %+v, want nil (case-only match must not count)", got)
			}
		})

		t.Run("two twins whose previousName differ only by case: only the exact match is chosen", func(t *testing.T) {
			wanted := k.twin("Ab1", "Ab1-20260917145432", time.Now())
			decoy := k.twin("AB1", "AB1-20260917145433", time.Now())
			if got := a.selectTarget([]*T{decoy, wanted}, "Ab1"); got != wanted {
				t.Fatalf("got %+v, want the exact-case match", got)
			}
		})
	})

	t.Run("eradicationTarget", func(t *testing.T) {
		now := time.Now()

		t.Run("finds the twin this delete created, ignoring a pre-existing twin of the same previousName", func(t *testing.T) {
			preexisting := k.twin("a1", "a1-20260101000000", now.Add(-time.Hour))
			before := []*T{k.named("a1"), preexisting}
			after := []*T{preexisting, k.twin("a1", "a1-20260917145432", now)}

			name, ok := a.eradicationTarget(before, after, "a1")
			if !ok || name != "a1-20260917145432" {
				t.Fatalf("got (%q, %v), want the newly created twin", name, ok)
			}
		})

		t.Run("matches the server's spelling of the removed object, not state's", func(t *testing.T) {
			before := []*T{k.named("A1")}
			after := []*T{k.twin("A1", "A1-20260917145432", now)}

			name, ok := a.eradicationTarget(before, after, "a1")
			if !ok || name != "A1-20260917145432" {
				t.Fatalf("got (%q, %v), want the twin named from the server's spelling", name, ok)
			}
		})

		t.Run("no twin appeared: not found", func(t *testing.T) {
			if _, ok := a.eradicationTarget([]*T{k.named("a1")}, nil, "a1"); ok {
				t.Fatal("expected ok=false when the delete left no twin")
			}
		})

		t.Run("nothing removed: not found", func(t *testing.T) {
			same := []*T{k.named("a1")}
			if _, ok := a.eradicationTarget(same, same, "a1"); ok {
				t.Fatal("expected ok=false when nothing was removed")
			}
		})
	})
}

func TestIsDestroyed(t *testing.T) {
	tests := []struct {
		name string
		sd   *armpurestorageblock.DestroyedStateProperties
		want bool
	}{
		{"nil", nil, false},
		{"nil Destroyed", &armpurestorageblock.DestroyedStateProperties{}, false},
		{"destroyed false", &armpurestorageblock.DestroyedStateProperties{Destroyed: to.Ptr(false)}, false},
		{"destroyed true", &armpurestorageblock.DestroyedStateProperties{Destroyed: to.Ptr(true)}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDestroyed(tt.sd); got != tt.want {
				t.Errorf("isDestroyed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSoftDeletedTwinNotFoundWarning(t *testing.T) {
	for _, k := range []kind{kindVolume, kindSnapshot} {
		t.Run(k.detail, func(t *testing.T) {
			d := softDeletedTwinNotFoundWarning(k, "a1")
			if d.Severity() != diag.SeverityWarning {
				t.Errorf("severity = %v, want a warning", d.Severity())
			}
			requireContains(t, d.Summary(), k.summary)
			requireContains(t, d.Detail(), `"a1"`)
			requireContains(t, d.Detail(), k.detail)
		})
	}
}
