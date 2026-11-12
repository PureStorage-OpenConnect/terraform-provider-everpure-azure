// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"sort"
)

// reportEntry is one row of a `go-licenses report` CSV: a dependency's
// license-grouping path (a module path, or occasionally a package path
// within one — whichever directory go-licenses found the nearest license
// file in), the upstream URL to its license text, and the detected license
// name.
type reportEntry struct {
	Name       string
	LicenseURL string
	License    string
}

// parseReportEntries reads the plain 3-column CSV that `go-licenses report`
// (without --template) writes to stdout.
func parseReportEntries(r io.Reader) ([]reportEntry, error) {
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parsing go-licenses report output: %w", err)
	}
	entries := make([]reportEntry, 0, len(rows))
	for _, row := range rows {
		if len(row) != 3 {
			return nil, fmt.Errorf("expected 3 columns in go-licenses report output, got %d: %v", len(row), row)
		}
		entries = append(entries, reportEntry{Name: row[0], LicenseURL: row[1], License: row[2]})
	}
	return entries, nil
}

// sortUniqueEntries sorts entries the same way `LC_ALL=C sort -u` would sort
// their CSV lines, and drops exact duplicates — the union of several
// platforms' reports re-lists any dependency common to more than one of them.
func sortUniqueEntries(entries []reportEntry) []reportEntry {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.LicenseURL != b.LicenseURL {
			return a.LicenseURL < b.LicenseURL
		}
		return a.License < b.License
	})

	unique := entries[:0]
	for i, e := range entries {
		if i == 0 || e != unique[len(unique)-1] {
			unique = append(unique, e)
		}
	}
	return unique
}
