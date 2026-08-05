package blobstore

import (
	"strings"
)

// Object layout. Blobs are immutable and deduplicated by digest; manifests are
// immutable per revision; only the ref mutates, and it is written last, which
// makes it the atomic commit of a publication.
//
//	<prefix>/blobs/sha256/<ab>/<cd>/<hex>
//	<prefix>/manifests/<repo>/<revision>.tsv.zst
//	<prefix>/refs/<repo>/current
const (
	blobsDir     = "blobs/sha256"
	manifestsDir = "manifests"
	refsDir      = "refs"

	manifestSuffix = ".tsv.zst"
	refLeaf        = "current"

	sha256HexLen = 64
	shardLen     = 2
)

// join assembles an object key, tolerating a prefix with or without slashes.
func join(prefix string, parts ...string) string {
	clean := strings.Trim(prefix, "/")
	if clean != "" {
		parts = append([]string{clean}, parts...)
	}
	return strings.Join(parts, "/")
}

// BlobKey returns the object key of a blob. The two-level shard keeps any
// single listing page and any single directory, on backends that have them,
// down to a manageable size.
func BlobKey(prefix, hash string) string {
	return join(prefix, blobsDir, hash[:shardLen], hash[shardLen:2*shardLen], hash)
}

// BlobsPrefix returns the listing prefix covering every blob.
func BlobsPrefix(prefix string) string {
	return join(prefix, blobsDir) + "/"
}

// ManifestKey returns the object key of one revision's manifest.
func ManifestKey(prefix, repo, revision string) string {
	return join(prefix, manifestsDir, strings.Trim(repo, "/"), revision+manifestSuffix)
}

// ManifestsPrefix returns the listing prefix covering a repo's manifests.
func ManifestsPrefix(prefix, repo string) string {
	return join(prefix, manifestsDir, strings.Trim(repo, "/")) + "/"
}

// RefKey returns the object key of a repo's current revision pointer.
func RefKey(prefix, repo string) string {
	return join(prefix, refsDir, strings.Trim(repo, "/"), refLeaf)
}

// RefsPrefix returns the listing prefix covering every repo's ref.
func RefsPrefix(prefix string) string {
	return join(prefix, refsDir) + "/"
}

// HashFromBlobKey recovers the digest from a blob key, rejecting anything that
// is not exactly the layout BlobKey produces. Listings are the only inventory
// the master keeps, so a key that does not round-trip must not be mistaken for
// a blob: the GC would otherwise reason about objects it does not understand.
func HashFromBlobKey(prefix, key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, BlobsPrefix(prefix))
	if !ok {
		return "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return "", false
	}
	shardA, shardB, hash := parts[0], parts[1], parts[2]
	if !isSHA256Hex(hash) {
		return "", false
	}
	if shardA != hash[:shardLen] || shardB != hash[shardLen:2*shardLen] {
		return "", false
	}
	return hash, true
}

// RevisionFromManifestKey recovers a revision from a manifest key.
func RevisionFromManifestKey(prefix, repo, key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, ManifestsPrefix(prefix, repo))
	if !ok {
		return "", false
	}
	revision, ok := strings.CutSuffix(rest, manifestSuffix)
	if !ok || revision == "" || strings.Contains(revision, "/") {
		return "", false
	}
	return revision, true
}

// RepoFromRefKey recovers a repo name from a ref key. Repo names contain
// slashes, so the listing cannot use a delimiter and has to parse the leaf.
func RepoFromRefKey(prefix, key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, RefsPrefix(prefix))
	if !ok {
		return "", false
	}
	repo, ok := strings.CutSuffix(rest, "/"+refLeaf)
	if !ok || repo == "" {
		return "", false
	}
	return repo, true
}

func isSHA256Hex(s string) bool {
	if len(s) != sha256HexLen {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
