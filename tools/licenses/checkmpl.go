// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"strings"
)

// mplLicenseName is the exact license name go-licenses must report for a
// module on the MPL-2.0 exception list; anything else means it relicensed.
const mplLicenseName = "MPL-2.0"

// checkMPL re-verifies every module on the reviewed MPL-2.0 exception list
// against the merged report generate built: each one must still show up in
// the dependency graph (otherwise it's a stale exception -- remove it) and
// must still carry exactly the MPL-2.0 license (otherwise it's a silent
// upstream relicense -- re-review it). MPL-2.0 is a weak, file-level
// copyleft: its share-back obligation attaches only to the MPL-licensed
// files themselves, and since these libraries are used unmodified, pointing
// at their upstream source (the per-dependency URLs in NOTICE.txt) is the
// only obligation left. go-licenses classes MPL-2.0 as "reciprocal", which
// the license-type gate disallows wholesale, so each reviewed module is
// exempted individually here instead of allowing the whole category --
// that would silently admit any future copyleft dependency, MPL or
// otherwise.
func checkMPL(entries []reportEntry, mpl []string) error {
	var failed bool
	for _, m := range mpl {
		rows := entriesForModule(entries, m)
		if len(rows) == 0 {
			fmt.Printf("ERROR: %s no longer appears in the dependency graph -- it's a stale exception, remove it from LICENSES_ALLOWED_MPL.\n", m)
			failed = true
			continue
		}
		for _, row := range rows {
			if row.License != mplLicenseName {
				fmt.Printf("ERROR: %s's license changed and is no longer %s -- re-review required:\n    %s,%s,%s\n",
					m, mplLicenseName, row.Name, row.LicenseURL, row.License)
				failed = true
			}
		}
	}
	if failed {
		return errors.New("one or more LICENSES_ALLOWED_MPL entries failed re-verification")
	}

	fmt.Println("licenses-allowed-mpl OK")
	return nil
}

// entriesForModule returns every report row whose name is modulePath itself
// or a package path within it.
func entriesForModule(entries []reportEntry, modulePath string) []reportEntry {
	var rows []reportEntry
	prefix := modulePath + "/"
	for _, e := range entries {
		if e.Name == modulePath || strings.HasPrefix(e.Name, prefix) {
			rows = append(rows, e)
		}
	}
	return rows
}
