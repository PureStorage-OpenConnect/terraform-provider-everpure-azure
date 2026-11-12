// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"strings"
	"testing"
)

// nameOfLength builds a name of exactly n characters that would otherwise be valid
// (starts with a letter, ends with a letter, hyphen-free in between), so length is
// the only variable under test.
func nameOfLength(n int) string {
	if n < 2 {
		return strings.Repeat("a", n)
	}
	return "a" + strings.Repeat("b", n-2) + "a"
}

func TestStorageObjectNamePattern(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"minimum length, 2 chars", "ab", true},
		{"maximum length, 40 chars", nameOfLength(40), true},
		{"41 chars, one too many", nameOfLength(41), false},
		{"single char, too short", "a", false},
		{"empty", "", false},
		{"starts with a digit", "1abc", false},
		{"starts with a hyphen", "-abc", false},
		{"ends with a hyphen", "abc-", false},
		{"contains an underscore", "ab_cd", false},
		{"hyphen in the middle", "ab-cd", true},
		{"all letters", "Volume-Group-1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := storageObjectNamePattern.MatchString(tt.value); got != tt.want {
				t.Fatalf("MatchString(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
