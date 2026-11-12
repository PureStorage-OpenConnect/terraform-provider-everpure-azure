// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// goreleaserConfig is the slice of a goreleaser config this tool cares
// about: the build matrix. Everything else in the file is ignored by
// yaml.Unmarshal.
type goreleaserConfig struct {
	Builds []goreleaserBuild `yaml:"builds"`
}

// goreleaserBuild is one entry of goreleaserConfig.Builds.
type goreleaserBuild struct {
	Goos   []string           `yaml:"goos"`
	Goarch []string           `yaml:"goarch"`
	Ignore []goreleaserIgnore `yaml:"ignore"`
}

// goreleaserIgnore is one goos/goarch pair excluded from its build's matrix.
type goreleaserIgnore struct {
	Goos   string `yaml:"goos"`
	Goarch string `yaml:"goarch"`
}

// runCheckPlatforms verifies that the Makefile's LICENSES_PLATFORMS still
// matches .goreleaser.yml's build matrix, so the two can't silently drift
// apart.
func runCheckPlatforms(args []string) error {
	fs := flag.NewFlagSet("check-platforms", flag.ExitOnError)
	goreleaserPath := fs.String("goreleaser", ".goreleaser.yml", "path to the goreleaser config to check the platform list against")
	platformsFlag := fs.String("platforms", "", "space-separated GOOS_GOARCH platforms declared in the Makefile")
	if err := fs.Parse(args); err != nil {
		return err
	}

	declared := strings.Fields(*platformsFlag)
	if len(declared) == 0 {
		return errors.New("--platforms must list at least one GOOS_GOARCH pair")
	}

	data, err := os.ReadFile(*goreleaserPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", *goreleaserPath, err)
	}
	return checkPlatforms(string(data), declared, *goreleaserPath)
}

// checkPlatforms is runCheckPlatforms' file-I/O-free core: given a
// .goreleaser.yml's content and the declared platform list, it reports
// whether they agree. The computed matrix is the UNION of every build
// entry's own goos x goarch matrix (each minus that build's own ignore
// list), not an intersection: attribution must cover every platform any of
// the config's builds actually distributes, so a platform only one build
// out of several produces still has to be in LICENSES_PLATFORMS. A config
// that fails to parse, declares no builds, or yields an empty matrix is a
// hard failure, never "nothing to check".
func checkPlatforms(goreleaserContent string, declared []string, goreleaserPath string) error {
	var cfg goreleaserConfig
	if err := yaml.Unmarshal([]byte(goreleaserContent), &cfg); err != nil {
		return fmt.Errorf("parsing %s: %w", goreleaserPath, err)
	}
	if len(cfg.Builds) == 0 {
		return fmt.Errorf("%s declares no builds", goreleaserPath)
	}

	computed := buildMatrix(cfg.Builds)
	if len(computed) == 0 {
		return fmt.Errorf("%s's builds yield an empty goos/goarch matrix", goreleaserPath)
	}

	computedSorted, declaredSorted := sortedCopy(computed), sortedCopy(declared)
	if !slices.Equal(computedSorted, declaredSorted) {
		return fmt.Errorf(
			"LICENSES_PLATFORMS (Makefile) has drifted from %s's build matrix\n"+
				"  %s (goos x goarch, minus ignore):\n    %s\n"+
				"  LICENSES_PLATFORMS:\n    %s\n"+
				"update LICENSES_PLATFORMS in the Makefile to match %s",
			goreleaserPath, goreleaserPath, strings.Join(computedSorted, "\n    "),
			strings.Join(declaredSorted, "\n    "), goreleaserPath,
		)
	}

	fmt.Println("licenses-platform-guard OK")
	return nil
}

// buildMatrix returns the deduplicated union, as "goos_goarch" strings, of
// every build's own goos x goarch pairs minus that build's own ignore list.
func buildMatrix(builds []goreleaserBuild) []string {
	seen := make(map[string]bool)
	var pairs []string
	for _, b := range builds {
		for _, o := range b.Goos {
			for _, a := range b.Goarch {
				if b.ignores(o, a) {
					continue
				}
				pair := o + "_" + a
				if !seen[pair] {
					seen[pair] = true
					pairs = append(pairs, pair)
				}
			}
		}
	}
	return pairs
}

// ignores reports whether this build's own ignore list excludes the given
// goos/goarch pair.
func (b goreleaserBuild) ignores(goos, goarch string) bool {
	for _, ig := range b.Ignore {
		if ig.Goos == goos && ig.Goarch == goarch {
			return true
		}
	}
	return false
}

func sortedCopy(items []string) []string {
	sorted := slices.Clone(items)
	slices.Sort(sorted)
	return sorted
}
