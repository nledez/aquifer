# Operations

## Publishing

Run `aquifer publish` on the master, once per publication, after `aptly
publish` has written its tree.

```sh
export AQUIFER_S3_ENDPOINT=https://s3.example.net
export AQUIFER_S3_BUCKET=aquifer
export AQUIFER_S3_PREFIX=mirror
export AQUIFER_S3_ACCESS_KEY=…
export AQUIFER_S3_SECRET_KEY=…

for suite in bookworm trixie; do
  aquifer publish --repo "debian/$suite" "/var/lib/aptly/public/$suite"
done
```

Each run prints one line:

```
debian/bookworm 1785951669-6272eef5: 794 entries, 17.4 GiB; uploaded 4 blobs (147.3 KiB), 790 already present, 2 hashed
```

- **uploaded** is what actually changed. On an unchanged publication it is 0.
- **already present** is deduplication doing its job.
- **hashed** counts files no index mentions — the `Release` files, an exported
  signing key. Everything else uses the digest aptly already computed.

A publication that fails leaves the ref untouched. Nothing has happened, and
the orphaned blobs are collected by the next GC. Rerunning is always safe.

### When publish refuses

```
publish: pool/main/n/nginx/nginx_1.24.0-1_amd64.deb: dists/bookworm/main/binary-amd64/Packages
declares 200000 bytes but the file holds 199872; the index is stale
```

The publication directory and its own indices disagree. Rerun `aptly publish`;
do not work around this. The alternative is a manifest that 404s on the edges
long after anyone could connect the two events.

```
publish: 3 file(s) referenced by an index are missing from disk, first: pool/main/…
```

Same cause, usually a publication interrupted part-way.

## Garbage collection

```sh
aquifer gc --keep 5 --dry-run     # always look first
aquifer gc --keep 5
```

```
1 repos, 812 blobs scanned, 794 referenced; deleted 18 blobs (412.7 MiB) and 2 manifests; 0 spared by the grace period
```

Run it on a schedule, daily is plenty. It is idempotent and interruptible: a
run that dies part-way leaves orphans, never a manifest referencing a blob that
is gone.

**Do not shorten `--grace` casually.** A publication uploads its blobs before
it writes its ref, so for the whole duration of an upload its blobs are
indistinguishable from orphans. The 24-hour default is what stops a GC from
destroying a publication that is still running. `--grace=0` is honoured
literally and warns:

```
level=WARN msg="grace period is shorter than the default; a publication in flight may lose its blobs" grace=0s default=24h0m0s
```

It is legitimate only when you know no publication is running — reclaiming
space during an incident, for instance.

`--keep` must be at least as large as the edges' `window`, or an edge can
resolve a revision whose blobs the GC has removed. Keep them equal, or keep
more on the master.

## Monitoring

### Scraping

`/metrics` is on the admin port. **It binds to loopback by default**, which
means Prometheus cannot reach it from another host or another container. Set
`admin_listen` (or `AQUIFER_ADMIN_LISTEN`) to `0.0.0.0:8081` where you want it
scrapable, and control access with the network rather than the bind address.

```yaml
scrape_configs:
  - job_name: aquifer
    metrics_path: /metrics
    static_configs:
      - targets:
          - edge-1.example.net:8081
          - edge-2.example.net:8081
          - edge-3.example.net:8081
    relabel_configs:
      - source_labels: [__address__]
        regex: '([^:]+):.*'
        target_label: instance
        replacement: '$1'
```

With Consul:

```yaml
scrape_configs:
  - job_name: aquifer
    consul_sd_configs:
      - server: consul.service.consul:8500
        services: [aquifer-admin]
```

### What to watch

| Metric | What it tells you |
|---|---|
| `aquifer_cache_requests_total{class,result}` | hit ratio, **split by class** |
| `aquifer_fetch_coalesced_readers_total` | downloads saved by coalescing |
| `aquifer_fetch_inflight` | downloads running now |
| `aquifer_cache_bytes`, `aquifer_cache_objects` | the evictable segment |
| `aquifer_cache_evictions_total` | eviction pressure |
| `aquifer_cache_pinned_bytes`, `aquifer_cache_pinned_objects` | pinned, resident |
| `aquifer_cache_pinned_planned_objects` | pinned, called for by the current revisions |
| `aquifer_cache_temp_bytes` | in-flight downloads on disk |
| `aquifer_manifest_revision_info{repo,revision}` | which revision each edge serves |
| `aquifer_manifest_age_seconds{repo}` | how old that revision is |
| `aquifer_release_valid_until_seconds{repo,suite}` | seconds before apt refuses the repo |
| `aquifer_request_duration_seconds{class}` | latency, by class |

