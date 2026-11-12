// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"
	"strings"
	"testing"
)

// goreleaserFixture mirrors the real .goreleaser.yml's shape (flow-style
// lists): one build, three goos, two goarch, one ignored pair.
const goreleaserFixture = `# goreleaser configuration
version: 2

project_name: terraform-provider-everpure-azure

builds:
  - id: release
    env:
      - CGO_ENABLED=0
    goos: [linux, darwin, windows]
    goarch: [amd64, arm64]
    ignore:
      - goos: windows
        goarch: arm64
    binary: '{{ .ProjectName }}_v{{ .Version }}'

archives:
  - ids: [release]
`

// goreleaserBlockStyleFixture is the same matrix as goreleaserFixture, but
// with block-style (`- item` per line) lists and quoted scalars throughout,
// to prove parsing isn't tied to one YAML style.
const goreleaserBlockStyleFixture = `version: 2
builds:
  - id: "release"
    goos:
      - "linux"
      - "darwin"
      - "windows"
    goarch:
      - "amd64"
      - "arm64"
    ignore:
      - goos: "windows"
        goarch: "arm64"
`

func TestBuildMatrix(t *testing.T) {
	tests := []struct {
		name   string
		builds []goreleaserBuild
		want   []string
	}{
		{
			name: "one build, no ignores",
			builds: []goreleaserBuild{
				{Goos: []string{"linux", "darwin"}, Goarch: []string{"amd64", "arm64"}},
			},
			want: []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"},
		},
		{
			name: "one build, one ignored pair",
			builds: []goreleaserBuild{
				{
					Goos:   []string{"linux", "darwin", "windows"},
					Goarch: []string{"amd64", "arm64"},
					Ignore: []goreleaserIgnore{{Goos: "windows", Goarch: "arm64"}},
				},
			},
			want: []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64", "windows_amd64"},
		},
		{
			name: "two builds with differing matrices and per-build ignores union, deduplicated",
			builds: []goreleaserBuild{
				{
					Goos:   []string{"linux", "windows"},
					Goarch: []string{"amd64"},
				},
				{
					Goos:   []string{"linux", "darwin"},
					Goarch: []string{"amd64", "arm64"},
					Ignore: []goreleaserIgnore{{Goos: "darwin", Goarch: "amd64"}},
				},
			},
			// linux_amd64 is produced by both builds and appears once;
			// windows_amd64 and linux_arm64 each by only one build;
			// darwin_arm64 by the second build (darwin_amd64 is that
			// build's own ignore, so it never appears at all).
			want: []string{"linux_amd64", "windows_amd64", "linux_arm64", "darwin_arm64"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildMatrix(tt.builds)
			if !slices.Equal(sortedCopy(got), sortedCopy(tt.want)) {
				t.Fatalf("buildMatrix(%+v) = %v, want %v (as sets)", tt.builds, got, tt.want)
			}
		})
	}
}

func TestCheckPlatforms(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		declared []string
		wantErr  string // substring expected in the error; empty means no error.
	}{
		{
			name:     "flow-style config matching the real .goreleaser.yml shape",
			content:  goreleaserFixture,
			declared: []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64", "windows_amd64"},
			wantErr:  "",
		},
		{
			name:     "block-style config with quoted scalars, same matrix",
			content:  goreleaserBlockStyleFixture,
			declared: []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64", "windows_amd64"},
			wantErr:  "",
		},
		{
			name: "multiple builds, declared list must cover their union",
			content: `builds:
  - goos: [linux]
    goarch: [amd64, arm64]
  - goos: [windows]
    goarch: [amd64]
`,
			declared: []string{"linux_amd64", "linux_arm64", "windows_amd64"},
			wantErr:  "",
		},
		{
			name:     "an extra declared platform not in the matrix",
			content:  goreleaserFixture,
			declared: []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64", "windows_amd64", "freebsd_amd64"},
			wantErr:  "drifted",
		},
		{
			name:     "a missing declared platform",
			content:  goreleaserFixture,
			declared: []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"},
			wantErr:  "drifted",
		},
		{
			name:     "YAML that does not parse at all",
			content:  "builds: [this is not, valid: yaml: at all",
			declared: []string{"linux_amd64"},
			wantErr:  "parsing",
		},
		{
			name:     "valid YAML with no builds key",
			content:  "version: 2\nproject_name: foo\n",
			declared: []string{"linux_amd64"},
			wantErr:  "no builds",
		},
		{
			name:     "builds present but empty",
			content:  "builds: []\n",
			declared: []string{"linux_amd64"},
			wantErr:  "no builds",
		},
		{
			name: "a build with no goos/goarch yields an empty matrix",
			content: `builds:
  - id: release
`,
			declared: []string{"linux_amd64"},
			wantErr:  "empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkPlatforms(tt.content, tt.declared, ".goreleaser.yml")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}
