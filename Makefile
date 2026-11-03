GO_LICENSES_VERSION := v1.6.0
GO_LICENSES         := $(shell go env GOPATH)/bin/go-licenses

VERSION   ?= dev
PLUGINS   := nomad kubernetes systemd
# image builds the receiver image from this checkout and the wherry release WHERRY_VERSION, such as v0.4.0, with
# CONTAINER, podman or docker.
WHERRY_VERSION ?=
IMAGE          ?= wherry-receiver
CONTAINER      ?= podman

LICENCE_ALLOWLIST := MIT,Apache-2.0,BSD-2-Clause,BSD-3-Clause,ISC,MPL-2.0

.PHONY: help
help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "%-16s %s\n", $$1, $$2}'

.PHONY: build
# build honours an exported GOOS and GOARCH, such as GOOS=linux GOARCH=amd64 for a Linux receiver; systemd builds for
# Linux alone. BIN is the directory the binaries go to.
BIN ?= .
build: ## Build every plugin (GOOS, GOARCH, BIN)
	@set -e; for p in $(PLUGINS); do \
		goos=$$(go env GOOS); [ $$p = systemd ] && goos=linux; \
		(cd $$p && GOOS=$$goos go build -trimpath -o $(abspath $(BIN))/wherry-plugin-$$p .); \
	done

.PHONY: check-cross
# check-cross runs build for an OS and architecture other than the host's and checks each binary's, so a recipe that
# drops the exported GOOS or GOARCH fails here. GOHOSTOS and GOHOSTARCH ignore an exported GOOS and GOARCH.
check-cross: ## Fail on a cross build that ignores GOOS and GOARCH
	@set -e; out=$$(mktemp -d); trap 'rm -rf "$$out"' EXIT; \
	goos=linux; [ "$$(go env GOHOSTOS)" = linux ] && goos=darwin; \
	goarch=arm64; [ "$$(go env GOHOSTARCH)" = arm64 ] && goarch=amd64; \
	GOOS=$$goos GOARCH=$$goarch $(MAKE) --no-print-directory build BIN="$$out" PLUGINS="$(PLUGINS)"; \
	for p in $(PLUGINS); do \
		want=$$goos; [ $$p = systemd ] && want=linux; \
		got=$$(go version -m "$$out/wherry-plugin-$$p" | awk '$$2 ~ /^GO(OS|ARCH)=/ {printf "%s ", $$2}'); \
		if [ "$$got" != "GOARCH=$$goarch GOOS=$$want " ]; then echo "FAIL: wherry-plugin-$$p built as $$got, want GOOS=$$want GOARCH=$$goarch"; exit 1; fi; \
		echo "wherry-plugin-$$p: $$got"; \
	done

.PHONY: check-fmt
check-fmt: ## Fail on Go files gofmt would change
	@out=$$(gofmt -l $$(git ls-files '*.go')); \
	if [ -n "$$out" ]; then echo "$$out"; echo "FAIL: gofmt"; exit 1; fi

.PHONY: vet
# The plugins ship for Linux alone, and systemd builds nowhere else, so vet and the licence check read them as Linux.
vet: ## Vet every plugin
	@set -e; for p in $(PLUGINS); do (cd $$p && GOOS=linux go vet ./...); done

.PHONY: test
test: ## Test every plugin with the race detector; one with no package for this OS is skipped
	@set -e; for p in $(PLUGINS); do (cd $$p && if [ -n "$$(go list ./... 2>/dev/null)" ]; then go test -race ./...; else echo "$$p: skipped, no package for $$(go env GOOS)"; fi); done

$(GO_LICENSES):
	go install github.com/google/go-licenses@$(GO_LICENSES_VERSION)

.PHONY: check-licenses
# A failure means a dependency changed licence on upgrade: investigate the
# module, do not widen the list to get green.
check-licenses: $(GO_LICENSES) ## Fail on dependency licences outside the allowlist
	@set -e; for p in $(PLUGINS); do (cd $$p && GOOS=linux $(GO_LICENSES) check ./... --allowed_licenses=$(LICENCE_ALLOWLIST)); done

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

.PHONY: check-examples
check-examples: ## Fail on a receiver example whose drain_timeout is not 10s below its stop window, or a removed command
	@.github/check-examples.sh $(PLUGINS)

.PHONY: check
check: check-fmt vet test check-cross check-licenses check-markers check-examples ## Run every CI gate

.PHONY: image
image: ## Build the receiver image (VERSION, WHERRY_VERSION, CONTAINER)
	@test -n "$(WHERRY_VERSION)" || { echo "FAIL: set WHERRY_VERSION"; exit 1; }
	$(CONTAINER) build -f examples/kubernetes/Containerfile --build-arg VERSION=$(VERSION) --build-arg WHERRY_VERSION=$(WHERRY_VERSION) \
		-t $(IMAGE):$(VERSION) .
