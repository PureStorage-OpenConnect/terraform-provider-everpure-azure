// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// storageObjectNamePattern is the name grammar that volume groups, volumes and
// snapshots all share for their name path parameter: one pattern, so all three
// resources accept and reject exactly the same names. 2-40 characters, must
// start with a letter, must end with a letter or digit; hyphens are allowed
// everywhere in between.
var storageObjectNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,38}[A-Za-z0-9]$`)

// storageObjectNameValidator is the shared name validator for volume groups, volumes
// and snapshots, so all three resources reject the same names the same way.
func storageObjectNameValidator() validator.String {
	return stringvalidator.RegexMatches(storageObjectNamePattern, storageObjectNameRule)
}
