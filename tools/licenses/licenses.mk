# Third-party license registry -- generic make machinery.
#
# This file is meant to be copied into another repo along with the rest of
# this directory and included from its root Makefile, unmodified:
#
#   LICENSES_PLATFORMS := linux_amd64 darwin_amd64 ...
#   LICENSES_ALLOWED_MPL := github.com/some/mpl-licensed-module ...
#   include tools/licenses/licenses.mk
#
# See README.md in this directory for the full adoption contract, the
# subcommands this drives, and the guard failure modes.

# Resolved relative to this file (not $(CURDIR)), so the tools/licenses
# folder works after being copied into another repo at any path.
LICENSES_TOOL_DIR := ./$(patsubst %/,%,$(dir $(lastword $(MAKEFILE_LIST))))

GO_LICENSES_VERSION ?= v1.6.0
LICENSES_GORELEASER ?= .goreleaser.yml

# No module path, go-licenses binary path, or install step to configure
# here: the tool derives its own module via `go list -m`, and installs and
# caches go-licenses itself under the user cache dir, keyed by version --
# this file only hands it the version to pin.

.PHONY: licenses licenses-platform-guard licenses-check

# Regenerates NOTICE.txt and licenses/: for every release platform, unions
# go-licenses' `save`/`report` output and runs its license-type gate, then
# re-verifies LICENSES_ALLOWED_MPL against the merged result (tools/licenses'
# `generate` command).
licenses:
	go run $(LICENSES_TOOL_DIR) generate \
		--go-licenses-version $(GO_LICENSES_VERSION) \
		--platforms "$(LICENSES_PLATFORMS)" \
		--mpl "$(LICENSES_ALLOWED_MPL)"

# Verifies LICENSES_PLATFORMS still matches LICENSES_GORELEASER's build
# matrix (tools/licenses' `check-platforms` command).
licenses-platform-guard:
	go run $(LICENSES_TOOL_DIR) check-platforms --goreleaser $(LICENSES_GORELEASER) --platforms "$(LICENSES_PLATFORMS)"

# CI guard (docs-check pattern): verify the platform list, regenerate --
# which itself verifies every dependency's license type and the MPL-2.0
# exception list -- then fail if git sees a diff, meaning the registry
# wasn't committed alongside whatever dependency change produced it.
licenses-check: licenses-platform-guard licenses
	@if [ -n "$$(git status --porcelain -- NOTICE.txt licenses)" ]; then \
		echo "ERROR: NOTICE.txt/licenses are out of date. Run 'make licenses' and commit the result:"; \
		git status --short -- NOTICE.txt licenses; \
		git --no-pager diff -- NOTICE.txt; \
		exit 1; \
	fi
	@echo "licenses-check OK"
