BINARY  := terraform-provider-everpure-azure
VERSION := 0.0.1-dev
OS_ARCH := $(shell go env GOOS)_$(shell go env GOARCH)
INSTALL_DIR := $(HOME)/.terraform.d/plugins/registry.terraform.io/purestorage-openconnect/everpure-azure/$(VERSION)/$(OS_ARCH)

LDFLAGS := -X main.version=$(VERSION)

.PHONY: build install vet fmt fmt-check test test-unit tidy docs docs-check terraform-bin

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

install: build
	mkdir -p $(INSTALL_DIR)
	cp $(BINARY) $(INSTALL_DIR)/

vet:
	go vet ./...

lint:
	go tool golangci-lint run --config .golangci.yml ./...

# Formatting with autofix (local use; CI runs `lint` which only reports).
format:
	go tool golangci-lint fmt --config .golangci.yml

GORELEASER_VERSION := v2.17.1
goreleaser-check:
	go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION) check --config .goreleaser.yml

# Write the CHANGELOG.md section of RELEASE_VERSION (X.Y.Z) to RELEASE_NOTES;
# fails when the section is missing or empty.
.PHONY: release-notes
release-notes:
	@[ -n "$(RELEASE_VERSION)" ] && [ -n "$(RELEASE_NOTES)" ] || \
		{ echo "ERROR: usage: make release-notes RELEASE_VERSION=X.Y.Z RELEASE_NOTES=/path/to/notes.md"; exit 1; }
	@awk '/^## v$(RELEASE_VERSION)([[:space:]]|$$)/{found=1;next} /^## v[0-9]/{if(found)exit} found{print}' \
		CHANGELOG.md > "$(RELEASE_NOTES)"
	@[ -s "$(RELEASE_NOTES)" ] || \
		{ echo "ERROR: no content found for ## v$(RELEASE_VERSION) in CHANGELOG.md"; exit 1; }

# Import the armored private key in GPG_PRIVATE_KEY into the keyring directory
# GNUPGHOME (created if missing) and print its fingerprint, the only output on
# stdout. Fails when no secret key comes out of the import.
.PHONY: gpg-import
gpg-import:
	@[ -n "$$GNUPGHOME" ] && [ -n "$$GPG_PRIVATE_KEY" ] || \
		{ echo "ERROR: usage: GNUPGHOME=/keyring/dir GPG_PRIVATE_KEY=<armored key> make gpg-import" >&2; exit 1; }
	@install -d -m 700 "$$GNUPGHOME"
	@printf '%s\n' "$$GPG_PRIVATE_KEY" | gpg --batch --quiet --import
	@FPR=$$(gpg --list-secret-keys --with-colons | awk -F: '/^fpr:/ {print $$10; exit}'); \
		[ -n "$$FPR" ] || { echo "ERROR: no secret key in $$GNUPGHOME after the import" >&2; exit 1; }; \
		echo "$$FPR"

# Build the checked-out tag and publish it as a GitHub Release of the repository
# `origin` points at. Needs GITHUB_TOKEN, GPG_FINGERPRINT naming an imported key
# to sign the checksum file with, and PASSPHRASE unlocking that key.
.PHONY: goreleaser-release
goreleaser-release:
	go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION) release \
		--config .goreleaser.yml --clean \
		$(if $(RELEASE_NOTES),--release-notes $(RELEASE_NOTES))

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

test: terraform-bin
	TF_ACC_TERRAFORM_VERSION=$(TERRAFORM_VERSION) TF_ACC_TERRAFORM_PATH=$(TERRAFORM_BIN) go test ./...

# Fast loop: unit tests only (no terraform binary needed).
test-unit:
	go test -short ./...

tidy:
	go mod tidy

