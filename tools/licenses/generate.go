// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// licensesDir, noticeHeaderFile and noticeOutputFile are this registry's
// fixed layout: the per-dependency license texts, the hand-written
// NOTICE.txt header, and the generated NOTICE.txt itself, all at the repo
// root.
const (
	licensesDir      = "licenses"
	noticeHeaderFile = "NOTICE.header"
	noticeOutputFile = "NOTICE.txt"
)

// disallowedLicenseTypes are the go-licenses license categories this
// registry never accepts automatically: forbidden and unknown for obvious
// reasons, restricted and reciprocal because both carry copyleft-style
// redistribution obligations this project does not take on by default.
// This policy is fixed, not a flag -- the only sanctioned way past it is a
// reviewed, per-module entry in the MPL-2.0 exception list, never a
// blanket allowance for a whole type.
const disallowedLicenseTypes = "forbidden,unknown,restricted,reciprocal"

// licenseFilePattern matches the license file go-licenses save leaves in a
// notice/permissive/unencumbered dependency's directory.
var licenseFilePattern = regexp.MustCompile(`(?i)^(license|copying)(\.(txt|md))?$`)

// versionPattern pulls a semver-shaped version out of a license URL, e.g.
// ".../blob/v1.23.1/..." -> "v1.23.1". Some pseudo-versioned dependencies'
// license URLs only ever embed a bare commit hash, so this can legitimately
// find nothing.
var versionPattern = regexp.MustCompile(`v[0-9]+\.[0-9]+\.[0-9]+[A-Za-z0-9.+-]*`)

// runGenerate regenerates NOTICE.txt and the licenses/ directory: for every
// release platform it runs go-licenses' own `save`, `report` and `check`
// against the main package, merges the per-platform `save`/`report` output
// into a union (some dependencies, like golang.org/x/sys/windows, only show
// up when analyzing one specific platform), copies the reviewed MPL-2.0
// exceptions' LICENSE files in by hand (see copyModuleLicense), re-verifies
// that exception list against the merged report, and renders NOTICE.txt
// from a hand-written header plus the merged report. Because `check` runs
// as part of this, an unreviewed non-permissive dependency fails generation
// itself -- a registry is never written for a license nobody has reviewed.
func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	goLicensesVersion := fs.String("go-licenses-version", "", "go-licenses version to install and cache")
	module := fs.String("module", "", "this module's own import path, excluded from the registry (default: derived via 'go list -m')")
	pkg := fs.String("package", ".", "the main package whose import graph to analyze")
	platformsFlag := fs.String("platforms", "", "space-separated GOOS_GOARCH platforms to analyze")
	mplFlag := fs.String("mpl", "", "space-separated list of reviewed MPL-2.0 module paths")
	if err := fs.Parse(args); err != nil {
		return err
	}

	platforms := strings.Fields(*platformsFlag)
	mpl := strings.Fields(*mplFlag)
	switch {
	case *goLicensesVersion == "":
		return errors.New("--go-licenses-version is required")
	case len(platforms) == 0:
		return errors.New("--platforms must list at least one GOOS_GOARCH pair")
	}

	bin, err := ensureGoLicenses(*goLicensesVersion)
	if err != nil {
		return err
	}
	mod := *module
	if mod == "" {
		mod, err = currentModulePath()
		if err != nil {
			return err
		}
	}

	if err := os.RemoveAll(licensesDir); err != nil {
		return fmt.Errorf("removing %s: %w", licensesDir, err)
	}
	if err := os.MkdirAll(licensesDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", licensesDir, err)
	}

	merged, err := generateForPlatforms(bin, mod, *pkg, platforms, mpl, licensesDir)
	if err != nil {
		return err
	}
	merged = sortUniqueEntries(merged)

	fmt.Println("verifying MPL-2.0 exceptions...")
	if err := checkMPL(merged, mpl); err != nil {
		return err
	}

	// The MPL exceptions are excluded from `save` above (see
	// generateForPlatforms), whose handling of a reciprocal-type dependency
	// is to copy its entire source tree into licenses/ -- its generic way of
	// satisfying source-redistribution obligations we do not have, since we
	// use these libraries unmodified. Copy just their LICENSE file instead,
	// the same treatment every other dependency gets.
	for _, m := range mpl {
		if err := copyModuleLicense(m, licensesDir); err != nil {
			return err
		}
	}

	return writeNotice(noticeHeaderFile, noticeOutputFile, merged)
}

