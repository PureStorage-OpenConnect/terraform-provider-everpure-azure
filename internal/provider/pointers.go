// SPDX-License-Identifier: Apache-2.0

package provider

// derefString returns *s, or "" when s is nil.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// derefStrings dereferences every non-nil entry; nil entries are dropped, not mapped to "".
func derefStrings(s []*string) []string {
	result := make([]string, 0, len(s))
	for _, p := range s {
		if p != nil {
			result = append(result, *p)
		}
	}
	return result
}

// canonicalLocationPtr is canonicalLocation over an optional location; nil stays nil.
func canonicalLocationPtr(location *string) *string {
	if location == nil {
		return nil
	}
	canonical := canonicalLocation(*location)
	return &canonical
}
