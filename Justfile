# Aquifer's task runner. Run `just` for the list.

set shell := ["bash", "-euo", "pipefail", "-c"]

binary := "aquifer"
pkg := "github.com/nledez/aquifer"
notices_file := "THIRD-PARTY-NOTICES.txt"
version := env_var_or_default("VERSION", `git describe --tags --always --dirty 2>/dev/null || echo dev`)

# The shipped binary is static, so cgo is off everywhere. The race detector is
# the one exception - it is built on TSAN, which is C, and on Linux the
# toolchain refuses -race outright without cgo. Every -race recipe below
# re-enables it for that command alone.
export CGO_ENABLED := "0"

# --- development --------------------------------------------------------------

default:
    @just --list

# Lint, test, build - what a change should pass before it is pushed.
all: lint test build

# Static binary into dist/, stamped with the version.
build:
    go build -trimpath -ldflags='-s -w -X {{pkg}}/internal/cli.version={{version}}' \
        -o dist/{{binary}} ./cmd/{{binary}}

# Unit tests.
test:
    go test ./...

# Unit tests under the race detector.
race:
    CGO_ENABLED=1 go test -race -count=1 ./...

# The coalescer is the heart of the project; hammer it harder than the rest.
stress:
    CGO_ENABLED=1 go test -race -count=20 ./internal/fetch/...

# Set AQUIFER_TEST_S3_ENDPOINT to point at an existing store instead of
# starting one.
[doc("The blobstore contract against a real MinIO in Docker")]
test-integration:
    go test -tags=integration -count=1 -timeout=15m ./...

# Coverage across every package, reported as one number.
cover:
    CGO_ENABLED=1 go test -race -coverprofile=coverage.out ./...
    go tool cover -func=coverage.out | tail -1

vet:
    go vet ./...

# golangci-lint, the same version CI pins.
lint:
    golangci-lint run

fmt:
    gofmt -l -w .

tidy:
    go mod tidy

# Regenerates the third-party notices from the module cache.
notices:
    go run ./internal/tools/notices -o {{notices_file}} ./...

# Fails when a dependency change was not reflected in the notices file.
notices-check: notices
    git diff --exit-code -- {{notices_file}}

clean:
    rm -rf dist coverage.out

# --- container image ----------------------------------------------------------

image_repo := env_var_or_default("IMAGE", "ghcr.io/nledez/aquifer")
platforms := env_var_or_default("PLATFORMS", "linux/amd64,linux/arm64")
vcs_ref := `git rev-parse --short HEAD 2>/dev/null || echo unknown`
# SOURCE_DATE_EPOCH keeps the build reproducible; override it to pin a date.
build_date := ```
    sde="${SOURCE_DATE_EPOCH:-$(git log -1 --pretty=%ct 2>/dev/null || echo 0)}"
    date -u -r "$sde" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
        || date -u -d "@$sde" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
        || echo unknown
```
build_metadata := "--build-arg VCS_REF=" + vcs_ref + " --build-arg BUILD_DATE=" + build_date

buildx_builder := env_var_or_default("BUILDX_BUILDER", "aquifer")

# Multi-arch builds and attestations need a docker-container driver; the
# default "docker" driver supports neither.
[doc("Creates the buildx builder the multi-arch recipes need")]
buildx-setup:
    docker buildx inspect {{buildx_builder}} >/dev/null 2>&1 \
        || docker buildx create --name {{buildx_builder}} --driver docker-container --bootstrap

# Builds for the host architecture and loads the result into the local daemon.
image tag=version repo=image_repo: notices
    docker buildx build {{build_metadata}} --build-arg VERSION={{tag}} \
        --provenance=false --sbom=false \
        -t {{repo}}:{{tag}} --load .

# Multi-arch images cannot be loaded into the local daemon, so this pushes. CI
# does it on every push; this recipe is the manual fallback, and it needs a
# docker login first.
[doc("Builds both architectures with an SBOM and pushes them")]
image-push tag=version repo=image_repo: notices buildx-setup
    docker buildx build --builder {{buildx_builder}} \
        {{build_metadata}} --build-arg VERSION={{tag}} \
        --platform={{platforms}} \
        --sbom=true --provenance=mode=max \
        -t {{repo}}:{{tag}} --push .

# Builds both architectures without pushing, to check the Dockerfile.
image-check: notices buildx-setup
    docker buildx build --builder {{buildx_builder}} \
        {{build_metadata}} --build-arg VERSION={{version}} \
        --platform={{platforms}} \
        --sbom=true --provenance=mode=max \
        --output=type=cacheonly .

# The only test that proves the whole system works, for every configured suite
# and architecture.
[doc("A real signed repository, published, served, installed by a real apt")]
test-apt: (image "apt-test" "aquifer")
    AQUIFER_IMAGE=aquifer:apt-test ./test/apt/run.sh
