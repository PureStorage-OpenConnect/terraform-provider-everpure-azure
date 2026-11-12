// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// diagRegexp matches d the way the terraform CLI prints it: the summary, then the first
// paragraph of the detail. A cause puts its own sentences in the first paragraph and the
// Azure response and request IDs after it, so the pattern holds whatever the response
// says. Terraform re-wraps the detail's lines, so words are matched across any run of
// whitespace.
func diagRegexp(d diag.Diagnostic) *regexp.Regexp {
	firstParagraph, _, _ := strings.Cut(d.Detail(), "\n\n")
	return regexp.MustCompile(`(?s)` + wordsRegexp(d.Summary()) + `.*` + wordsRegexp(firstParagraph))
}

// summaryRegexp matches the summary of d alone.
func summaryRegexp(d diag.Diagnostic) *regexp.Regexp {
	return regexp.MustCompile(wordsRegexp(d.Summary()))
}

func wordsRegexp(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		words[i] = regexp.QuoteMeta(w)
	}
	return strings.Join(words, `\s+`)
}

// causeRegexp matches the first paragraph of a cause's text as terraform prints it.
func causeRegexp(c detailError) *regexp.Regexp {
	firstParagraph, _, _ := strings.Cut(c.Error(), "\n\n")
	return regexp.MustCompile(`(?s)` + wordsRegexp(firstParagraph))
}

const testOtherSubscriptionID = "22222222-2222-2222-2222-222222222222"

// subscriptionMismatchRegexp matches the cause of an id of kind k in testOtherSubscriptionID.
func subscriptionMismatchRegexp(k kind, id string) *regexp.Regexp {
	return causeRegexp(newSubscriptionMismatchError(k, id, testOtherSubscriptionID, testSubscriptionID))
}

// deletionProtectedRegexp matches the summary of the deletion-protected error for k.
func deletionProtectedRegexp(k kind) *regexp.Regexp {
	return summaryRegexp(deletionProtectedDiagnostic(k, detailError{}))
}

// deletionProtectedReplaceRegexp matches the deletion-protected error that a replace
// triggered by a change to attr raises: the summary, then the attribute's config line.
func deletionProtectedReplaceRegexp(k kind, attr string) *regexp.Regexp {
	return regexp.MustCompile(`(?s)` + deletionProtectedRegexp(k).String() + `.*\n\s*\d+:\s+` + attr + `\s+=`)
}
