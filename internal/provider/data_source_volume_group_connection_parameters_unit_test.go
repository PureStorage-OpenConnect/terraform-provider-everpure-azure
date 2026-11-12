// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
)

func TestConnectionParametersEndpointsAndWarning(t *testing.T) {
	ctx := context.Background()

	t.Run("zero endpoints yields an empty, non-null list and a warning", func(t *testing.T) {
		list, diags := connectionParametersEndpointsAndWarning(ctx, "vg-id", nil)
		if diags.HasError() {
			t.Fatalf("unexpected error diagnostics: %v", diags)
		}
		if list.IsNull() {
			t.Fatal("expected a non-null empty list, got null")
		}
		if got := len(list.Elements()); got != 0 {
			t.Fatalf("expected zero elements, got %d", got)
		}

		var sawWarning bool
		for _, d := range diags {
			if d.Severity() == diag.SeverityWarning {
				sawWarning = true
			}
		}
		if !sawWarning {
			t.Fatalf("expected a warning diagnostic, got %v", diags)
		}
	})

	t.Run("non-empty endpoints are sorted, with no warning", func(t *testing.T) {
		targets := []iscsiTarget{
			{IQN: "iqn:vg1", IP: "10.0.1.5", Port: 3260},
			{IQN: "iqn:vg1", IP: "10.0.1.4", Port: 3260},
		}

		list, diags := connectionParametersEndpointsAndWarning(ctx, "vg-id", targets)
		if diags.HasError() {
			t.Fatalf("unexpected error diagnostics: %v", diags)
		}
		for _, d := range diags {
			if d.Severity() == diag.SeverityWarning {
				t.Fatalf("unexpected warning for non-empty endpoints: %v", d)
			}
		}

		var got []ResolvedTargetModel
		if d := list.ElementsAs(ctx, &got, false); d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}
		if len(got) != 2 {
			t.Fatalf("got %d elements, want 2", len(got))
		}
		if got[0].IP.ValueString() != "10.0.1.4" || got[1].IP.ValueString() != "10.0.1.5" {
			t.Fatalf("expected endpoints sorted by IP, got %+v", got)
		}
	})
}
