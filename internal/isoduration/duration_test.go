// SPDX-License-Identifier: Apache-2.0

package isoduration

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	t.Run("valid durations", func(t *testing.T) {
		tests := []struct {
			name string
			in   string
			want string // a semantically equal reference duration string
		}{
			{"days", "P7D", "P7D"},
			{"hours", "PT1H", "PT1H"},
			{"minutes equal to hours", "PT60M", "PT1H"},
			{"hours equal to days", "PT48H", "P2D"},
			{"minutes", "PT30M", "PT30M"},
			{"seconds", "PT45S", "PT45S"},
			{"combined time components", "PT1H30M", "PT1H30M"},
			{"days plus time", "P1DT1H", "P1DT1H"},
			{"zero seconds", "PT0S", "PT0S"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got, err := Parse(tt.in)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				want, err := Parse(tt.want)
				if err != nil {
					t.Fatalf("unexpected error parsing reference: %v", err)
				}
				if got != want {
					t.Fatalf("Parse(%q) = %v, want %v (== Parse(%q))", tt.in, got, want, tt.want)
				}
			})
		}
	})

	t.Run("PT60M and PT1H parse to the same duration", func(t *testing.T) {
		a, err := Parse("PT60M")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		b, err := Parse("PT1H")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if a != b {
			t.Fatalf("PT60M = %v, PT1H = %v, want equal", a, b)
		}
	})

	t.Run("PT48H and P2D parse to the same duration", func(t *testing.T) {
		a, err := Parse("PT48H")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		b, err := Parse("P2D")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if a != b {
			t.Fatalf("PT48H = %v, P2D = %v, want equal", a, b)
		}
	})

	t.Run("rejects weeks, months and years with an InvalidError carrying the input", func(t *testing.T) {
		for _, in := range []string{"P1W", "P1M", "P1Y", "P1Y2M3D"} {
			t.Run(in, func(t *testing.T) {
				_, err := Parse(in)
				var invalid *InvalidError
				if !errors.As(err, &invalid) {
					t.Fatalf("Parse(%q) error = %v, want an *InvalidError", in, err)
				}
				if invalid.Input != in {
					t.Fatalf("InvalidError.Input = %q, want %q", invalid.Input, in)
				}
			})
		}
	})

	t.Run("rejects garbage and empty/incomplete forms", func(t *testing.T) {
		for _, in := range []string{"", "P", "PT", "garbage", "1D", "PXD", "P1DT", "P99999999999999999999D"} {
			t.Run(in, func(t *testing.T) {
				_, err := Parse(in)
				var invalid *InvalidError
				if !errors.As(err, &invalid) {
					t.Fatalf("Parse(%q) error = %v, want an *InvalidError", in, err)
				}
			})
		}
	})

	t.Run("rejects a dangling T designator with no hours, minutes or seconds after it", func(t *testing.T) {
		for _, in := range []string{"P1DT", "PT"} {
			t.Run(in, func(t *testing.T) {
				_, err := Parse(in)
				if err == nil {
					t.Fatalf("expected an error for %q, got nil", in)
				}
			})
		}
	})

	t.Run("still accepts P0D and PT0S", func(t *testing.T) {
		for _, in := range []string{"P0D", "PT0S"} {
			t.Run(in, func(t *testing.T) {
				if _, err := Parse(in); err != nil {
					t.Fatalf("unexpected error for %q: %v", in, err)
				}
			})
		}
	})
}

func TestEqual(t *testing.T) {
	t.Run("PT60M and PT1H are equal", func(t *testing.T) {
		equal, err := Equal("PT60M", "PT1H")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !equal {
			t.Fatal("expected PT60M and PT1H to be equal")
		}
	})

	t.Run("PT2H and PT1H are not equal", func(t *testing.T) {
		equal, err := Equal("PT2H", "PT1H")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if equal {
			t.Fatal("expected PT2H and PT1H not to be equal")
		}
	})

	t.Run("an unparseable first argument returns an error", func(t *testing.T) {
		if _, err := Equal("garbage", "PT1H"); err == nil {
			t.Fatal("expected an error, got nil")
		}
	})

	t.Run("an unparseable second argument returns an error", func(t *testing.T) {
		if _, err := Equal("PT1H", "garbage"); err == nil {
			t.Fatal("expected an error, got nil")
		}
	})
}
