// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/purestorageblock/armpurestorageblock"
)

// Volumes and snapshots share a two-step delete: the first DELETE renames the live
// object to "{name}-{YYYYMMDDHHmmss}" and destroys it (freeing the original name at
// once), and a second DELETE on the renamed "twin" eradicates it. The twin's name is
// only discoverable by listing, so eradicate_on_destroy lists the parent before and
// after the first DELETE and picks the twin by set difference. Everything in this file
// is that algorithm, parameterised over the listed object type T through
// softDeletionTwinAccessors, so every soft-deletable leaf resource uses the same
// implementation.

// softDeletionTwinAccessors reads the two fields of a listed object the twin algorithm
// needs.
type softDeletionTwinAccessors[T any] struct {
	name         func(*T) *string
	softDeletion func(*T) *armpurestorageblock.DestroyedStateProperties
}

var volumeTwins = softDeletionTwinAccessors[armpurestorageblock.Volume]{
	name: func(v *armpurestorageblock.Volume) *string { return v.Name },
	softDeletion: func(v *armpurestorageblock.Volume) *armpurestorageblock.DestroyedStateProperties {
		if v.Properties == nil {
			return nil
		}
		return v.Properties.SoftDeletion
	},
}

var snapshotTwins = softDeletionTwinAccessors[armpurestorageblock.VolumeGroupSnapshot]{
	name: func(s *armpurestorageblock.VolumeGroupSnapshot) *string { return s.Name },
	softDeletion: func(s *armpurestorageblock.VolumeGroupSnapshot) *armpurestorageblock.DestroyedStateProperties {
		if s.Properties == nil {
			return nil
		}
		return s.Properties.SoftDeletion
	},
}

// isDestroyed reports whether sd marks a soft-deleted (destroyed, not yet eradicated)
// object. A nil sd, or one whose Destroyed flag is nil or false, means the object is
// live.
func isDestroyed(sd *armpurestorageblock.DestroyedStateProperties) bool {
	return sd != nil && sd.Destroyed != nil && *sd.Destroyed
}

// byName returns the entries of items whose name isn't present in against, so newTwins
// and removed can each ask the same question in opposite directions. Entries with no
// name are skipped.
func (a softDeletionTwinAccessors[T]) byName(items, against []*T) []*T {
	names := make(map[string]bool, len(against))
	for _, item := range against {
		if item == nil {
			continue
		}
		if name := a.name(item); name != nil {
			names[*name] = true
		}
	}

	var result []*T
	for _, item := range items {
		if item == nil {
			continue
		}
		name := a.name(item)
		if name == nil {
			continue
		}
		if !names[*name] {
			result = append(result, item)
		}
	}
	return result
}

// newTwins returns the entries in after whose name isn't present in before, so callers
// can find exactly the twin a specific delete just created — without relying on clocks
// or parsing a timestamp out of a name — even when an unrelated twin with the same
// previousName already existed.
func (a softDeletionTwinAccessors[T]) newTwins(before, after []*T) []*T {
	return a.byName(after, before)
}

// removed returns the entries in before whose name isn't present in after — the mirror
// image of newTwins — so callers can learn the exact, server-preserved spelling of the
// object a specific delete removed, without ever consulting state's own copy of the
// name.
func (a softDeletionTwinAccessors[T]) removed(before, after []*T) []*T {
	return a.byName(before, after)
}

// removedName resolves, from removed (see removed), the exact server spelling of the one
// object this delete itself removed. Normally there is exactly one candidate and its
// name is returned outright. Concurrent destroys in the same parent — reachable under
// Terraform's default parallelism — can remove more than one object between the two
// listings; narrowing that set to whichever entry's name case-insensitively matches
// stateName resolves the ordinary case without ever treating state's spelling as
// authoritative for the eventual exact match. Zero or more than one candidate left after
// that narrowing means there is no reliable way to tell which removal was this delete's
// own, so it returns ok=false rather than guess.
func (a softDeletionTwinAccessors[T]) removedName(removed []*T, stateName string) (name string, ok bool) {
	if len(removed) > 1 {
		var narrowed []*T
		for _, item := range removed {
			if item == nil {
				continue
			}
			if n := a.name(item); n != nil && strings.EqualFold(*n, stateName) {
				narrowed = append(narrowed, item)
			}
		}
		removed = narrowed
	}
	if len(removed) != 1 || removed[0] == nil {
		return "", false
	}
	n := a.name(removed[0])
	if n == nil {
		return "", false
	}
	return *n, true
}

// selectTarget picks, among newly appeared entries (see newTwins), the one whose
// softDeletion.previousName equals name. Normally there is at most one: a delete creates
// exactly one twin. If several somehow qualify, the newest by softDeletion.destroyedAt
// wins; nil means none did. The comparison is exact, not case-insensitive: soft delete
// frees the exact spelling it renamed away from, so two twins' previousName can differ
// only by case, and folding the comparison could eradicate the wrong one irreversibly.
func (a softDeletionTwinAccessors[T]) selectTarget(newTwins []*T, name string) *T {
	var selected *T
	var selectedAt time.Time

	for _, twin := range newTwins {
		if twin == nil {
			continue
		}
		softDeletion := a.softDeletion(twin)
		if softDeletion == nil || softDeletion.PreviousName == nil || *softDeletion.PreviousName != name {
			continue
		}

		var destroyedAt time.Time
		if softDeletion.DestroyedAt != nil {
			destroyedAt = *softDeletion.DestroyedAt
		}
		if selected == nil || destroyedAt.After(selectedAt) {
			selected, selectedAt = twin, destroyedAt
		}
	}

	return selected
}

// eradicationTarget is the whole lookup a delete with eradicate_on_destroy performs once
// both listings are in hand: resolve the server's own spelling of the removed object,
// then pick the new twin whose previousName is exactly that, returning the twin's own
// name. ok is false when either step cannot identify a single object.
//
// The removed object is matched by the server's own spelling, not state's copy of the
// name: names fold for lookup but keep whatever case they were created or renamed under,
// so state can legitimately disagree with the server after import, uncorrected by Read.
// stateName is used only to disambiguate concurrent removals, never as the match itself.
func (a softDeletionTwinAccessors[T]) eradicationTarget(before, after []*T, stateName string) (twinName string, ok bool) {
	removedName, ok := a.removedName(a.removed(before, after), stateName)
	if !ok {
		return "", false
	}
	target := a.selectTarget(a.newTwins(before, after), removedName)
	if target == nil {
		return "", false
	}
	name := a.name(target)
	if name == nil {
		return "", false
	}
	return *name, true
}
