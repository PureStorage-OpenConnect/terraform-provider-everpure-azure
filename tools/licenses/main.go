// SPDX-License-Identifier: Apache-2.0

// Command licenses backs the Makefile's licenses/licenses-check targets: it
// regenerates the third-party notice registry (NOTICE.txt and licenses/) and
// verifies it, so that logic reads as plain Go instead of a shell script.
//
// Subcommands:
//
//	generate         regenerate NOTICE.txt and licenses/ for every release
//	                 platform, verifying every dependency's license type and
//	                 the MPL-2.0 exception list along the way
//	check-platforms  verify the release platform list against .goreleaser.yml
package main

import (
	"errors"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fatal(usageError())
	}

	var err error
	switch os.Args[1] {
	case "generate":
		err = runGenerate(os.Args[2:])
	case "check-platforms":
		err = runCheckPlatforms(os.Args[2:])
	default:
		err = usageError()
	}
	if err != nil {
		fatal(err)
	}
}

func usageError() error {
	return errors.New("usage: licenses <generate|check-platforms> [flags]")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	os.Exit(1)
}
