# Configuration

Three layers, each overriding the one before: the **YAML file**, then
**environment variables**, then **flags**.

A misspelled key is an error, not a silently taken default. `aquifer serve`
reports everything wrong with a configuration at once, before it opens
anything, so a bad deployment fails at startup rather than on the first
request.

## A complete file

This is the whole reference. Nothing below is a fragment; the file works as
shown.

```yaml
# Client-facing address. Everything apt talks to.
listen: "0.0.0.0:8080"

# /metrics, /healthz and /readyz. A separate port so that none of them is
# reachable from the client-facing address.
#
# It binds to loopback by default, which means Prometheus cannot scrape it from
# outside the container. Set it to 0.0.0.0:8081 when you want that; see
# operations.md.
admin_listen: "127.0.0.1:8081"

log:
  format: json        # json or text. json is the default; production is where this runs.
  level: info         # debug, info, warn, error

s3:
  # A URL, or a bare host:port in which case TLS is assumed unless insecure is set.
  endpoint: https://s3.example.net
  bucket: aquifer
  # Key prefix inside the bucket. Two deployments can share a bucket.
  prefix: mirror
  region: ""          # only if the endpoint needs one
  # Bucket-in-path addressing. Most non-AWS endpoints, including Swift's S3
  # compatibility layer, need this.
  path_style: true
  insecure: false     # allow plain HTTP for a bare host:port endpoint
  # Credentials belong in the environment, not in a file that ends up in a git
  # repository. They are accepted here for completeness only.
  # access_key: ""
  # secret_key: ""

# How often each repo's ref is polled. The request carries If-None-Match, so an
# unchanged ref costs one round trip and no transfer.
poll_interval: 15s

# How many revisions of each repo stay resolvable. Pool paths resolve against
# all of them, metadata against the newest alone.
window: 5

# Parallel background downloads after a revision switch.
prefetch_concurrency: 4

cache:
  dir: /var/cache/aquifer

  # The LRU budget. Pinned entries and in-flight downloads are outside it.
  max_size: 5GiB

  # Refuse to start if the pinned set exceeds this.
  pinned_max_size: 1GiB

  # Disk headroom for in-flight downloads, outside max_size.
  temp_reserve: 3GiB

  pinned:
    - "**/dists/**"
    - "dists/**"

  prefetch:
    - "**/dists/**"
    - "dists/**"

# One entry per publication.
repos:
  - repo: debian/bookworm
    prefix: debian/bookworm
  - repo: debian/trixie
    prefix: debian/trixie
  - repo: ubuntu/noble
    prefix: ubuntu/noble
  # An empty prefix serves this publication at the archive root. The router
  # tries the longest prefix first, so this entry only catches what nothing
  # else claims.
  - repo: root
    prefix: ""
```

## Sizes and durations

Sizes accept `B`, `KiB`, `MiB`, `GiB`, `TiB` and their decimal counterparts
`KB`, `MB`, `GB`, `TB`. **A bare number is bytes.** Guessing a larger unit
would size a cache a million times wrong, in the direction that looks fine
until the disk fills.

Durations are Go duration strings: `15s`, `2m`, `1h30m`.

## `pinned` and `prefetch`

This is the main tuning lever, so it is worth understanding exactly.

### What the two lists mean

| | Fetched when | Evictable | Consumes `max_size` |
|---|---|---|---|
| **pinned** | every revision load | never | no |
| **prefetch** | in the background after a switch | yes | yes |
| neither | on the first request for it | yes | yes |

A pinned pattern is implicitly prefetched: pinning a blob that is only fetched
when someone asks for it achieves nothing.

### What the patterns match

The **full serving path**, repo prefix included, exactly as a client requests
it. Not the path inside the publication.

```
debian/bookworm/dists/bookworm/main/binary-amd64/Packages.gz
└─── prefix ───┘└──────────── path within the publication ────────────┘
```

