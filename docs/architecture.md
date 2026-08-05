# Architecture

## The problem

A single machine running aptly, rsync and nginx is a mirror's whole world: it
holds every byte, it is in the path of every request, and it absorbs every
spike alone. Aquifer splits that into a master nobody talks to and N edges that
hold almost nothing.

## The one idea

In a Debian repository, `pool/**` is immutable by construction: a package file
never changes, a new version is a new file. Only `dists/**` mutates.

Aquifer addresses **everything** by content:

```
s3://<bucket>/<prefix>/blobs/sha256/<ab>/<cd>/<full-hex-digest>   immutable, deduplicated
s3://<bucket>/<prefix>/manifests/<repo>/<revision>.tsv.zst        immutable
s3://<bucket>/<prefix>/refs/<repo>/current                        the only mutable object
```

A changed file yields a new digest, so a new blob, so a new manifest entry. The
superseded blob becomes unreferenced and falls out of the LRU on its own.

**Nothing is ever invalidated.** There is no purge, no TTL, no cache
invalidation anywhere in the codebase, and there is no place where adding one
would make sense. If you find yourself needing one, something has drifted.

## Data model

### Blobs

Content-addressed, sharded two levels deep by the first two bytes of the digest
so no single listing page or directory grows unmanageable. Two publications
that share a package share its blob, whatever path each serves it under.

### Manifests

One per revision per repo: a zstd-compressed TSV, sorted by path.

```
# format_version 1
# repo debian/bookworm
# revision 1785944988-7a64d09d
# created_at 2026-08-05T15:49:48Z
debian/bookworm/dists/bookworm/InRelease	250b42d7…	388
debian/bookworm/pool/main/n/nginx/nginx_1.24.0-1_amd64.deb	b7a491ec…	200000
```

At ~1000 entries, lookup is never the bottleneck, so the format was chosen for
everything else. It greps:

```sh
zstdcat manifest.tsv.zst | grep nginx
```

It diffs between two revisions, and it stays readable in ten years without a
tool. A database would charge a whole SQL engine for what is a
`map[string]entry` costing 200 KiB of heap.

Loading is all-or-nothing. An unknown `format_version`, a malformed record, an
uppercase or wrong-length digest, a duplicate path, or paths out of order all
reject the entire file. A half-accepted manifest would serve 404s for paths
that do exist, and it would do so on the edge, long after publication.

Manifests are deterministic: the same content produces the same bytes, so two
publications of an unchanged repository are indistinguishable.

### Revisions

`<unix-seconds>-<short-uuid>`, zero-padded, so revisions sort lexicographically
in the same order as chronologically. That is what lets the object store list
them in order with no index at all.

## Publishing

`aquifer publish` takes a directory produced by `aptly publish`.

1. Walk the tree, skipping `by-hash/` directories.
2. Parse each `dists/**/InRelease` (or `Release`) for the indices and their
   digests.
3. Parse one variant of each `Packages` and `Sources` for the `pool/` digests.
4. **Recompute nothing.** aptly already hashed every file, and those digests
   are exactly what the indices record. Only files no index mentions — an
   exported signing key, the Release files themselves — are read and hashed.
5. `ListObjectsV2` on `blobs/` to learn what is already there. At ~1000 objects
   that is one paginated request, cheaper than maintaining an index and
   impossible to desynchronise. **The master keeps no local state.**
6. Upload the missing blobs in parallel.
7. Upload `<revision>.tsv.zst`.
8. **Last, and only last**, write `refs/<repo>/current`.

Step 8 is the atomic commit. If the job dies before it, nothing has happened:
what it leaves behind is orphaned blobs, which the GC collects, and never a ref
pointing at a manifest that never landed.

Two consistency checks earn their keep without re-reading 17 GiB. A declared
size that disagrees with the file on disk means the index is stale. A file an
index promises but does not deliver would publish a manifest that 404s on the
edge. Both refuse to publish.

One exception is deliberate: `apt-ftparchive` lists the suite's own `Release`
inside the checksum sections it is generating, so that entry is stale by
construction — writing the digest into the file changes the file. apt ignores
it, and so does Aquifer. A per-component `main/binary-amd64/Release` is a real
index and is still checked.

## Serving

An edge polls `refs/<repo>/current` for every repo, every 15 seconds, with
`If-None-Match`. An unchanged ref costs one round trip and no transfer. On a
change it downloads the manifest and swaps an `atomic.Pointer`, so the serving
path takes no lock.

### Revision window

The K most recent revisions of each repo stay resolvable (5 by default):

- `pool/**` resolves against the **union** of retained revisions;
- `dists/**` resolves against the **current revision alone**.

The asymmetry is the point. A client that ran `apt update` against one edge and
`apt install` against another, mid-switchover, still finds its package; and a
stale index can never be served.

### Request routing

One publication is served at the archive root, so a prefix map would be
ambiguous. Prefixes are tried longest first, the root last. A repo that does
not hold the path yields a plain 404 rather than an ambiguous search through
the others.

### What never reaches object storage

- **HEAD** is answered from the manifest, which already states the size.
- **A matching `If-None-Match`** is a 304. Content addressing makes
  revalidation exact rather than a guess, and apt revalidates constantly.
- **A cache hit**, served with `http.ServeContent`, which gets `Range`,
  `Last-Modified` and `ETag` right for free.

### by-hash

`dists/**/by-hash/SHA256/<digest>` resolves straight from the digest in the
URL. The digest is still checked against the retained revisions, so the
endpoint cannot be used to make an edge pull arbitrary objects out of storage.

