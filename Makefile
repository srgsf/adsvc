APP := adsvc

# ── Target platform (default: the host) ───────────────────────────────────────
HOST_OS   := $(shell uname -s | tr A-Z a-z)
HOST_ARCH := $(shell uname -m | sed -e s/x86_64/amd64/ -e s/aarch64/arm64/)
GOOS      ?= $(HOST_OS)
GOARCH    ?= $(HOST_ARCH)
GOARM     ?= 7
# Target names of the minimal ffmpeg: linux_arm64, linux_armv7, ...
ARCH_TAG   = $(GOARCH)$(if $(filter arm,$(GOARCH)),v$(GOARM))

# ── Container runtime: Apple `container` or Docker, whichever is on PATH ──────
DOCKER := $(shell command -v container 2>/dev/null || command -v docker 2>/dev/null)
CPUS   ?= $(shell sysctl -n hw.ncpu 2>/dev/null || nproc)
MEMORY ?= 8G
# Apple container VMs default to 1 GB, too little for ffmpeg, goreleaser and -race.
RESOURCES := $(if $(filter container,$(notdir $(DOCKER))),-c $(CPUS) -m $(MEMORY))

IMAGE_BUILDER := $(APP).builder
IMAGE_TEST    := $(APP).test
IMAGE         ?= $(APP)/$(APP)

CACHE_DIR      := $(CURDIR)/.cache
CACHE_GO_MOD   := $(CACHE_DIR)/go-mod
CACHE_GO_BUILD  = $(CACHE_DIR)/go-build-$(1)
# The images need no files from the checkout: build them from an empty context.
EMPTY_CONTEXT  := $(CACHE_DIR)/empty

# The checkout is mounted at its own path, so paths are the same inside and out. A git
# worktree keeps its repository elsewhere: mount that too, goreleaser needs git.
GIT_COMMON := $(abspath $(shell git rev-parse --git-common-dir 2>/dev/null))
GIT_MOUNT  := $(if $(filter $(CURDIR)/%,$(GIT_COMMON)),,-v $(GIT_COMMON):$(GIT_COMMON))

# $(call RUN,<image>,<platform>,<cache name>) runs a command in <image> as the current user.
RUN = $(DOCKER) run --rm --platform $(2) $(RESOURCES) \
	-v $(CURDIR):$(CURDIR) $(GIT_MOUNT) -w $(CURDIR) \
	-v $(CACHE_GO_MOD):/go/pkg/mod \
	-v $(call CACHE_GO_BUILD,$(3)):/go/cache \
	-u $(shell id -u):$(shell id -g) \
	-e HOME=/tmp -e GOPATH=/go -e GOCACHE=/go/cache -e GOFLAGS=-buildvcs=false \
	$(4) $(1)

# The builder (Dockerfile.builder) runs on the host platform.
BUILDER         = $(call RUN,$(IMAGE_BUILDER),linux/$(HOST_ARCH),$(HOST_ARCH),$(1))
GO_TARGET       = -e GOOS=$(GOOS) -e GOARCH=$(GOARCH) -e GOARM=$(GOARM)
GORELEASER      = $(call BUILDER,$(GO_TARGET))
# Tests run natively, with a full ffmpeg on PATH to generate media and act as the oracle.
TEST_RUN   = $(call RUN,$(IMAGE_TEST),linux/$(HOST_ARCH),test,$(1))

# Intermediate outputs live in the cache; dist/ gets only what a target is for. These
# paths are also in .goreleaser.yaml (dist, archive files) and scripts/build-ffmpeg.sh (OUT).
GORELEASER_DIST := $(CACHE_DIR)/goreleaser
FFMPEG_BIN      := $(CACHE_DIR)/ffmpeg/bin
EXE              = $(if $(filter windows,$(GOOS)),.exe)

# Minimal ffmpeg: the GOOS/GOARCH target by default (on a Mac: darwin_arm64), otherwise the
# Linux build for the same CPU. dist/ffmpeg-all build them all.
FFMPEG_ALL     := linux_amd64 linux_arm64 linux_armv7 darwin_arm64 windows_amd64
FFMPEG_TARGETS ?= $(or $(filter $(GOOS)_$(ARCH_TAG),$(FFMPEG_ALL)),linux_$(ARCH_TAG))

.DEFAULT_GOAL := build

# ── Images and caches ─────────────────────────────────────────────────────────

