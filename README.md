# Aquifer

A distributed APT mirror with a content-addressed cache.

[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD--3--Clause-blue.svg)](LICENSE)

## What it is

A single machine running aptly, rsync and nginx holds every byte of a mirror,
sits in the path of every request, and absorbs every spike alone. Aquifer
splits that in two:

- a private **master** with no inbound traffic, which publishes to object
  storage and is never in the serving path;
- N stateless **edges**, which serve apt clients from a size-capped local disk
  cache filled on demand from that object storage.

One static binary, `aquifer`, with subcommands: `serve`, `publish`, `gc`,
`ping`.

## Why it works

In a Debian repository `pool/**` is immutable by construction; only `dists/**`
changes. Aquifer addresses **everything** by content, so a modified file yields
a new digest, a new blob, and a new manifest entry. The superseded blob becomes
unreferenced and falls out of the LRU on its own.

**Nothing is ever invalidated.** There is no purge, no TTL and no cache
invalidation anywhere in the codebase.

That single decision buys three things:

- **Disk.** An edge holds a 5 GiB cache instead of a 17.4 GiB mirror.
- **No single point of failure.** The master can be down for a week; the edges
  keep serving.
- **Spike absorption.** N edges, and an edge never downloads the same blob
  twice at once. Forty machines colliding on one uncached 90 MiB package cost
  90 MiB of egress, not 3.6 GiB.

## Container image

Published to
[`ghcr.io/nledez/aquifer`](https://github.com/nledez/aquifer/pkgs/container/aquifer)
for `linux/amd64` and `linux/arm64`, on a distroless base, running as uid 65532
with no shell.

```sh
docker pull ghcr.io/nledez/aquifer:latest
docker run --rm ghcr.io/nledez/aquifer:latest version
```

| tag | what it is |
|---|---|
| `latest` | the newest release; what the guides here use |
| `X.Y.Z`, `X.Y` | a specific release, built from the `vX.Y.Z` git tag |
| `edge` | the current `main`; not a release |

CI publishes a tag only after the unit tests, the MinIO integration suite, the
apt end-to-end test, the linter and the cross-architecture builds have all
passed, so a tag that exists is a tag that was tested. Each one carries an SBOM
and a `mode=max` provenance attestation, both visible on the package page.

Pin by digest for anything you depend on — a tag is mutable, a digest is not:

```sh
docker pull ghcr.io/nledez/aquifer@sha256:<digest from the package page>
```

The image expects its configuration at `/etc/aquifer/config.yaml`, keeps its
cache in `/var/cache/aquifer`, and exposes 8080 for apt clients and 8081 for
metrics and health. See [configuration.md](docs/configuration.md).

## Quick start

MinIO stands in for the object store; one edge sits in front of it.

```sh
docker compose -f deploy/docker-compose.yml up -d
```

Publish a publication that `aptly publish` produced:

```sh
export AQUIFER_S3_ENDPOINT=http://127.0.0.1:9000
export AQUIFER_S3_BUCKET=aquifer
export AQUIFER_S3_PREFIX=mirror
export AQUIFER_S3_INSECURE=1
export AQUIFER_S3_ACCESS_KEY=aquifer
export AQUIFER_S3_SECRET_KEY=aquifer-secret

aquifer publish --repo debian/bookworm /var/lib/aptly/public/bookworm
```

```
debian/bookworm 1785951669-6272eef5: 794 entries, 17.4 GiB; uploaded 794 blobs (17.4 GiB), 0 already present, 2 hashed
```

Point a client at the edge:

```sh
sudo tee /etc/apt/sources.list.d/aquifer.sources <<'EOF'
Types: deb
URIs: http://127.0.0.1:8080/debian/bookworm
Suites: bookworm
Components: main contrib
Signed-By: /usr/share/keyrings/your-archive-keyring.gpg
EOF

sudo apt update
sudo apt install hello && hello
```

Then look at what the edge did:

```sh
curl -s http://127.0.0.1:8081/metrics | grep -E '^aquifer_(cache_requests_total|fetch_coalesced)'
```

Republishing unchanged content uploads nothing:

```sh
aquifer publish --repo debian/bookworm /var/lib/aptly/public/bookworm
```

```
debian/bookworm 1785951702-9c1ad3f1: 794 entries, 17.4 GiB; uploaded 0 blobs (0 B), 794 already present, 2 hashed
```

## Documentation

| | |
|---|---|
| [architecture.md](docs/architecture.md) | data model, revisions, coalescing, caching, GC |
| [configuration.md](docs/configuration.md) | the complete YAML reference, and the `pinned` / `prefetch` patterns |
| [operations.md](docs/operations.md) | publishing, GC, monitoring, a diverged edge, sizing the cache |
| [deploy-nginx.md](docs/deploy-nginx.md) | nginx in front, and why not to enable `proxy_cache` |
| [deploy-caddy.md](docs/deploy-caddy.md) | Caddy with automatic TLS |
| [deploy-nomad.md](docs/deploy-nomad.md) | a complete Nomad job with Consul, Traefik and Vault |

## Building

Requires Go 1.26+ and [just](https://github.com/casey/just) 1.27+, which is
where the `[doc]` attribute the recipes use arrived. CI pins 1.58.0.
`CGO_ENABLED=0` throughout; no dependency may need cgo.

Tasks run through just; `just` on its own lists them.

```sh
just build            # static binary into dist/
just test             # unit tests
just race             # with the race detector
just stress           # hammer the download coalescer
just lint             # golangci-lint v2
just image            # container image for this architecture
```

Two heavier suites, both requiring Docker:

```sh
just test-integration # the blobstore contract against a real MinIO
just test-apt         # a real signed repository installed by a real apt
```

`just test-apt` is the one that proves the system works: it builds a signed
Debian repository with `dpkg-scanpackages` and `apt-ftparchive`, publishes it,
serves it from an edge running read-only as non-root, and installs a package
with a real apt — for every configured suite and architecture, checking that
the installed package actually runs.

## Releasing

```sh
just release 0.2.0
```

It writes the changelog entry from the commits since the last tag, commits it,
tags `v0.2.0` and pushes both. It refuses to run from a dirty tree, from
anywhere but `main`, when `main` and `origin/main` have diverged, or when
`THIRD-PARTY-NOTICES.txt` no longer matches the dependencies — the archives
ship that file, so a stale one would state licences the binary beside it does
not have. A refusal on that last count leaves the regenerated file in the tree,
ready to commit.

Pushing the tag is what publishes. CI runs the whole suite on the tagged
commit, then pushes the image and creates the GitHub Release with the four
binaries and their checksums. A tag whose build fails publishes nothing.
`just image-push` remains the manual fallback for the image alone, and needs a
`docker login ghcr.io` first.

## License

BSD 3-Clause. See [LICENSE](LICENSE).

Dependencies keep their own terms, reproduced in
[THIRD-PARTY-NOTICES.txt](THIRD-PARTY-NOTICES.txt). All of them are permissive
(MIT, Apache-2.0, BSD-2/3-Clause); none is copyleft.