So `**/dists/**` covers every publication's metadata, while
`debian/bookworm/dists/**` covers only that one. `dists/**` is needed as well
to cover a publication served at the root, since `**/dists/**` requires at
least one leading segment.

Syntax is glob with `**` support, via
[doublestar](https://github.com/bmatcuk/doublestar). `**` matches across path
separators; `*` does not.

### Why the default pins metadata

Metadata across all 24 publications is 6.8 MiB. Pinning it costs nothing and
means `apt update` — the operation every client performs, constantly — never
misses. Packages are 17.4 GiB against a 5 GiB budget and are left to the LRU.

### The guard rail

If the pinned set exceeds `pinned_max_size`, the edge **refuses to start**, and
a revision switch that would exceed it is refused whole while the edge keeps
serving the previous revision. That is what turns a pattern like `**` into an
immediate, explicit failure instead of a disk that fills overnight.

Every load logs what the patterns actually cover:

```
level=INFO msg="revision selection" repo=debian/bookworm revision=1785953930-81d26f60
  entries=794 pinned_objects=312 pinned_bytes=7130316 prefetch_objects=0 prefetch_bytes=0
```

A pattern that covers nothing is named:

```
level=WARN msg="cache pattern matches nothing in this revision"
  repo=debian/bookworm pattern="ubunutu/**"
```

That is almost always a typo in a repo prefix, and it fails silently otherwise.

### Worked examples

Pin metadata everywhere, and keep the packages an installer pulls warm:

```yaml
cache:
  pinned:
    - "**/dists/**"
    - "dists/**"
  prefetch:
    - "**/dists/**"
    - "dists/**"
    - "**/pool/main/l/linux-*/**"
    - "**/pool/main/s/systemd/**"
```

Pin one publication harder than the others, because its clients are latency
sensitive:

```yaml
cache:
  pinned:
    - "**/dists/**"
    - "dists/**"
    - "debian/bookworm/pool/main/**"
  pinned_max_size: 4GiB    # raised deliberately; the default 1GiB would refuse this
```

Serve one publication lazily and cache nothing ahead of time:

```yaml
cache:
  pinned: []
  prefetch: []
```

That last one is legal but costs you: every `apt update` becomes a fetch from
object storage, and `aquifer_release_valid_until_seconds` disappears, because
the metric is read from cached `Release` files and never triggers a download of
its own.

## Environment variables

| Variable | Overrides |
|---|---|
| `AQUIFER_CONFIG` | which file to read |
| `AQUIFER_LISTEN` | `listen` |
| `AQUIFER_ADMIN_LISTEN` | `admin_listen` |
| `AQUIFER_LOG_FORMAT` | `log.format` |
| `AQUIFER_LOG_LEVEL` | `log.level` |
| `AQUIFER_S3_ENDPOINT` | `s3.endpoint` |
| `AQUIFER_S3_BUCKET` | `s3.bucket` |
| `AQUIFER_S3_PREFIX` | `s3.prefix` |
| `AQUIFER_S3_REGION` | `s3.region` |
| `AQUIFER_S3_PATH_STYLE` | `s3.path_style` |
| `AQUIFER_S3_INSECURE` | `s3.insecure` |
| `AQUIFER_S3_ACCESS_KEY`, else `AWS_ACCESS_KEY_ID` | `s3.access_key` |
| `AQUIFER_S3_SECRET_KEY`, else `AWS_SECRET_ACCESS_KEY` | `s3.secret_key` |
| `AQUIFER_CACHE_DIR` | `cache.dir` |
| `AQUIFER_CACHE_MAX_SIZE` | `cache.max_size` |
| `AQUIFER_CACHE_PINNED_MAX_SIZE` | `cache.pinned_max_size` |
| `AQUIFER_CACHE_TEMP_RESERVE` | `cache.temp_reserve` |
| `AQUIFER_POLL_INTERVAL` | `poll_interval` |
| `AQUIFER_WINDOW` | `window` |
| `AQUIFER_PREFETCH_CONCURRENCY` | `prefetch_concurrency` |
| `AQUIFER_ADMIN_ADDR` | the address `aquifer ping` queries |

The standard AWS credential names are accepted so that existing tooling works
unchanged.

## Commands

### `aquifer serve`

```
--config        configuration file (env AQUIFER_CONFIG)
--listen        client-facing address
--admin-listen  address for /metrics, /healthz and /readyz
--cache-dir     cache directory
--log-format    json or text
--log-level     debug, info, warn or error
```

### `aquifer publish`

```
aquifer publish --repo debian/bookworm /var/lib/aptly/public/bookworm
```

```
--repo          repo name in object storage (required)
--prefix        serving path prefix; defaults to the repo name.
                Use --prefix=/ to serve this publication at the archive root.
--concurrency   parallel uploads (default: GOMAXPROCS, capped at 8)
--json          structured JSON logs
--endpoint --bucket --key-prefix --region --path-style --insecure
```

> **Two different prefixes.** `--prefix` is the *serving path* clients request
> under. `--key-prefix` is the *object storage* key prefix inside the bucket,
> the same value as `s3.prefix` in the edge's configuration. They are unrelated
> and both are easy to get wrong.

### `aquifer gc`

```
aquifer gc --keep 5 --dry-run
```

```
--keep      revisions to retain per repo (default: 5)
--grace     protect blobs written more recently than this (default: 24h)
--dry-run   report what would be deleted without deleting it
--json      structured JSON logs
--endpoint --bucket --key-prefix --region --path-style --insecure
```

`--grace=0` genuinely means no grace period and logs a warning. See
[operations.md](operations.md#garbage-collection) before using it.

### `aquifer ping`

```
--addr      admin address to query (env AQUIFER_ADMIN_ADDR, else the config file)
--config    configuration file to read the admin address from
--timeout   how long to wait before giving up (default: 2s)
--verbose   print the full /readyz document
```

Silent on success, exit 0. On failure it prints one line naming the condition
that failed, and exits 1. It needs no arguments: it resolves its target from
the flag, then `AQUIFER_ADMIN_ADDR`, then a readable configuration file, then
`http://127.0.0.1:8081`.

## Verification

Check a configuration without committing to it. A validation failure exits 2
and prints every problem at once:

```sh
aquifer serve --config /etc/aquifer/config.yaml --listen 127.0.0.1:18080 \
  --admin-listen 127.0.0.1:18081
```

```
aquifer serve: config: cache.max_size must be positive; s3.endpoint is required; s3.bucket is required; repos: prefix "debian/bookworm" is claimed by both "debian/bookworm" and "other"
```

A misspelled key is caught before any of that, and named:

```
aquifer serve: config: /etc/aquifer/config.yaml: yaml: unmarshal errors:
  line 3: field max_sise not found in type cli.CacheConfig
```

With a good file it prints one line and starts serving:

```
aquifer 0.1.0 serving 3 repo(s) on 127.0.0.1:18080 (admin on 127.0.0.1:18081)
```

Confirm the patterns cover what you meant, from the same instance:

```sh
curl -s http://127.0.0.1:18081/metrics | grep aquifer_cache_pinned
```

```
aquifer_cache_pinned_bytes 7130316
aquifer_cache_pinned_objects 312
aquifer_cache_pinned_planned_objects 312
```

`pinned_objects` reaching `pinned_planned_objects` means the prefetch has
finished. Then confirm a client is happy:

```sh
sudo tee /etc/apt/sources.list.d/aquifer.sources <<'EOF'
Types: deb
URIs: http://127.0.0.1:18080/debian/bookworm
Suites: bookworm
Components: main contrib
Signed-By: /usr/share/keyrings/your-archive-keyring.gpg
EOF

sudo apt update
apt policy | head
```