# One recipe for every image; each target sets which one (IMAGE_*: name, platform, file,
# stage). Images build from an empty context and only when missing.
setup-builder: IMAGE_NAME = $(IMAGE_BUILDER)
setup-builder: IMAGE_ARGS = -f Dockerfile.builder
setup-test:    IMAGE_NAME = $(IMAGE_TEST)
setup-test:    IMAGE_ARGS = -f Dockerfile.test
setup-builder setup-test: setup-cache
	@$(DOCKER) image inspect $(IMAGE_NAME) >/dev/null 2>&1 || \
		$(DOCKER) build --platform linux/$(HOST_ARCH) $(IMAGE_ARGS) $(RESOURCES) -t $(IMAGE_NAME) $(EMPTY_CONTEXT)

setup-cache:
	@mkdir -p $(CACHE_GO_MOD) $(EMPTY_CONTEXT) $(foreach c,$(HOST_ARCH) test,$(call CACHE_GO_BUILD,$(c)))

setup-dist:
	@mkdir -p dist

# ── Build ─────────────────────────────────────────────────────────────────────

# adsvc for GOOS/GOARCH[/GOARM] into dist/adsvc (dist/adsvc.exe for Windows), next to
# dist/ffmpeg from `make ffmpeg`, which the proxy then uses.
build: setup-builder adsvc-bin setup-dist
	cp $(GORELEASER_DIST)/$(APP)$(EXE) dist/$(APP)$(EXE)

# The minimal static ffmpeg for GOOS/GOARCH into dist/ffmpeg (dist/ffmpeg.exe for Windows).
ffmpeg: ffmpeg-bin setup-dist
	cp $(FFMPEG_BIN)/$(firstword $(FFMPEG_TARGETS))/ffmpeg$(EXE) dist/ffmpeg$(EXE)