Only SHA256 is served, and every by-hash request that resolves to nothing is
counted and logged with a reason naming why. apt asks for the strongest digest
the `Release` declares, and both `apt-ftparchive` and aptly emit SHA512, so an
apt configured for by-hash gets a 404 and falls back to the plain path —
correctly, at the cost of one wasted round trip per index. That gap is deliberate: the access
logs of the mirror this replaces contain no by-hash request at all, so serving
SHA512 would mean carrying a second digest for every index and roughly 1200
extra manifest entries per revision for a path nobody takes. See
[operations.md](operations.md#if-you-ever-enable-acquire-by-hash) for what
would change the answer.

## Download coalescing

**An edge never downloads the same blob twice at once. At any instant, for a
given digest, there is at most one GET to object storage.**

This is the most important requirement in the system, and there is no way
around it: clients sit behind domain and IP filtering and can only reach the
edge. There is no redirect to object storage and no offload. Without
coalescing, forty VMs colliding on an uncached 90 MiB package cost 3.6 GiB of
egress instead of 90 MiB.

### One code path for every miss

```
requester 1 ──▶ leader ──▶ GET object storage ──▶ temp file ──┬──▶ sha256
                             │                                 └──▶ written++ (atomic)
requester 2 ──▶ follower ────┘   reads the same file, waits when it catches up
requester N ──▶ follower ────┘
                                          ▼
                          verify digest, fsync, then and only then:
                          rename into the cache, or unlink
```

1. The first requester becomes the **leader**: it opens the GET, writes to a
   temp file, publishes progress through an atomic counter, and hashes as it
   goes.
2. Later requesters become **followers**. They read the same temp file through
   a descriptor the entry holds, and wait on a broadcast channel when they
   catch up. A channel rather than a `sync.Cond`, because it composes with
   `ctx.Done()`.
3. On completion the digest is verified and the file is fsynced. **Only then**
   does admission decide: rename into the cache, or unlink. What the clients
   received is identical either way.
4. On any failure — network, checksum, full disk — every follower gets the same
   error and nothing enters the cache.

That single path is what makes an uncacheable blob coalesce like any other.
Without the temp file, followers would have nothing to follow.

### Non-negotiable

**The leader's download does not run on its request's context.** It runs under
`context.WithoutCancel`. Otherwise the first client to hit Ctrl-C kills the
download the other thirty-nine are following. This is the classic bug in this
pattern.

The consequence is that downloads outlive the requests that started them, so
shutdown drains them explicitly rather than leaving goroutines writing into a
cache directory the process is about to abandon.

A `Range` request on a blob still downloading waits for completion rather than
issuing a second, partial GET. It never bypasses the coalescer.

`aquifer_fetch_coalesced_readers_total` is the number that proves this works.

## Caching

Two independent glob lists, matched against the **full serving path**, repo
prefix included, so one publication's metadata can be pinned without pinning
another's. `**` is required, which is why matching uses `doublestar` rather
than `path/filepath`.

- **Pinned**: fetched on every revision load, never evicted, accounted
  separately. They do not consume `max_size`, so a flood of packages can never
  push metadata out.
- **Prefetched**: fetched in the background after a switch, then managed by the
  LRU like anything else. A pinned pattern is implicitly prefetched — pinning a
  blob that is only fetched on demand achieves nothing.
- Everything else is lazy.

A plan larger than `pinned_max_size` is refused whole rather than partially
applied, and the edge keeps serving the previous revision. That cap is what
makes a pattern like `**` fail fast and loudly instead of quietly filling the
disk.

Eviction never runs inside a request. Crossing 100% of the budget wakes a
background goroutine that evicts to 90%; evicting exactly to the budget would
make the next admission evict again, turning every request into an unlink of a
possibly 90 MiB file. Victims are chosen under the lock and removed from disk
outside it.

In-flight downloads are staged in a temp directory on the same filesystem, so
admission is a rename and a partial blob is never visible under its final name.
Those bytes sit outside `max_size` with their own reserve: twenty concurrent
90 MiB packages are 1.8 GiB in flight that must not provoke an eviction.

On startup the accounting is rebuilt from disk, ordered by modification time.
That ordering is approximate and only matters until traffic warms the real one
back up; an edge that forgot its cache would re-download everything.

## Garbage collection

`aquifer gc` is mark-and-sweep across all repos.

Retained: the last K revisions of each repo, **plus whatever revision the ref
points at**. An operator who rolled back does not get the live revision
collected from under them.

Manifests are deleted before blobs. Interrupted that way, the worst case is an
orphan; the other order would leave a manifest referencing a blob that is gone.
A manifest that cannot be read stops the run, since treating it as referencing
nothing would delete precisely the blobs it alone protects.

**The grace period is mandatory.** A publication uploads its blobs before it
writes its ref, so between those two moments its blobs look exactly like
orphans. Nothing younger than 24 hours is collected, whatever references it.

## What is deliberately absent

- **Authentication.** The server serves everyone. There is an `Authorizer`
  interface with an `AllowAll` default wired in as middleware, and nothing else.
- **TLS in the application.** The reverse proxy terminates it.
- **Redirects to object storage.** Excluded by the clients' network filtering.
- **Other formats.** Debian and Ubuntu only.
- **Peer-to-peer fetch between edges.**
- **Snapshot and rollback endpoints.** The data model stays compatible with
  them: a ref can be pointed at any retained revision by writing one object.
