// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLicenseFilePattern(t *testing.T) {
	tests := []struct {
		name  string
		match bool
	}{
		{"LICENSE", true},
		{"LICENSE.txt", true},
		{"COPYING", true},
		{"license.md", true},
		{"LICENSES", false},
		{"LICENSE.html", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := licenseFilePattern.MatchString(tt.name); got != tt.match {
				t.Fatalf("licenseFilePattern.MatchString(%q) = %v, want %v", tt.name, got, tt.match)
			}
		})
	}
}

func TestWriteNotice(t *testing.T) {
	const header = "NOTICE\n\nThird-party Go modules:\n"

	tests := []struct {
		name    string
		entries []reportEntry
		want    string
	}{
		{
			name: "a plain vX.Y.Z version in the URL",
			entries: []reportEntry{
				{Name: "github.com/foo/bar", LicenseURL: "https://github.com/foo/bar/blob/v1.23.1/LICENSE", License: "MIT"},
			},
			want: header +
				"\n    github.com/foo/bar v1.23.1\n" +
				"    License: MIT\n" +
				"    https://github.com/foo/bar/blob/v1.23.1/LICENSE\n",
		},
		{
			name: "a version with a prerelease suffix",
			entries: []reportEntry{
				{Name: "github.com/foo/baz", LicenseURL: "https://github.com/foo/baz/blob/v1.1.0-beta.2/LICENSE", License: "MIT"},
			},
			want: header +
				"\n    github.com/foo/baz v1.1.0-beta.2\n" +
				"    License: MIT\n" +
				"    https://github.com/foo/baz/blob/v1.1.0-beta.2/LICENSE\n",
		},
		{
			name: "a pseudo-versioned URL with only a commit hash gets no version",
			entries: []reportEntry{
				{Name: "github.com/pkg/browser", LicenseURL: "https://github.com/pkg/browser/blob/5ac0b6a4141c/LICENSE", License: "BSD-2-Clause"},
			},
			want: header +
				"\n    github.com/pkg/browser\n" +
				"    License: BSD-2-Clause\n" +
				"    https://github.com/pkg/browser/blob/5ac0b6a4141c/LICENSE\n",
		},
		{
			name: "multiple entries render one block each, in order",
			entries: []reportEntry{
				{Name: "github.com/a", LicenseURL: "https://example.com/a/blob/v1.0.0/LICENSE", License: "MIT"},
				{Name: "github.com/b", LicenseURL: "https://example.com/b/blob/abc123/LICENSE", License: "Apache-2.0"},
			},
			want: header +
				"\n    github.com/a v1.0.0\n" +
				"    License: MIT\n" +
				"    https://example.com/a/blob/v1.0.0/LICENSE\n" +
				"\n    github.com/b\n" +
				"    License: Apache-2.0\n" +
				"    https://example.com/b/blob/abc123/LICENSE\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			headerPath := filepath.Join(dir, "NOTICE.header")
			noticePath := filepath.Join(dir, "NOTICE.txt")
			if err := os.WriteFile(headerPath, []byte(header), 0o600); err != nil {
				t.Fatalf("writing header fixture: %v", err)
			}

			if err := writeNotice(headerPath, noticePath, tt.entries); err != nil {
				t.Fatalf("writeNotice: %v", err)
			}

			got, err := os.ReadFile(noticePath)
			if err != nil {
				t.Fatalf("reading generated notice: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("writeNotice output = %q, want %q", got, tt.want)
			}
		})
	}
}
