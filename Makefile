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

.PHONY: race
race:
	go test -race -count=1 ./...

# The coalescer is the heart of the project; hammer it harder than the rest.
.PHONY: stress
stress:
	go test -race -count=20 ./internal/fetch/...

# Runs the blobstore contract against a real MinIO in Docker. Set
# AQUIFER_TEST_S3_ENDPOINT to point at an existing store instead.
.PHONY: test-integration
test-integration:
	go test -tags=integration -count=1 -timeout=15m ./...

.PHONY: cover
cover:
	go test -race -coverprofile=coverage.out ./...
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