// generateForPlatforms runs go-licenses' save, report and check once per
// platform, merging the save/report output into dir and the (not yet sorted
// or de-duplicated) report entries returned to the caller. MPL exception
// modules are excluded from `save` -- they would otherwise pull each
// reciprocal dependency's entire source tree into dir -- and from `check`
// -- they are the reviewed exceptions to its license-type gate -- but not
// from `report`, so they still get a NOTICE.txt entry and stay visible to
// the MPL re-verification that runs after this returns.
func generateForPlatforms(goLicensesBin, module, pkg string, platforms, mpl []string, dir string) ([]reportEntry, error) {
	var merged []reportEntry
	for _, p := range platforms {
		goos, goarch, ok := strings.Cut(p, "_")
		if !ok {
			return nil, fmt.Errorf("platform %q is not in GOOS_GOARCH form", p)
		}

		ignoreSelfAndMPL := []string{"--ignore", module}
		for _, m := range mpl {
			ignoreSelfAndMPL = append(ignoreSelfAndMPL, "--ignore", m)
		}

		saveDir, err := os.MkdirTemp("", "go-licenses-save-")
		if err != nil {
			return nil, fmt.Errorf("creating a scratch save directory for %s: %w", p, err)
		}
		saveArgs := append([]string{"save", pkg}, ignoreSelfAndMPL...)
		saveArgs = append(saveArgs, "--save_path", saveDir, "--force")
		if err := runGoLicenses(goLicensesBin, goos, goarch, saveArgs...); err != nil {
			return nil, fmt.Errorf("go-licenses save for %s: %w", p, err)
		}

		mergeErr := mergeNoClobber(saveDir, dir)
		_ = os.RemoveAll(saveDir)
		if mergeErr != nil {
			return nil, fmt.Errorf("merging go-licenses save output for %s into %s: %w", p, dir, mergeErr)
		}

		out, err := runGoLicensesOutput(goLicensesBin, goos, goarch, "report", pkg, "--ignore", module)
		if err != nil {
			return nil, fmt.Errorf("go-licenses report for %s: %w", p, err)
		}
		entries, err := parseReportEntries(strings.NewReader(string(out)))
		if err != nil {
			return nil, fmt.Errorf("go-licenses report for %s: %w", p, err)
		}
		merged = append(merged, entries...)

		checkArgs := append([]string{"check", pkg}, ignoreSelfAndMPL...)
		checkArgs = append(checkArgs, "--disallowed_types", disallowedLicenseTypes)
		if err := runGoLicenses(goLicensesBin, goos, goarch, checkArgs...); err != nil {
			return nil, fmt.Errorf("go-licenses check for %s: %w", p, err)
		}
	}
	return merged, nil
}

// copyModuleLicense copies modulePath's own LICENSE (or COPYING) file into
// dir/modulePath/, the same layout go-licenses save uses for every other
// notice/permissive/unencumbered dependency.
func copyModuleLicense(modulePath, dir string) error {
	moduleDir, err := goListModuleDir(modulePath)
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(moduleDir)
	if err != nil {
		return fmt.Errorf("reading %s: %w", moduleDir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var licenseFile string
	for _, name := range names {
		if licenseFilePattern.MatchString(name) {
			licenseFile = name
			break
		}
	}
	if licenseFile == "" {
		return fmt.Errorf("no LICENSE file found for %s in %s", modulePath, moduleDir)
	}

	destDir := filepath.Join(dir, modulePath)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", destDir, err)
	}
	return copyFile(filepath.Join(moduleDir, licenseFile), filepath.Join(destDir, licenseFile))
}

// writeNotice writes headerPath's content followed by one block per entry,
// in the same layout the old go-licenses report --template produced. A
// per-entry version is recovered from its license URL where the URL happens
// to embed one; pseudo-versioned dependencies whose license URL only embeds
// a commit hash (e.g. github.com/pkg/browser) are listed without a version.
func writeNotice(headerPath, noticePath string, entries []reportEntry) (err error) {
	header, err := os.ReadFile(headerPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", headerPath, err)
	}

	f, err := os.Create(noticePath)
	if err != nil {
		return fmt.Errorf("creating %s: %w", noticePath, err)
	}
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}()

	if _, err := f.Write(header); err != nil {
		return fmt.Errorf("writing %s: %w", noticePath, err)
	}
	for _, e := range entries {
		version := versionPattern.FindString(e.LicenseURL)
		if version != "" {
			_, err = fmt.Fprintf(f, "\n    %s %s\n    License: %s\n    %s\n", e.Name, version, e.License, e.LicenseURL)
		} else {
			_, err = fmt.Fprintf(f, "\n    %s\n    License: %s\n    %s\n", e.Name, e.License, e.LicenseURL)
		}
		if err != nil {
			return fmt.Errorf("writing %s: %w", noticePath, err)
		}
	}
	return nil
}
