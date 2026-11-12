// SPDX-License-Identifier: Apache-2.0

// Package isoduration parses, formats and compares ISO 8601 durations limited
// to days-or-smaller units (no weeks, months or years). It is pure string/time
// processing with no framework dependency, so it can be used anywhere a
// days-or-smaller ISO 8601 duration needs to be parsed, canonically formatted,
// or compared for semantic equality (e.g. by a Terraform custom type wrapping
// it, or by any other caller with the same grammar).
package isoduration

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// daysOrSmallerPattern matches the duration grammar this package accepts: an
// optional day count, optionally followed by a time component of hours,
// minutes and seconds. At least one component must be present. A day is
// exactly 24 hours here, matching how APIs that canonicalize on read collapse
// PT24H to P1D.
var daysOrSmallerPattern = regexp.MustCompile(`^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$`)

// InvalidError is returned by Parse for every rejected input: not a duration at all, a
// dangling "T", no components, a unit larger than days, or a count too large to hold.
type InvalidError struct {
	Input string
}

func (e *InvalidError) Error() string {
	return fmt.Sprintf("%q is not an ISO 8601 duration in days or smaller", e.Input)
}

// Parse parses an ISO 8601 duration string limited to days-or-smaller units,
// returning its total length. Weeks, months and years are rejected. Every
// rejection is an *InvalidError.
func Parse(s string) (time.Duration, error) {
	invalid := &InvalidError{Input: s}
	if s == "" || s == "P" || s == "PT" {
		return 0, invalid
	}

	// A trailing "T" with nothing after it ("P1DT") is a dangling time
	// designator, not a valid duration. The early check above already rejects a
	// bare "PT", so reaching here with the string ending in "T" can only mean a
	// dangling one — checking the literal suffix is simpler than threading an
	// extra capture group through the regex below just to detect it.
	if strings.HasSuffix(s, "T") {
		return 0, invalid
	}

	m := daysOrSmallerPattern.FindStringSubmatch(s)
	if m == nil || (m[1] == "" && m[2] == "" && m[3] == "" && m[4] == "") {
		return 0, invalid
	}

	var total time.Duration
	for i, unit := range []time.Duration{24 * time.Hour, time.Hour, time.Minute, time.Second} {
		if m[i+1] == "" {
			continue
		}
		count, err := strconv.ParseInt(m[i+1], 10, 64)
		if err != nil {
			return 0, invalid
		}
		total += time.Duration(count) * unit
	}

	return total, nil
}

// Equal reports whether a and b denote the same length of time once parsed,
// e.g. "PT60M" and "PT1H". It returns an error, without a meaningful bool,
// if either string fails to parse — callers that want to treat an unparseable
// value as simply "not equal" (rather than propagating the error) can ignore
// it and use the zero-value false returned alongside it.
func Equal(a, b string) (bool, error) {
	da, err := Parse(a)
	if err != nil {
		return false, err
	}
	db, err := Parse(b)
	if err != nil {
		return false, err
	}
	return da == db, nil
}
