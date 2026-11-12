// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestEntriesForModule(t *testing.T) {
	entries := []reportEntry{
		{Name: "github.com/hashicorp/go-plugin", LicenseURL: "u1", License: "MPL-2.0"},
		{Name: "github.com/hashicorp/terraform-plugin-go", LicenseURL: "u2", License: "MPL-2.0"},
		{Name: "github.com/hashicorp/terraform-plugin-go/tfprotov6", LicenseURL: "u3", License: "MPL-2.0"},
		{Name: "github.com/hashicorp/terraform-plugin-framework-timeouts", LicenseURL: "u4", License: "MPL-2.0"},
	}

	tests := []struct {
		name       string
		modulePath string
		wantNames  []string
	}{
		{
			name:       "exact module match only",
			modulePath: "github.com/hashicorp/go-plugin",
			wantNames:  []string{"github.com/hashicorp/go-plugin"},
		},
		{
			name:       "module plus a subpackage",
			modulePath: "github.com/hashicorp/terraform-plugin-go",
			wantNames:  []string{"github.com/hashicorp/terraform-plugin-go", "github.com/hashicorp/terraform-plugin-go/tfprotov6"},
		},
		{
			name:       "a lookalike prefix does not match",
			modulePath: "github.com/hashicorp/terraform-plugin-framework",
			wantNames:  nil,
		},
		{
			name:       "no match at all",
			modulePath: "github.com/hashicorp/does-not-exist",
			wantNames:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := entriesForModule(entries, tt.modulePath)
			gotNames := make([]string, len(got))
			for i, e := range got {
				gotNames[i] = e.Name
			}
			if len(gotNames) != len(tt.wantNames) {
				t.Fatalf("entriesForModule(%q) = %v, want %v", tt.modulePath, gotNames, tt.wantNames)
			}
			for i := range gotNames {
				if gotNames[i] != tt.wantNames[i] {
					t.Fatalf("entriesForModule(%q) = %v, want %v", tt.modulePath, gotNames, tt.wantNames)
				}
			}
		})
	}
}

func TestCheckMPL(t *testing.T) {
	tests := []struct {
		name       string
		entries    []reportEntry
		mpl        []string
		wantErr    bool
		wantStdout string // substring expected in the diagnostic output.
	}{
		{
			name: "every exception present and still MPL-2.0",
			entries: []reportEntry{
				{Name: "github.com/hashicorp/go-plugin", LicenseURL: "u1", License: "MPL-2.0"},
				{Name: "github.com/hashicorp/go-uuid", LicenseURL: "u2", License: "MPL-2.0"},
			},
			mpl:        []string{"github.com/hashicorp/go-plugin", "github.com/hashicorp/go-uuid"},
			wantErr:    false,
			wantStdout: "licenses-allowed-mpl OK",
		},
		{
			name: "a stale exception no longer in the graph",
			entries: []reportEntry{
				{Name: "github.com/hashicorp/go-plugin", LicenseURL: "u1", License: "MPL-2.0"},
			},
			mpl:        []string{"github.com/hashicorp/go-plugin", "github.com/hashicorp/terraform-plugin-testing"},
			wantErr:    true,
			wantStdout: "github.com/hashicorp/terraform-plugin-testing no longer appears in the dependency graph -- it's a stale exception",
		},
		{
			name: "the module's own row relicensed",
			entries: []reportEntry{
				{Name: "github.com/hashicorp/go-plugin", LicenseURL: "u1", License: "GPL-3.0"},
			},
			mpl:        []string{"github.com/hashicorp/go-plugin"},
			wantErr:    true,
			wantStdout: "github.com/hashicorp/go-plugin's license changed and is no longer MPL-2.0",
		},
		{
			name: "only a subpackage row relicensed",
			entries: []reportEntry{
				{Name: "github.com/hashicorp/terraform-plugin-go", LicenseURL: "u1", License: "MPL-2.0"},
				{Name: "github.com/hashicorp/terraform-plugin-go/tfprotov6", LicenseURL: "u2", License: "GPL-3.0"},
			},
			mpl:        []string{"github.com/hashicorp/terraform-plugin-go"},
			wantErr:    true,
			wantStdout: "github.com/hashicorp/terraform-plugin-go's license changed and is no longer MPL-2.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			stdout := captureStdout(t, func() {
				err = checkMPL(tt.entries, tt.mpl)
			})

			if tt.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(stdout, tt.wantStdout) {
				t.Fatalf("stdout %q does not contain %q", stdout, tt.wantStdout)
			}
		})
	}
}

// captureStdout redirects os.Stdout for the duration of f and returns
// whatever f wrote to it.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	f()

	if err := w.Close(); err != nil {
		t.Fatalf("closing pipe writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("reading captured stdout: %v", err)
	}
	return buf.String()
}
