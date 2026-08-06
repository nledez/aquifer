BINARY      := aquifer
PKG         := github.com/nledez/aquifer
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOFLAGS     := -trimpath
LDFLAGS     := -s -w -X $(PKG)/internal/cli.version=$(VERSION)
NOTICES     := THIRD-PARTY-NOTICES.txt

export CGO_ENABLED := 0

.PHONY: all
all: lint test build

.PHONY: build
build:
	go build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o dist/$(BINARY) ./cmd/$(BINARY)

.PHONY: test
test:
	go test ./...

# CGO_ENABLED is 0 for this whole file because the shipped binary is static.
# The race detector is the exception: it is built on TSAN, which is C, so on
# Linux the toolchain refuses -race outright without cgo. Every -race target
# below therefore re-enables it for that command only.
.PHONY: race
race:
	CGO_ENABLED=1 go test -race -count=1 ./...

# The coalescer is the heart of the project; hammer it harder than the rest.
.PHONY: stress
stress:
	CGO_ENABLED=1 go test -race -count=20 ./internal/fetch/...

# Runs the blobstore contract against a real MinIO in Docker. Set
# AQUIFER_TEST_S3_ENDPOINT to point at an existing store instead.
.PHONY: test-integration
test-integration:
	go test -tags=integration -count=1 -timeout=15m ./...

.PHONY: cover
cover:
	CGO_ENABLED=1 go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: vet
vet:
	go vet ./...

.PHONY: lint
lint:
	golangci-lint run

.PHONY: fmt
fmt:
	gofmt -l -w .

.PHONY: tidy
tidy:
	go mod tidy

$(NOTICES): go.mod go.sum
	go run ./internal/tools/notices -o $@ ./...

.PHONY: notices
notices:
	go run ./internal/tools/notices -o $(NOTICES) ./...

# Fails when a dependency change was not reflected in the notices file.
.PHONY: notices-check
notices-check: notices
	git diff --exit-code -- $(NOTICES)

.PHONY: clean
clean:
	rm -rf dist coverage.out

# --- container image ----------------------------------------------------------

IMAGE       ?= ghcr.io/nledez/aquifer
PLATFORMS   ?= linux/amd64,linux/arm64
VCS_REF     := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
# SOURCE_DATE_EPOCH keeps the build reproducible; override it to pin a date.
SOURCE_DATE_EPOCH ?= $(shell git log -1 --pretty=%ct 2>/dev/null || echo 0)
BUILD_DATE  := $(shell date -u -r $(SOURCE_DATE_EPOCH) +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
                || date -u -d @$(SOURCE_DATE_EPOCH) +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)

# Multi-arch builds and attestations need a docker-container driver; the
# default "docker" driver supports neither. "make buildx-setup" creates one.
BUILDX_BUILDER ?= aquifer
BUILDER_FLAG   := $(if $(BUILDX_BUILDER),--builder $(BUILDX_BUILDER),)

DOCKER_BUILD_ARGS = \
	--build-arg VERSION=$(VERSION) \
	--build-arg VCS_REF=$(VCS_REF) \
	--build-arg BUILD_DATE=$(BUILD_DATE)

.PHONY: buildx-setup
buildx-setup:
	docker buildx inspect $(BUILDX_BUILDER) >/dev/null 2>&1 \
		|| docker buildx create --name $(BUILDX_BUILDER) --driver docker-container --bootstrap

# Builds for the host architecture and loads the result into the local daemon.
.PHONY: image
image: $(NOTICES)
	docker buildx build $(DOCKER_BUILD_ARGS) \
		--provenance=false --sbom=false \
		-t $(IMAGE):$(VERSION) --load .

# Builds both architectures with an SBOM and provenance attestations. Multi-arch
# images cannot be loaded into the local daemon, so this pushes.
.PHONY: image-push
image-push: $(NOTICES) buildx-setup
	docker buildx build $(BUILDER_FLAG) $(DOCKER_BUILD_ARGS) \
		--platform=$(PLATFORMS) \
		--sbom=true --provenance=mode=max \
		-t $(IMAGE):$(VERSION) --push .

# Builds both architectures without pushing, to check the Dockerfile.
.PHONY: image-check
image-check: $(NOTICES) buildx-setup
	docker buildx build $(BUILDER_FLAG) $(DOCKER_BUILD_ARGS) \
		--platform=$(PLATFORMS) \
		--sbom=true --provenance=mode=max \
		--output=type=cacheonly .

# The only test that proves the whole system works: a real signed Debian
# repository, published, served by an edge, installed with a real apt, for
# every configured suite and architecture.
.PHONY: test-apt
test-apt:
	$(MAKE) image VERSION=apt-test IMAGE=aquifer
	AQUIFER_IMAGE=aquifer:apt-test ./test/apt/run.sh
