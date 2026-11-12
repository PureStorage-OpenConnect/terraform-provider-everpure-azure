// SPDX-License-Identifier: Apache-2.0

package armids

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// ResourceType describes one ARM resource type's place in this provider's ID
// grammar: which resource type it nests under (parent), and its own path
// segment name. Every full-ID regex, validator message, parse error and
// builder for a type is derived from those facts, plus namespace (present
// only at the root of a provider chain; inherited from parent otherwise) and
// noun (this resource's name, used both in error messages and as its
// placeholder where it appears as another type's ancestor in exampleID's
// example; a type's own leaf segment always renders as <name>).
//
// Every name segment in idPattern() matches the catch-all `[^/]+`, never a
// resource's own creation-time name grammar (e.g. storagePoolNamePattern,
// storageObjectNamePattern, both in the provider package): an ID names a
// resource that may already exist — possibly created outside Terraform, or
// renamed by the backend itself, like a soft-deleted volume's
// timestamp-suffixed name — so the grammar this provider imposes when
// creating a name must never be used to reject an ID that merely references
// one.
type ResourceType struct {
	parent    *ResourceType
	namespace string
	segment   string
	noun      string

	patternOnce sync.Once
	pattern     *regexp.Regexp
}

// chain returns t's ancestors root-first, ending with t itself.
func (t *ResourceType) chain() []*ResourceType {
	if t.parent == nil {
		return []*ResourceType{t}
	}
	return append(t.parent.chain(), t)
}

// idPattern returns the full-ID regex for t, composed from its chain, with a
// named capture group for the subscription, the resource group, and each
// ancestor's (and t's own) name segment — e.g. (?P<storagePools>...). Every
// name segment matches `[^/]+` (see ResourceType's own doc comment for why). It is
// compiled once per type, on first use, not on every call.
func (t *ResourceType) idPattern() *regexp.Regexp {
	t.patternOnce.Do(func() {
		chain := t.chain()
		var b strings.Builder
		b.WriteString(`(?i)^/subscriptions/(?P<subscriptionID>[^/]+)/resourceGroups/(?P<resourceGroup>[^/]+)/providers/`)
		b.WriteString(regexp.QuoteMeta(chain[0].namespace))
		for _, a := range chain {
			fmt.Fprintf(&b, "/%s/(?P<%s>[^/]+)", a.segment, a.segment)
		}
		b.WriteString("$")
		t.pattern = regexp.MustCompile(b.String())
	})
	return t.pattern
}

// exampleID renders a quoted example of a full ARM ID of type t, for use in
// both InvalidIDError.WantFormat and Validator's messages. t's own (leaf) segment always
// shows as <name>; an ancestor segment shows that ancestor's own noun,
// since <name> would be ambiguous once more than one segment is in play.
func (t *ResourceType) exampleID() string {
	chain := t.chain()
	var b strings.Builder
	b.WriteString(`"/subscriptions/.../resourceGroups/.../providers/`)
	b.WriteString(chain[0].namespace)
	for i, a := range chain {
		placeholder := "name"
		if i < len(chain)-1 {
			placeholder = a.noun
		}
		fmt.Fprintf(&b, "/%s/<%s>", a.segment, placeholder)
	}
	b.WriteString(`"`)
	return b.String()
}

// Validator returns the schema-level validator for a full ARM ID of type t.
func (t *ResourceType) Validator() validator.String {
	return stringvalidator.RegexMatches(t.idPattern(),
		"must be the full Azure Resource Manager ID of a "+t.noun+", e.g. "+t.exampleID())
}

// nestedID composes the full ARM ID of a child of parentID by appending t's
// own segment and name — the only place a segment is appended. A chain root
// has no parent resource, so it passes ResourceGroupID.prefix instead: the provider
// namespace plays the same role for a top-level type that a parent's ID plays
// for a nested one.
//
// This composes from components, which is one of this package's two
// ID-construction mechanisms; ParentID/AncestorID (in parsed.go) are the
// other, cutting a substring out of an ID the caller already holds. Only the
// cut preserves the caller's casing — composing normalises the segment
// keywords and namespace to their canonical spelling.
func (t *ResourceType) nestedID(parentID, name string) string {
	return fmt.Sprintf("%s/%s/%s", parentID, t.segment, name)
}

// ResourceGroupID identifies the resource group an ARM ID sits in — the two
// segments every ID in this provider starts with. It is embedded in each of
// the per-resource ID structs, which is why it deliberately has no FullID()
// method of its own: embedding promotes methods, so one here would be
// inherited by every one of them, and a type that forgot to define its own
// would silently return its resource group's ID instead.
type ResourceGroupID struct {
	ResourceGroupName string `tfsdk:"resource_group_name"`
	SubscriptionID    string `tfsdk:"subscription_id"`
}

// Subscription returns the ID of the subscription the resource group is in. Unlike a
// FullID() it names no resource, so a per-resource ID type that embeds ResourceGroupID
// can inherit it safely.
func (rg ResourceGroupID) Subscription() string { return rg.SubscriptionID }

// prefix returns the "/subscriptions/.../resourceGroups/.../providers/<namespace>"
// text a chain root's ID hangs off, which nestedID then appends to.
func (rg ResourceGroupID) prefix(namespace string) string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/%s",
		rg.SubscriptionID, rg.ResourceGroupName, namespace)
}