**Always read the hit ratio by class.** An overall 85% can hide metadata at
100% and pool at 40%, and those two situations call for opposite responses:

```promql
sum by (class) (rate(aquifer_cache_requests_total{result="hit"}[1h]))
  / sum by (class) (rate(aquifer_cache_requests_total[1h]))
```

- `class="pinned"` below 100% means metadata is being fetched on demand. Your
  `pinned` patterns are missing something.
- `class="pool"` is expected to be well under 100% with a 5 GiB budget against
  an 8 GiB working set. Read it against `aquifer_cache_evictions_total`.

### Alerts worth having

```yaml
groups:
  - name: aquifer
    rules:
      - alert: AquiferEdgeDiverged
        expr: count(count by (revision) (aquifer_manifest_revision_info)) by (repo) > 1
        for: 10m
        annotations:
          summary: "Edges disagree on the revision of {{ $labels.repo }}"

      - alert: AquiferManifestStale
        expr: aquifer_manifest_age_seconds > 86400 * 2
        for: 30m
        annotations:
          summary: "{{ $labels.repo }} has not seen a new revision in two days"

      - alert: AquiferReleaseExpiringSoon
        expr: aquifer_release_valid_until_seconds < 86400
        for: 15m
        annotations:
          summary: "{{ $labels.repo }} {{ $labels.suite }} expires in under a day; apt will refuse it"

      - alert: AquiferMetadataMissing
        expr: aquifer_cache_pinned_objects < aquifer_cache_pinned_planned_objects
        for: 15m
        annotations:
          summary: "Pinned blobs are still missing well after a revision switch"

      - alert: AquiferErrors
        expr: rate(aquifer_cache_requests_total{result="error"}[15m]) > 0
        for: 10m
        annotations:
          summary: "The edge is failing to serve requests"
```

`aquifer_release_valid_until_seconds` reports `+Inf` for a suite whose
`Release` declares no expiry, which is what Debian stable does; the alert above
never fires for those. It is reported only for `Release` files the cache holds,
so it exists exactly when your `pinned` or `prefetch` patterns cover metadata.

## Health checks

- `/healthz` — the process is alive. Use it as a liveness probe.
- `/readyz` — every manifest loaded, every pinned blob on disk, no suite past
  its `Valid-Until`. Use it as a readiness probe and as the Consul check.

```sh
aquifer ping              # exit 0 or 1, silent on success
aquifer ping --verbose    # the whole document
```

```json
{
  "ready": true,
  "repos": [
    {
      "repo": "debian/bookworm",
      "revision": "1785951636-4e9a90d6",
      "age_seconds": 33.2,
      "valid_until": { "bookworm": "2026-08-12T17:40:31Z" }
    }
  ],
  "cache": {
    "bytes": 4294967296, "objects": 380,
    "pinned_bytes": 7130316, "pinned_objects": 312, "pinned_missing": 0
  }
}
```

On failure `ping` prints one line naming the condition, which is what you read
out of `docker inspect` at three in the morning:

```
aquifer: not ready: debian/bookworm suite bookworm is past its Valid-Until
aquifer: not ready: 12 pinned blob(s) not yet on disk
aquifer: http://127.0.0.1:8081 is not answering: dial tcp 127.0.0.1:8081: connect: connection refused
```

## When an edge diverges

`AquiferEdgeDiverged` means two edges report different revisions for the same
repo. Clients hitting different edges may see different indices, and `apt
update` on one followed by `apt install` on another is exactly the case the
revision window exists to survive — but only within `window` revisions.

1. **Confirm which revision each edge holds.**

   ```sh
   for e in edge-1 edge-2 edge-3; do
     printf '%s ' "$e"
     curl -s "http://$e:8081/metrics" | grep '^aquifer_manifest_revision_info'
   done
   ```

2. **Find the truth.** The ref in object storage is authoritative.

   ```sh
   aws s3 cp s3://aquifer/mirror/refs/debian/bookworm/current - --endpoint-url "$AQUIFER_S3_ENDPOINT"
   ```

3. **Read the lagging edge's logs.** A refresh that fails leaves the previous
   revision in place rather than going dark, and says so:

   ```
   level=ERROR msg="could not refresh a repo" repo=debian/bookworm error="…"
   ```

   Credentials, network reachability to the object store, and a manifest that
   will not parse are the three causes worth checking, in that order.

