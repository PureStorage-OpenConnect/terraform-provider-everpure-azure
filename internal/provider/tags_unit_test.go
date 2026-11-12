// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"reflect"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestTagsFromAPI(t *testing.T) {
	ctx := context.Background()

	t.Run("returns null when the API has no tags and config was null", func(t *testing.T) {
		got, diags := tagsFromAPI(ctx, nil, types.MapNull(types.StringType))
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if !got.IsNull() {
			t.Fatal("expected a null map")
		}
	})

	t.Run("returns an empty map when the API has no tags but config was an empty map", func(t *testing.T) {
		emptyConfig, d := types.MapValueFrom(ctx, types.StringType, map[string]string{})
		if d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}

		got, diags := tagsFromAPI(ctx, nil, emptyConfig)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if got.IsNull() {
			t.Fatal("expected a non-null map")
		}
		if len(got.Elements()) != 0 {
			t.Fatalf("got %v elements, want 0", len(got.Elements()))
		}
	})

	t.Run("maps API tags through", func(t *testing.T) {
		apiTags := map[string]*string{"environment": to.Ptr("prod")}
		got, diags := tagsFromAPI(ctx, apiTags, types.MapNull(types.StringType))
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}

		var m map[string]string
		if d := got.ElementsAs(ctx, &m, false); d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}
		want := map[string]string{"environment": "prod"}
		if !reflect.DeepEqual(m, want) {
			t.Fatalf("got %v, want %v", m, want)
		}
	})
}

func TestTagsToAPI(t *testing.T) {
	ctx := context.Background()

	t.Run("returns nil for a null map, so PATCH omits the tags key", func(t *testing.T) {
		got, diags := tagsToAPI(ctx, types.MapNull(types.StringType))
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if got != nil {
			t.Fatalf("got %v, want nil", got)
		}
	})

	t.Run("returns a non-nil empty map for an empty-but-non-null map, so PATCH can clear tags", func(t *testing.T) {
		empty, d := types.MapValueFrom(ctx, types.StringType, map[string]string{})
		if d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}

		got, diags := tagsToAPI(ctx, empty)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if got == nil {
			t.Fatal("got nil, want a non-nil empty map")
		}
		if len(got) != 0 {
			t.Fatalf("got %v, want empty", got)
		}
	})

	t.Run("maps a populated map through", func(t *testing.T) {
		tags, d := types.MapValueFrom(ctx, types.StringType, map[string]string{"environment": "prod"})
		if d.HasError() {
			t.Fatalf("unexpected diagnostics: %v", d)
		}

		got, diags := tagsToAPI(ctx, tags)
		if diags.HasError() {
			t.Fatalf("unexpected diagnostics: %v", diags)
		}
		if got["environment"] == nil || *got["environment"] != "prod" {
			t.Fatalf("got %v, want environment=prod", got)
		}
	})
}
