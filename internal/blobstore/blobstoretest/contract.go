// Package blobstoretest provides an in-memory blob store and the contract
// every blobstore.Store implementation must satisfy.
//
// The contract lives here rather than in a _test.go file so that both the
// in-memory store and the S3 store can be held to exactly the same behaviour,
// the latter under the "integration" build tag against a real MinIO.
package blobstoretest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/nledez/aquifer/internal/blobstore"
)

// Hash returns the digest of body, the way a blob is addressed.
func Hash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// RunStoreContract exercises the behaviour every Store must provide. newStore
// must return an empty store.
func RunStoreContract(t *testing.T, newStore func(t *testing.T) blobstore.Store) {
	t.Helper()

	t.Run("blob round trip", func(t *testing.T) {
		s := newStore(t)
		ctx := t.Context()
		body := []byte("a package payload")
		hash := Hash(body)

		if err := s.PutBlob(ctx, hash, bytes.NewReader(body), int64(len(body))); err != nil {
			t.Fatalf("PutBlob: %v", err)
		}

		rc, err := s.GetBlob(ctx, hash)
		if err != nil {
			t.Fatalf("GetBlob: %v", err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read blob: %v", err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("blob content = %q, want %q", got, body)
		}

		info, err := s.StatBlob(ctx, hash)
		if err != nil {
			t.Fatalf("StatBlob: %v", err)
		}
		if info.Hash != hash || info.Size != int64(len(body)) {
			t.Fatalf("StatBlob = %+v, want hash %s size %d", info, hash, len(body))
		}
		if info.LastModified.IsZero() {
			t.Fatal("StatBlob returned a zero LastModified; the GC grace period depends on it")
		}
	})

	t.Run("missing blob reports ErrNotFound", func(t *testing.T) {
		s := newStore(t)
		absent := Hash([]byte("never stored"))

		if _, err := s.GetBlob(t.Context(), absent); !errors.Is(err, blobstore.ErrNotFound) {
			t.Fatalf("GetBlob: got %v, want ErrNotFound", err)
		}
		if _, err := s.StatBlob(t.Context(), absent); !errors.Is(err, blobstore.ErrNotFound) {
			t.Fatalf("StatBlob: got %v, want ErrNotFound", err)
		}
	})

	t.Run("put blob is idempotent", func(t *testing.T) {
		s := newStore(t)
		ctx := t.Context()
		body := []byte("same bytes twice")
		hash := Hash(body)

		for i := range 2 {
			if err := s.PutBlob(ctx, hash, bytes.NewReader(body), int64(len(body))); err != nil {
				t.Fatalf("PutBlob %d: %v", i, err)
			}
		}
		blobs, err := s.ListBlobs(ctx)
		if err != nil {
			t.Fatalf("ListBlobs: %v", err)
		}
		if len(blobs) != 1 {
			t.Fatalf("ListBlobs returned %d blobs, want 1", len(blobs))
		}
	})

	t.Run("list and delete blobs", func(t *testing.T) {
		s := newStore(t)
		ctx := t.Context()

		want := map[string]int64{}
		for _, body := range [][]byte{[]byte("one"), []byte("two"), []byte("three")} {
			hash := Hash(body)
			want[hash] = int64(len(body))
			if err := s.PutBlob(ctx, hash, bytes.NewReader(body), int64(len(body))); err != nil {
				t.Fatalf("PutBlob: %v", err)
			}
		}

		blobs, err := s.ListBlobs(ctx)
		if err != nil {
			t.Fatalf("ListBlobs: %v", err)
		}
		if len(blobs) != len(want) {
			t.Fatalf("ListBlobs returned %d blobs, want %d", len(blobs), len(want))
		}
		for _, info := range blobs {
			size, ok := want[info.Hash]
			if !ok {
				t.Fatalf("ListBlobs returned an unexpected hash %s", info.Hash)
			}
			if info.Size != size {
				t.Fatalf("blob %s size = %d, want %d", info.Hash, info.Size, size)
			}
		}

		victim := Hash([]byte("two"))
		if err := s.DeleteBlob(ctx, victim); err != nil {
			t.Fatalf("DeleteBlob: %v", err)
		}
		if _, err := s.GetBlob(ctx, victim); !errors.Is(err, blobstore.ErrNotFound) {
			t.Fatalf("GetBlob after delete: got %v, want ErrNotFound", err)
		}
		// Deleting what is already gone is not an error: the GC must be
		// restartable without bookkeeping.
		if err := s.DeleteBlob(ctx, victim); err != nil {
			t.Fatalf("second DeleteBlob: %v", err)
		}
	})

	t.Run("manifest round trip", func(t *testing.T) {
		s := newStore(t)
		ctx := t.Context()
		const repo = "debian/bookworm"
		body := []byte("pretend this is zstd")

		if err := s.PutManifest(ctx, repo, "1754400000-aaaa", bytes.NewReader(body), int64(len(body))); err != nil {
			t.Fatalf("PutManifest: %v", err)
		}
		rc, err := s.GetManifest(ctx, repo, "1754400000-aaaa")
		if err != nil {
			t.Fatalf("GetManifest: %v", err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("manifest content = %q, want %q", got, body)
		}

		if _, err := s.GetManifest(ctx, repo, "1754400000-zzzz"); !errors.Is(err, blobstore.ErrNotFound) {
			t.Fatalf("GetManifest for an absent revision: got %v, want ErrNotFound", err)
		}
	})

	t.Run("manifests list in revision order", func(t *testing.T) {
		s := newStore(t)
		ctx := t.Context()
		const repo = "debian/bookworm"

		// Push them out of order; revisions sort lexicographically by time.
		for _, rev := range []string{"1754400200-cccc", "1754400000-aaaa", "1754400100-bbbb"} {
			if err := s.PutManifest(ctx, repo, rev, bytes.NewReader([]byte(rev)), int64(len(rev))); err != nil {
				t.Fatalf("PutManifest: %v", err)
			}
		}
		// A second repo must not leak into the first one's listing.
		if err := s.PutManifest(ctx, "ubuntu/noble", "1754400300-dddd", bytes.NewReader([]byte("x")), 1); err != nil {
			t.Fatalf("PutManifest: %v", err)
		}

		infos, err := s.ListManifests(ctx, repo)
		if err != nil {
			t.Fatalf("ListManifests: %v", err)
		}
		got := make([]string, len(infos))
		for i, info := range infos {
			got[i] = info.Revision
		}
		want := []string{"1754400000-aaaa", "1754400100-bbbb", "1754400200-cccc"}
		if !slices.Equal(got, want) {
			t.Fatalf("ListManifests = %v, want %v", got, want)
		}

		if delErr := s.DeleteManifest(ctx, repo, "1754400000-aaaa"); delErr != nil {
			t.Fatalf("DeleteManifest: %v", delErr)
		}
		infos, err = s.ListManifests(ctx, repo)
		if err != nil {
			t.Fatalf("ListManifests after delete: %v", err)
		}
		if len(infos) != 2 {
			t.Fatalf("ListManifests returned %d revisions after delete, want 2", len(infos))
		}
	})

	t.Run("ref publishes and reports change through its etag", func(t *testing.T) {
		s := newStore(t)
		ctx := t.Context()
		const repo = "debian/bookworm"

		if _, err := s.GetRef(ctx, repo, ""); !errors.Is(err, blobstore.ErrNotFound) {
			t.Fatalf("GetRef before publish: got %v, want ErrNotFound", err)
		}

		if err := s.SetRef(ctx, repo, "1754400000-aaaa"); err != nil {
			t.Fatalf("SetRef: %v", err)
		}
		ref, err := s.GetRef(ctx, repo, "")
		if err != nil {
			t.Fatalf("GetRef: %v", err)
		}
		if ref.Revision != "1754400000-aaaa" {
			t.Fatalf("ref revision = %q, want 1754400000-aaaa", ref.Revision)
		}
		if ref.ETag == "" {
			t.Fatal("ref has no ETag; the 15s poll depends on If-None-Match")
		}

		// Polling with the etag we already hold must not re-download the ref.
		if _, pollErr := s.GetRef(ctx, repo, ref.ETag); !errors.Is(pollErr, blobstore.ErrNotModified) {
			t.Fatalf("GetRef with a matching etag: got %v, want ErrNotModified", pollErr)
		}

		if setErr := s.SetRef(ctx, repo, "1754400100-bbbb"); setErr != nil {
			t.Fatalf("SetRef: %v", setErr)
		}
		updated, err := s.GetRef(ctx, repo, ref.ETag)
		if err != nil {
			t.Fatalf("GetRef after update: %v", err)
		}
		if updated.Revision != "1754400100-bbbb" {
			t.Fatalf("ref revision = %q, want 1754400100-bbbb", updated.Revision)
		}
		if updated.ETag == ref.ETag {
			t.Fatal("the etag did not change when the ref did")
		}
	})

	t.Run("repos are discovered from refs", func(t *testing.T) {
		s := newStore(t)
		ctx := t.Context()

		want := []string{"debian/bookworm", "salt", "ubuntu/noble"}
		for _, repo := range want {
			if err := s.SetRef(ctx, repo, "1754400000-aaaa"); err != nil {
				t.Fatalf("SetRef(%q): %v", repo, err)
			}
		}
		got, err := s.ListRepos(ctx)
		if err != nil {
			t.Fatalf("ListRepos: %v", err)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Fatalf("ListRepos = %v, want %v", got, want)
		}
	})

	t.Run("a cancelled context is honoured", func(t *testing.T) {
		s := newStore(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		if _, err := s.ListBlobs(ctx); err == nil {
			t.Fatal("ListBlobs ignored a cancelled context")
		}
	})
}
