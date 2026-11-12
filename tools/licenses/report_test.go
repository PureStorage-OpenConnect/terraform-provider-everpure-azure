// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

func TestParseReportEntries(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []reportEntry
		wantErr string // substring expected in the error; empty means no error.
	}{
		{
			name:  "valid 3-column rows",
			input: "github.com/foo/bar,https://example.com/bar/LICENSE,MIT\ngithub.com/baz,https://example.com/baz/LICENSE,Apache-2.0\n",
			want: []reportEntry{
				{Name: "github.com/foo/bar", LicenseURL: "https://example.com/bar/LICENSE", License: "MIT"},
				{Name: "github.com/baz", LicenseURL: "https://example.com/baz/LICENSE", License: "Apache-2.0"},
			},
		},
		{
			name:    "wrong column count",
			input:   "github.com/foo/bar,https://example.com/bar/LICENSE\n",
			wantErr: "expected 3 columns",
		},
		{
			name:  "empty input",
			input: "",
			want:  []reportEntry{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseReportEntries(strings.NewReader(tt.input))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("parseReportEntries() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("entry %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestSortUniqueEntries(t *testing.T) {
	t.Run("orders deterministically and drops exact duplicates", func(t *testing.T) {
		entries := []reportEntry{
			{Name: "github.com/z", LicenseURL: "u", License: "MIT"},
			{Name: "github.com/a", LicenseURL: "u", License: "MIT"},
			{Name: "github.com/a", LicenseURL: "u", License: "MIT"}, // Exact duplicate of the previous row.
			{Name: "github.com/m", LicenseURL: "u", License: "MIT"},
		}
		want := []reportEntry{
			{Name: "github.com/a", LicenseURL: "u", License: "MIT"},
			{Name: "github.com/m", LicenseURL: "u", License: "MIT"},
			{Name: "github.com/z", LicenseURL: "u", License: "MIT"},
		}

		got := sortUniqueEntries(entries)
		if len(got) != len(want) {
			t.Fatalf("sortUniqueEntries() = %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("entry %d = %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	t.Run("near-duplicates differing in one field are both kept", func(t *testing.T) {
		entries := []reportEntry{
			{Name: "github.com/a", LicenseURL: "u1", License: "MIT"},
			{Name: "github.com/a", LicenseURL: "u2", License: "MIT"},
		}

		got := sortUniqueEntries(entries)
		if len(got) != 2 {
			t.Fatalf("sortUniqueEntries() = %v, want 2 entries", got)
		}
	})
}