# Registry documentation. `docs/` is generated from the resource schemas'
# descriptions plus `templates/` and `examples/`, and is committed — the
# registry publishes what is in the repo, not what CI builds.
#
# tfplugindocs shells out to the terraform CLI to dump the provider schema.
# The version is pinned rather than taken from PATH so that generation is
# reproducible and works on runners with no terraform installed (it downloads
# the pinned build into a temp dir).
#
# --provider-name is `everpure`, not `everpure-azure`: it has to be a valid
# Terraform provider local name (no underscores) AND the prefix that
# tfplugindocs strips off resource type names to derive page filenames. Our
# resources are `everpure_azure_*`, which is also what Terraform's implied
# local name resolves to, so pages land at docs/resources/azure_<name>.md.
TERRAFORM_VERSION := 1.16.1
TFPLUGINDOCS_FLAGS := \
	--provider-name everpure \
	--rendered-provider-name "Everpure Azure" \
	--tf-version $(TERRAFORM_VERSION)

# With only TF_ACC_TERRAFORM_VERSION set, terraform-plugin-testing downloads
# Terraform again for every resource.Test; TF_ACC_TERRAFORM_PATH makes it use
# this cached binary instead.
TERRAFORM_CACHE_DIR := $(CURDIR)/.terraform-bin
TERRAFORM_BIN        := $(TERRAFORM_CACHE_DIR)/$(TERRAFORM_VERSION)/terraform

terraform-bin:
	@if "$(TERRAFORM_BIN)" version 2>/dev/null | head -n1 | grep -qx "Terraform v$(TERRAFORM_VERSION)"; then \
		echo "terraform $(TERRAFORM_VERSION) already cached at $(TERRAFORM_BIN)"; \
	else \
		mkdir -p "$(TERRAFORM_CACHE_DIR)/$(TERRAFORM_VERSION)" && \
		go run github.com/hashicorp/hc-install/cmd/hc-install install -version $(TERRAFORM_VERSION) -path "$(TERRAFORM_CACHE_DIR)/$(TERRAFORM_VERSION)" terraform; \
	fi

docs:
	go tool tfplugindocs generate $(TFPLUGINDOCS_FLAGS)

# CI guard: docs/ must be regenerated in the same commit as the schema change
# that affects it. Regenerates in place, then asks git whether anything moved —
# `git status` rather than `git diff` so that a brand-new (untracked) page for a
# brand-new resource is caught too.
docs-check: docs
	@if [ -n "$$(git status --porcelain -- docs)" ]; then \
		echo "ERROR: docs/ is out of date. Run 'make docs' and commit the result:"; \
		git status --short -- docs; \
		git --no-pager diff -- docs; \
		exit 1; \
	fi
	@echo "docs OK"

# Third-party notices. NOTICE.txt and licenses/ are generated from the import
# graph of the main package (`.`) only: tool-only and test-only dependencies
# never ship in the provider binary, so they carry no attribution obligation.
# The generation/check machinery itself is a self-contained, copy-elsewhere
# drop-in (tools/licenses/README.md); this repo only supplies its inputs.

# Every distributed build; must match .goreleaser.yml's goos/goarch/ignore —
# licenses-platform-guard fails CI on drift.
LICENSES_PLATFORMS := linux_amd64 linux_arm64 darwin_amd64 darwin_arm64 windows_amd64

# Reviewed MPL-2.0 exceptions to the license-type gate below -- see
# tools/licenses/checkmpl.go (why MPL-2.0 is fine, and why each module is
# listed individually) and generate.go (why they're excluded from `save`).
LICENSES_ALLOWED_MPL := \
	github.com/hashicorp/go-plugin \
	github.com/hashicorp/go-uuid \
	github.com/hashicorp/terraform-plugin-framework \
	github.com/hashicorp/terraform-plugin-framework-timeouts \
	github.com/hashicorp/terraform-plugin-framework-validators \
	github.com/hashicorp/terraform-plugin-go \
	github.com/hashicorp/terraform-plugin-log \
	github.com/hashicorp/terraform-registry-address \
	github.com/hashicorp/terraform-svchost \
	github.com/hashicorp/yamux

# Hard include (not -include): a missing tools/licenses/licenses.mk must
# break the build loudly, never silently turn licenses-check into a no-op.
include tools/licenses/licenses.mk

# Internal-only targets, excluded from public releases
-include private/Makefile.mk
