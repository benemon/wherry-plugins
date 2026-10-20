GO_LICENSES_VERSION := v1.6.0
GO_LICENSES         := $(shell go env GOPATH)/bin/go-licenses
CYCLONEDX_VERSION   := v1.11.0
CYCLONEDX           := $(shell go env GOPATH)/bin/cyclonedx-gomod

VERSION   ?= dev
DIST      ?= dist
PLATFORMS ?= linux/amd64 linux/arm64

LICENCE_ALLOWLIST := MIT,Apache-2.0,BSD-2-Clause,BSD-3-Clause,ISC,MPL-2.0

.PHONY: help
help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "%-16s %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the Nomad plugin
	cd nomad && go build -trimpath -o ../wherry-plugin-nomad .

.PHONY: check-fmt
check-fmt: ## Fail on Go files gofmt would change
	@out=$$(gofmt -l $$(git ls-files '*.go')); \
	if [ -n "$$out" ]; then echo "$$out"; echo "FAIL: gofmt"; exit 1; fi

.PHONY: vet
vet: ## Vet the Nomad plugin
	cd nomad && go vet ./...

.PHONY: test
test: ## Test the Nomad plugin
	cd nomad && go test ./...

$(GO_LICENSES):
	go install github.com/google/go-licenses@$(GO_LICENSES_VERSION)

$(CYCLONEDX):
	go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@$(CYCLONEDX_VERSION)

.PHONY: check-licenses
# A failure means a dependency changed licence on upgrade: investigate the
# module, do not widen the list to get green.
check-licenses: $(GO_LICENSES) ## Fail on dependency licences outside the allowlist
	cd nomad && $(GO_LICENSES) check ./... --allowed_licenses=$(LICENCE_ALLOWLIST)

.PHONY: check-markers
# wherry integrates agent harnesses by name, so product names are not
# markers here; attribution is. The patterns are assembled from fragments so
# this recipe never contains the strings it hunts and the Makefile is
# scanned like every other tracked file.
check-markers: ## Fail on tool-attribution markers in tracked files
	@p="co-auth""ored-by|gener""ated with|ai-gen""erated|claude\.ai/co""de|noreply@anthro""pic"; \
	if hits=$$(git grep -n -i -E "$$p" -- .); then \
		echo "$$hits"; echo "FAIL: tool-attribution markers in tracked files"; exit 1; \
	fi; \
	echo "no tool-attribution markers in tracked files"

.PHONY: check
check: check-fmt vet test check-licenses check-markers ## Run every CI gate

.PHONY: sbom
# app mode describes what the plugin binary links, not the module graph with
# its test-only dependencies. The tool reads tags through go-git, so run it
# from a plain clone with tags fetched.
sbom: $(CYCLONEDX) ## Write a CycloneDX SBOM for the Nomad plugin
	mkdir -p $(DIST)
	cd nomad && $(CYCLONEDX) app -json -licenses -main . -output ../$(DIST)/wherry-plugin-nomad.cdx.json .

.PHONY: dist
dist: ## Cross-compile release archives and checksums.txt into DIST
	rm -rf $(DIST)/wherry-plugin-nomad_*
	mkdir -p $(DIST)
	@set -e; for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; name=wherry-plugin-nomad_$(VERSION)_$${os}_$${arch}; \
		mkdir -p $(DIST)/$$name; \
		(cd nomad && CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -o ../$(DIST)/$$name/wherry-plugin-nomad .); \
		cp LICENSE nomad/README.md $(DIST)/$$name/; \
		tar -C $(DIST) -czf $(DIST)/$$name.tar.gz $$name; \
		rm -rf $(DIST)/$$name; \
	done
	cd $(DIST) && sha256sum wherry-plugin-nomad_*.tar.gz > checksums.txt
