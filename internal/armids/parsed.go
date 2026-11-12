// SPDX-License-Identifier: Apache-2.0

package armids

// Parse validates id against t's idPattern and extracts it into a parsedID in
// one pass. This is grammar only: it knows nothing about provider
// configuration or subscriptions — callers needing that check apply it
// themselves against the parsed subscription.
func (t *ResourceType) Parse(id string) (parsedID, error) {
	pattern := t.idPattern()
	loc := pattern.FindStringSubmatchIndex(id)
	if loc == nil {
		return parsedID{}, &InvalidIDError{Input: id, Noun: t.noun, WantFormat: t.exampleID()}
	}
	subscriptionIdx := pattern.SubexpIndex("subscriptionID")
	resourceGroupIdx := pattern.SubexpIndex("resourceGroup")
	return parsedID{
		raw:            id,
		subscriptionID: id[loc[2*subscriptionIdx]:loc[2*subscriptionIdx+1]],
		resourceGroup:  id[loc[2*resourceGroupIdx]:loc[2*resourceGroupIdx+1]],
		typ:            t,
		submatch:       loc,
	}, nil
}

// AncestorIDOf parses id as t and returns the ID text of t's ancestor named
// by want, for callers that need nothing else out of id. Folding the two
// steps together is more than shorthand: AncestorID cannot fail once Parse
// has succeeded and want really is one of t's ancestors — both of which the
// caller asserts by naming the two types — so keeping the steps apart leaves
// every caller writing a second error branch it can never enter.
func (t *ResourceType) AncestorIDOf(id string, want *ResourceType) (string, error) {
	p, err := t.Parse(id)
	if err != nil {
		return "", err
	}
	return p.AncestorID(want)
}

// ParentIDOf parses id as t and returns its immediate parent's ID: the
// one-level-up case of AncestorIDOf, and it exists for the same reason.
func (t *ResourceType) ParentIDOf(id string) (string, error) {
	p, err := t.Parse(id)
	if err != nil {
		return "", err
	}
	return p.ParentID()
}

// parsedID is the generic result of matching an id against a ResourceType's
// idPattern. It never leaves this package by name: a caller outside armids
// can only ever hold one via `:=` immediately after calling Parse, and calls
// ParentID/AncestorID (or a per-resource adapter's own parent accessor) on it
// right away — it can't declare a variable of this type itself, since the
// type is unexported. Every resource file gets its data through a per-type
// adapter (StoragePoolID, VolumeGroupID, VolumeID) or as a plain string from
// ParentID/AncestorID.
type parsedID struct {
	// raw is the ID exactly as it was parsed. Every name and ancestor ID is
	// sliced back out of it rather than rebuilt, so results keep whatever
	// casing the caller used.
	raw string

	subscriptionID string
	resourceGroup  string

	// typ is the ResourceType raw was matched against, and so which pattern's
	// capture groups submatch below indexes into.
	typ *ResourceType

	// submatch holds typ.idPattern()'s FindStringSubmatchIndex result: pairs
	// of start/end byte offsets into raw, two per capture group.
	submatch []int
}

// name returns p's own name segment.
func (p parsedID) name() string {
	return p.segmentText(p.typ.segment)
}

// nameOf returns the name segment belonging to anc, an ancestor of p.typ in the
// ID grammar. It cannot fail in practice: matching p.typ's pattern already
// pinned its entire ancestor chain, so anc's named group is always present
// whenever anc really is one of p.typ's ancestors — which every call site
// already knows, the same way it already knows p.typ itself.
func (p parsedID) nameOf(anc *ResourceType) string {
	return p.segmentText(anc.segment)
}

func (p parsedID) segmentText(segment string) string {
	idx := p.typ.idPattern().SubexpIndex(segment)
	return p.raw[p.submatch[2*idx]:p.submatch[2*idx+1]]
}

// resourceGroupID returns the resource group p was parsed from.
func (p parsedID) resourceGroupID() ResourceGroupID {
	return ResourceGroupID{ResourceGroupName: p.resourceGroup, SubscriptionID: p.subscriptionID}
}

// ParentID returns the ID text of p's immediate parent (one level up its
// chain), cut from p's original string rather than rebuilt via nestedID, so
// it keeps whatever casing the caller used throughout — see nestedID's own
// doc comment (in resourcetype.go) for how the two mechanisms divide the work.
// It fails only when p.typ sits directly under the resource group, with no
// parent segment to cut.
func (p parsedID) ParentID() (string, error) {
	chain := p.typ.chain()
	if len(chain) < 2 {
		return "", &NoParentError{Input: p.raw, Noun: p.typ.noun}
	}
	return p.AncestorID(chain[len(chain)-2])
}

// AncestorID returns the ID text of the ancestor of p.typ named by want, cut at
// the offset where want's own name segment ends in p's original string — so,
// like ParentID, the result preserves the caller's casing with no separate
// suffix-match guard needed. It fails only when want is not actually an
// ancestor of p.typ (or p.typ itself).
func (p parsedID) AncestorID(want *ResourceType) (string, error) {
	idx := p.typ.idPattern().SubexpIndex(want.segment)
	if idx < 0 || p.submatch[2*idx] < 0 {
		return "", &NotNestedError{Input: p.raw, Noun: p.typ.noun, WantNoun: want.noun}
	}
	return p.raw[:p.submatch[2*idx+1]], nil
}
