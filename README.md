# Aquifer

A distributed APT mirror with a content-addressed cache.

Aquifer replaces a monolithic APT mirror with two roles:

- a private **master** that publishes `aptly` output to object storage, and is
  never in the serving path;
- N stateless **edges** that serve `apt` clients from a size-capped local disk
  cache, filled on demand from object storage.

Everything is addressed by content, so nothing is ever invalidated. A modified
file yields a new hash, a new blob, and a new manifest entry; the superseded
blob simply falls out of the LRU. There is no purge, no TTL, no invalidation
anywhere in the codebase.

A single static binary, `aquifer`, provides all subcommands: `serve`,
`publish`, `gc`, and `ping`.

## Status

Under construction. See `SPEC.md` for the full design.

## Development

```sh
make test      # unit tests
make race      # race detector
make stress    # hammer the download coalescer
make lint      # golangci-lint (v2 required)
make notices   # regenerate THIRD-PARTY-NOTICES.txt
make build     # static binary into dist/
```

Requires Go 1.26+ and `CGO_ENABLED=0`. No dependency may need cgo.

## License

BSD 3-Clause. See [LICENSE](LICENSE).

Third-party dependencies keep their own terms, reproduced in
[THIRD-PARTY-NOTICES.txt](THIRD-PARTY-NOTICES.txt). All of them are permissive
(MIT, Apache-2.0, BSD-2/3-Clause); none is copyleft.
