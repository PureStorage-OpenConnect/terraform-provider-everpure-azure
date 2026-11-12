// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"time"

	atimeouts "github.com/hashicorp/terraform-plugin-framework-timeouts/action/timeouts"
	dstimeouts "github.com/hashicorp/terraform-plugin-framework-timeouts/datasource/timeouts"
	etimeouts "github.com/hashicorp/terraform-plugin-framework-timeouts/ephemeral/timeouts"
	lstimeouts "github.com/hashicorp/terraform-plugin-framework-timeouts/list/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	aschema "github.com/hashicorp/terraform-plugin-framework/action/schema"
	dsschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	esschema "github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	lsschema "github.com/hashicorp/terraform-plugin-framework/list/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
)

// durationFormat is the sentence every timeouts description uses to say how a duration
// is written.
const durationFormat = `A duration string: a number with a unit suffix s (seconds), m (minutes) or h ` +
	`(hours); segments can be combined, e.g. "30m" or "1h30m".`

// timeoutsDefaults carries a resource's default per-operation durations into
// timeoutsBlock, so the generated `timeouts` block's descriptions state the
// same defaults the resource's own Create/Read/Update/Delete methods resolve to via
// data.Timeouts.Create(ctx, defaultXCreateTimeout) and friends. Each resource builds
// this from its own defaultX*Timeout constants — the single place those durations are
// spelled out as literals; nothing here restates them.
type timeoutsDefaults struct {
	Create time.Duration
	Read   time.Duration
	Update time.Duration
	Delete time.Duration
}

// timeoutsBlock is the standard `timeouts` block every resource in this
// provider exposes. A zero defaults.Update omits the update timeout altogether, for a
// resource whose Update makes no API call (nothing to time out). resourceNoun is how
// the resource reads in a sentence like "Used when creating <resourceNoun>." (e.g. "the
// pool", "the volume group"). deleteExtra, if non-empty, is appended as a clause to the
// delete description — e.g. to note that eradicate_on_destroy makes delete cover a
// second operation, which is why that timeout's default is longer than create's.
func timeoutsBlock(ctx context.Context, resourceNoun string, defaults timeoutsDefaults, deleteExtra string) schema.Block {
	return timeouts.Block(ctx, timeouts.Opts{
		Create: true,
		Read:   true,
		Update: defaults.Update > 0,
		Delete: true,
		CreateDescription: fmt.Sprintf(
			"Defaults to %s. %s Used when creating %s.",
			humanMinutes(defaults.Create), durationFormat, resourceNoun,
		),
		ReadDescription: fmt.Sprintf(
			"Defaults to %s. %s Used when reading %s.",
			humanMinutes(defaults.Read), durationFormat, resourceNoun,
		),
		UpdateDescription: fmt.Sprintf(
			"Defaults to %s. %s Used when updating %s.",
			humanMinutes(defaults.Update), durationFormat, resourceNoun,
		),
		DeleteDescription: fmt.Sprintf(
			"Defaults to %s. %s Used when destroying %s%s.",
			humanMinutes(defaults.Delete), durationFormat, resourceNoun, deleteExtra,
		),
	})
}

// dataSourceTimeoutsBlock is the standard `timeouts` block every data source in
// this provider exposes: read-only, since a data source only ever reads. resourceNoun
// and the description wording match timeoutsBlock above, so a data source and its
// resource read the same way in the generated docs.
func dataSourceTimeoutsBlock(ctx context.Context, resourceNoun string, read time.Duration) dsschema.Block {
	return dstimeouts.BlockWithOpts(ctx, dstimeouts.Opts{
		ReadDescription: fmt.Sprintf(
			"Defaults to %s. %s Used when reading %s.",
			humanMinutes(read), durationFormat, resourceNoun,
		),
	})
}

// ephemeralTimeoutsBlock is the standard `timeouts` block every ephemeral
// resource in this provider exposes: a single `open` duration covering the whole Open
// call, SDK retries included. resourceNoun and the description wording match
// dataSourceTimeoutsBlock above.
func ephemeralTimeoutsBlock(ctx context.Context, resourceNoun string, open time.Duration) esschema.Block {
	return etimeouts.BlockWithOpts(ctx, etimeouts.Opts{
		OpenDescription: fmt.Sprintf(
			"Defaults to %s. %s Used when requesting %s.",
			humanMinutes(open), durationFormat, resourceNoun,
		),
	})
}

// actionTimeoutsBlock is the standard `timeouts` block every action in this
// provider exposes: a single `invoke` duration covering the whole Invoke call, SDK
// retries included. actionNoun and the description wording match
// ephemeralTimeoutsBlock above.
func actionTimeoutsBlock(ctx context.Context, actionNoun string, invoke time.Duration) aschema.Block {
	return atimeouts.BlockWithOpts(ctx, atimeouts.Opts{
		InvokeDescription: fmt.Sprintf(
			"Defaults to %s. %s Used when %s.",
			humanMinutes(invoke), durationFormat, actionNoun,
		),
	})
}

// listTimeoutsBlock is the standard `timeouts` block every list resource in this
// provider exposes: a single `list` duration covering the paging calls, defaulting to
// the matching resource's read timeout like the plural data sources do. resourceNoun
// and the description wording match dataSourceTimeoutsBlock above.
func listTimeoutsBlock(ctx context.Context, resourceNoun string, list time.Duration) lsschema.Block {
	return lstimeouts.BlockWithOpts(ctx, lstimeouts.Opts{
		ListDescription: fmt.Sprintf(
			"Defaults to %s. %s Used when listing %s.",
			humanMinutes(list), durationFormat, resourceNoun,
		),
	})
}

// humanMinutes renders a duration as whole minutes in prose ("95 minutes"), rather
// than Go's own compact-but-unfriendly formatting (Duration.String() would print
// "1h35m0s"). Every default timeout in this provider is a whole number of minutes, so
// this is exact, not a rounding approximation.
func humanMinutes(d time.Duration) string {
	minutes := int64(d / time.Minute)
	if minutes == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", minutes)
}
