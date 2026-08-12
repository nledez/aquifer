# Changelog

Newest first. Written by `just release` from the commit subjects
since the previous tag.

## 0.1.1 - 2026-08-12

- Empty commit
- chore: bootstrap the module, build tooling and license
- feat(fetch): share a single in-flight download between all requesters
- feat(manifest): read and write revision manifests, and window them
- feat(blobstore): address object storage by content, with an S3 backend
- feat(debian): parse Release, Packages and Sources indices
- feat(publish): publish revisions and collect what nothing references
- feat(cache): size-capped LRU with a pinned segment and glob selection
- feat(server): serve apt clients from the cache, tracking each repo's revision
- feat(cli): configuration, serve and ping
- build: multi-arch distroless image, plus a runnable deployment
- test: install real packages with real apt, and fix what that found
- docs(server): record why by-hash serves SHA256 only
- docs: the complete operator documentation
- feat(server): count and log every request that resolves to nothing
- build: pin every action and base image by digest, off the Node 20 runtime
- fix(test): hand the built repository back to the caller
- build(ci): lint with a golangci-lint that can read this go.mod
- fix: clear what the linter found once it could run
- fix(build): give the race detector the cgo it requires
- fix(notices): list the licenses of what ships, not of the host
- feat(ci): publish the image, and say where it lives
- docs: point every deployment at latest
- build: convert the Makefile to a Justfile
- feat(release): cut a release with one command, publish it from CI