# ffmpeg-all: every target's minimal ffmpeg into the cache (CI's release job runs goreleaser
# itself, on the runner). dist: release archives for every target into dist/, with checksums.txt. Each holds adsvc,
# the minimal ffmpeg with its license and config.yml.example.
dist ffmpeg-all: FFMPEG_TARGETS = $(FFMPEG_ALL)
ffmpeg-all: ffmpeg-bin
dist: setup-builder ffmpeg-bin setup-dist
	$(call BUILDER) release --clean --snapshot
	cp $(GORELEASER_DIST)/*.tar.gz $(GORELEASER_DIST)/*.zip $(GORELEASER_DIST)/checksums.txt dist/

# Local scratch image adsvc/adsvc for linux/GOARCH: adsvc + minimal ffmpeg. The context is
# staged like goreleaser's (<platform>/adsvc, .cache/ffmpeg/bin/...), so one Dockerfile
# serves both.
PLATFORM = linux/$(GOARCH)$(if $(filter arm,$(GOARCH)),/v$(GOARM))
CONTEXT  = $(CACHE_DIR)/image
docker: GOOS = linux
docker: setup-builder adsvc-bin ffmpeg-bin
	rm -rf $(CONTEXT)
	mkdir -p $(CONTEXT)/$(PLATFORM) $(CONTEXT)/.cache/ffmpeg/bin/linux_$(ARCH_TAG)
	cp $(GORELEASER_DIST)/$(APP) $(CONTEXT)/$(PLATFORM)/$(APP)
	cp $(FFMPEG_BIN)/linux_$(ARCH_TAG)/ffmpeg $(CONTEXT)/.cache/ffmpeg/bin/linux_$(ARCH_TAG)/
	$(DOCKER) build --platform $(PLATFORM) -f Dockerfile -t $(IMAGE):latest $(CONTEXT)

# Internal steps, into the cache. adsvc-bin: goreleaser's build of GOOS/GOARCH (the target
# that uses it sets up the builder image).
adsvc-bin: setup-cache
	$(GORELEASER) build --clean --snapshot --single-target

# ffmpeg-bin: FFMPEG_TARGETS in .cache/ffmpeg/bin/<os>_<arch>/ (the script skips what is
# built already).
ffmpeg-bin: setup-builder
	$(call BUILDER,--entrypoint bash) scripts/build-ffmpeg.sh $(FFMPEG_TARGETS)

# ── Checks ────────────────────────────────────────────────────────────────────

# Tests generate their own media with the full ffmpeg in the test image.
test: setup-test
	$(TEST_RUN) go test -race -coverprofile=coverage.out ./...

# The decode path against the minimal ffmpeg (TestDecodeFormats is its contract)
test-minimal: GOOS = linux
test-minimal: GOARCH = $(HOST_ARCH)
test-minimal: setup-test ffmpeg-bin
	$(call TEST_RUN,-e ADSVC_TEST_FFMPEG=$(FFMPEG_BIN)/linux_$(HOST_ARCH)/ffmpeg) go test -count=1 ./...

# The same on the host, against the host's minimal build (e.g. darwin_arm64): the one test
# that runs outside a container, since a macOS binary cannot run in a Linux one. Needs Go
# and a full ffmpeg/ffprobe on the host for the media and the oracle.
test-native: GOOS = $(HOST_OS)
test-native: GOARCH = $(HOST_ARCH)
test-native: ffmpeg-bin
	ADSVC_TEST_FFMPEG=$(FFMPEG_BIN)/$(HOST_OS)_$(HOST_ARCH)/ffmpeg go test -count=1 ./...

# gofmt only the module's packages: `gofmt -l .` would walk .cache (the module cache)
vet: setup-test
	$(TEST_RUN) sh -c 'go vet ./... && out=$$(gofmt -l $$(go list -f "{{.Dir}}" ./...)) && { test -z "$$out" || { echo "$$out"; exit 1; }; }'

lint: setup-cache
	$(call RUN,golangci/golangci-lint:latest,linux/$(HOST_ARCH),test,-e GOLANGCI_LINT_CACHE=/go/cache/lint) \
		golangci-lint run --max-issues-per-linter=0 --max-same-issues=0

# ── Federation testbed (docs/federation-testbed.md) ───────────────────────────

# Synthetic origins and five seeded nodes in .cache/fed. Flags of adsgen go in FED_ARGS,
# e.g. make fed-data FED_ARGS='-ads 3000 -hosts hub=192.168.1.10,mirror=192.168.1.11'
fed-data: setup-test
	$(TEST_RUN) go run ./internal/testdb/cmd/adsgen -o $(CACHE_DIR)/fed $(FED_ARGS)

# Start / stop / list the nodes with dist/adsvc (make build first); NODE=hub for one.
fed-up fed-down fed-status:
	scripts/fed.sh $(patsubst fed-%,%,$@) $(NODE)

release-check: setup-builder
	$(call BUILDER) check

# ── Housekeeping ──────────────────────────────────────────────────────────────

clean:
	rm -rf dist coverage.out $(GORELEASER_DIST) $(CONTEXT)

clean-cache:
	rm -rf $(CACHE_DIR)

# Drop the images so the next build recreates them (e.g. after editing Dockerfile.builder)
clean-images:
	-$(DOCKER) image rm $(IMAGE_BUILDER) $(IMAGE_TEST)

# The GitHub Pages site (site/, docs/usage.md, docs/handoff.md) into .cache/site, with pandoc
# in a container. Preview: python3 -m http.server -d .cache/site
PANDOC_IMAGE ?= docker.io/pandoc/core:3.6
site:
	$(DOCKER) run --rm $(RESOURCES) -v $(CURDIR):$(CURDIR) -w $(CURDIR) -u $(shell id -u):$(shell id -g) \
		-e SITE_REPO --entrypoint /bin/sh $(PANDOC_IMAGE) scripts/build-site.sh

help:
	@echo "Usage: make [target] [GOOS=.. GOARCH=.. GOARM=..]"
	@echo ""
	@echo "  build         dist/adsvc for the host or GOOS/GOARCH  (default)"
	@echo "  ffmpeg        dist/ffmpeg: minimal static ffmpeg for GOOS/GOARCH (now: $(FFMPEG_TARGETS))"
	@echo "  dist          dist/: release archives (adsvc, ffmpeg + license, config.yml.example) + checksums"
	@echo "  docker        local scratch image $(IMAGE):latest"
	@echo "  test          go test -race with the full ffmpeg"
	@echo "  test-minimal  go test with the decoder on the minimal ffmpeg (container)"
	@echo "  test-native   the same on the host with its own minimal ffmpeg (needs go, ffmpeg)"
	@echo "  fed-data      generate the federation testbed in .cache/fed (FED_ARGS=...)"
	@echo "  fed-up        start the testbed nodes (fed-down, fed-status; NODE=name for one)"
	@echo "  vet lint      go vet + gofmt; golangci-lint"
	@echo "  site          the GitHub Pages site in .cache/site (pandoc in a container)"
	@echo "  clean         remove dist/ and goreleaser's working files"
	@echo "  clean-cache   remove the Go, zig and ffmpeg caches (built ffmpegs too)"
	@echo "  clean-images  remove the builder and test images"

.PHONY: setup-builder setup-test setup-cache setup-dist build ffmpeg dist ffmpeg-all \
	docker adsvc-bin ffmpeg-bin \
	site fed-data fed-up fed-down fed-status test test-minimal test-native vet lint release-check clean clean-cache clean-images help