4. **Restarting is safe and cheap.** The cache survives a restart, and the
   accounting is rebuilt from disk, so a restart re-reads the ref without
   re-downloading anything.

An edge that never converges is not serving stale data silently:
`aquifer_manifest_age_seconds` climbs and
`aquifer_release_valid_until_seconds` falls, and `/readyz` fails once the suite
expires, which takes the edge out of the load balancer on its own.

## Sizing the cache

Do this from the metrics rather than from intuition.

**Is the budget too small?** Watch evictions against the pool hit ratio:

```promql
rate(aquifer_cache_evictions_total[1h])
```

Sustained eviction with a pool hit ratio below ~60% means the working set does
not fit. Raising `max_size` is the direct fix, and the working set of the
mirror this replaces is about 8 GiB, so 5 GiB is deliberately tight.

**Is the budget too large?** If `aquifer_cache_bytes` never approaches
`max_size` and `aquifer_cache_evictions_total` stays flat for a week, you are
reserving disk you do not use.

**Is metadata sized right?** `aquifer_cache_pinned_bytes` should be a few MiB
and stable. If it grows, a pinned pattern is catching packages:

```promql
aquifer_cache_pinned_bytes / aquifer_cache_pinned_objects
```

An average object above a megabyte or so means the patterns have escaped
`dists/`.

**Does the disk hold what you promised?** The volume needs
`max_size + pinned_max_size + temp_reserve`. Aquifer warns at startup when it
does not:

```
level=WARN msg="the cache volume is smaller than the configuration needs" available=6442450944 needed=9663676416
```

That is a warning rather than a refusal, because a volume can grow and an edge
that will not start is worse than one running tight.

## If you ever enable `acquire-by-hash`

Aquifer serves `dists/**/by-hash/SHA256/<digest>`. apt asks for the strongest
digest the `Release` declares, and both `apt-ftparchive` and aptly emit SHA512,
so an apt configured for by-hash takes a 404 and falls back to the plain path.
Acquisition still succeeds; it costs one wasted round trip per index per `apt
update`.

That gap is deliberate — the mirror this replaces logs no by-hash request at
all. If you publish with `aptly publish -acquire-by-hash` and want it to work
properly, the fix is in `publish`, not in the edge: the `Release` already
carries the SHA512 of every index, so publish can emit the by-hash paths as
ordinary manifest entries pointing at the same blob. That costs roughly 1200
extra manifest entries per revision and lets the edge's by-hash special case go
away entirely.

You will not see this in the metrics as it stands: a path that does not resolve
returns 404 without being counted. Check for it directly:

```sh
curl -s -o /dev/null -w '%{http_code}\n' \
  "http://edge-1:8080/debian/bookworm/dists/bookworm/main/binary-amd64/by-hash/SHA512/$(sha512sum Packages | cut -d' ' -f1)"
```

## Verification

After any change to a deployment, run this against an edge from a client
machine. It is the only check that exercises the whole path:

```sh
sudo tee /etc/apt/sources.list.d/aquifer.sources <<'EOF'
Types: deb
URIs: http://edge-1.example.net/debian/bookworm
Suites: bookworm
Components: main contrib
Signed-By: /usr/share/keyrings/your-archive-keyring.gpg
EOF

sudo apt update
sudo apt install --reinstall -y hello
hello
```

Then confirm the edge saw what you expect:

```sh
curl -s http://edge-1.example.net:8081/metrics \
  | grep -E '^aquifer_(cache_requests_total|fetch_coalesced_readers_total)'
```

A `Range` request, which apt uses to resume an interrupted download:

```sh
curl -s -D- -r 0-1023 -o /dev/null \
  http://edge-1.example.net/debian/bookworm/pool/main/h/hello/hello_2.10-3_amd64.deb \
  | grep -iE '^(HTTP|content-range)'
```

```
HTTP/1.1 206 Partial Content
Content-Range: bytes 0-1023/53100
```

And that revalidation is free:

```sh
etag=$(curl -sI http://edge-1.example.net/debian/bookworm/dists/bookworm/InRelease | awk -F'"' '/[Ee]tag/{print $2}')
curl -s -o /dev/null -w '%{http_code}\n' -H "If-None-Match: \"$etag\"" \
  http://edge-1.example.net/debian/bookworm/dists/bookworm/InRelease
```

```
304
```
