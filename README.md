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

Requires Go 1.26+. `CGO_ENABLED=0` throughout; no dependency may need cgo.

```sh
make build            # static binary into dist/
make test             # unit tests
make race             # with the race detector
make stress           # hammer the download coalescer
make lint             # golangci-lint v2
make image            # container image for this architecture
```

Two heavier suites, both requiring Docker:

```sh
make test-integration # the blobstore contract against a real MinIO
make test-apt         # a real signed repository installed by a real apt
```

`make test-apt` is the one that proves the system works: it builds a signed
Debian repository with `dpkg-scanpackages` and `apt-ftparchive`, publishes it,
serves it from an edge running read-only as non-root, and installs a package
with a real apt — for every configured suite and architecture, checking that
the installed package actually runs.

## License

BSD 3-Clause. See [LICENSE](LICENSE).

Dependencies keep their own terms, reproduced in
[THIRD-PARTY-NOTICES.txt](THIRD-PARTY-NOTICES.txt). All of them are permissive
(MIT, Apache-2.0, BSD-2/3-Clause); none is copyleft.
