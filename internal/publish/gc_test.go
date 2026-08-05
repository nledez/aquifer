package publish_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/nledez/aquifer/internal/blobstore"
	"github.com/nledez/aquifer/internal/blobstore/blobstoretest"
	"github.com/nledez/aquifer/internal/manifest"
	"github.com/nledez/aquifer/internal/publish"
)

// putBlob stores a body and returns its digest.
func putBlob(t *testing.T, store blobstore.Store, body []byte) string {
	t.Helper()
	hash := digest(body)
	if err := store.PutBlob(t.Context(), hash, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	return hash
}

// putRevision writes a manifest referencing the given bodies.
func putRevision(t *testing.T, store blobstore.Store, repo, revision string, hashes map[string]string) {
	t.Helper()

	entries := make([]manifest.Entry, 0, len(hashes))
	for path, hash := range hashes {
		entries = append(entries, manifest.Entry{Path: path, SHA256: hash, Size: 1})
	}
	var buf bytes.Buffer
	err := manifest.Write(&buf, manifest.Meta{
		Repo:      repo,
		Revision:  revision,
		CreatedAt: time.Unix(0, 0).UTC(),
	}, entries)
	if err != nil {
		t.Fatalf("manifest.Write: %v", err)
	}
	if err := store.PutManifest(t.Context(), repo, revision, bytes.NewReader(buf.Bytes()), int64(buf.Len())); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
}

func runGC(t *testing.T, store blobstore.Store, opts publish.GCOptions) *publish.GCResult {
	t.Helper()
	opts.Store = store
	res, err := publish.GC(t.Context(), opts)
	if err != nil {
		t.Fatalf("publish.GC: %v", err)
	}
	return res
}

// future puts the clock past the grace period so that every existing blob
// counts as old enough to collect.
func future() func() time.Time {
	return func() time.Time { return time.Now().Add(72 * time.Hour) }
}

func blobExists(t *testing.T, store blobstore.Store, hash string) bool {
	t.Helper()
	_, err := store.StatBlob(t.Context(), hash)
	if err == nil {
		return true
	}
	if errors.Is(err, blobstore.ErrNotFound) {
		return false
	}
	t.Fatalf("StatBlob: %v", err)
	return false
}

func TestGCDeletesUnreferencedBlobsAndKeepsReferencedOnes(t *testing.T) {
	t.Parallel()

	store := blobstoretest.NewMem()
	live := putBlob(t, store, []byte("still referenced"))
	orphan := putBlob(t, store, []byte("left over from an aborted publication"))

	putRevision(t, store, "debian/bookworm", "1754400000-aaaa", map[string]string{"a": live})
	if err := store.SetRef(t.Context(), "debian/bookworm", "1754400000-aaaa"); err != nil {
		t.Fatalf("SetRef: %v", err)
	}

	res := runGC(t, store, publish.GCOptions{Keep: 5, Grace: grace(24 * time.Hour), Now: future()})

	if !blobExists(t, store, live) {
		t.Fatal("a referenced blob was deleted")
	}
	if blobExists(t, store, orphan) {
		t.Fatal("an unreferenced blob survived")
	}
	if res.DeletedBlobs != 1 {
		t.Fatalf("DeletedBlobs = %d, want 1", res.DeletedBlobs)
	}
}

// SPEC section 4: a blob younger than the grace period may belong to a
// publication still in flight, whose ref has not landed yet.
func TestGCSparesBlobsInsideTheGracePeriod(t *testing.T) {
	t.Parallel()

	store := blobstoretest.NewMem()
	orphan := putBlob(t, store, []byte("uploaded seconds ago by a running publish"))
	putRevision(t, store, "debian/bookworm", "1754400000-aaaa", map[string]string{})
	if err := store.SetRef(t.Context(), "debian/bookworm", "1754400000-aaaa"); err != nil {
		t.Fatalf("SetRef: %v", err)
	}

	res := runGC(t, store, publish.GCOptions{Keep: 5, Grace: grace(24 * time.Hour)})

	if !blobExists(t, store, orphan) {
		t.Fatal("a blob inside the grace period was deleted")
	}
	if res.KeptYoung != 1 {
		t.Fatalf("KeptYoung = %d, want 1", res.KeptYoung)
	}
}

func TestGCRetainsTheLastKRevisionsAndDropsOlderManifests(t *testing.T) {
	t.Parallel()

	store := blobstoretest.NewMem()
	const repo = "debian/bookworm"

	blobs := map[string]string{}
	revisions := []string{"1754400000-aaaa", "1754400100-bbbb", "1754400200-cccc", "1754400300-dddd"}
	for _, rev := range revisions {
		blobs[rev] = putBlob(t, store, []byte("only in "+rev))
		putRevision(t, store, repo, rev, map[string]string{"pool/p_" + rev + ".deb": blobs[rev]})
	}
	if err := store.SetRef(t.Context(), repo, revisions[len(revisions)-1]); err != nil {
		t.Fatalf("SetRef: %v", err)
	}

	runGC(t, store, publish.GCOptions{Keep: 2, Grace: grace(24 * time.Hour), Now: future()})

	kept, err := store.ListManifests(t.Context(), repo)
	if err != nil {
		t.Fatalf("ListManifests: %v", err)
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d manifests, want 2", len(kept))
	}
	if kept[0].Revision != "1754400200-cccc" || kept[1].Revision != "1754400300-dddd" {
		t.Fatalf("kept the wrong revisions: %+v", kept)
	}

	for _, rev := range revisions[:2] {
		if blobExists(t, store, blobs[rev]) {
			t.Fatalf("a blob only reachable from dropped revision %s survived", rev)
		}
	}
	for _, rev := range revisions[2:] {
		if !blobExists(t, store, blobs[rev]) {
			t.Fatalf("a blob from retained revision %s was deleted", rev)
		}
	}
}

// Whatever the window says, the revision an edge is currently serving must
// survive; deleting it would break every running edge at once.
func TestGCAlwaysKeepsTheRevisionTheRefPointsAt(t *testing.T) {
	t.Parallel()

	store := blobstoretest.NewMem()
	const repo = "debian/bookworm"

	oldest := putBlob(t, store, []byte("referenced by the live revision"))
	putRevision(t, store, repo, "1754400000-aaaa", map[string]string{"a": oldest})
	for _, rev := range []string{"1754400100-bbbb", "1754400200-cccc", "1754400300-dddd"} {
		putRevision(t, store, repo, rev, map[string]string{})
	}
	// The ref deliberately lags behind: an operator rolled back.
	if err := store.SetRef(t.Context(), repo, "1754400000-aaaa"); err != nil {
		t.Fatalf("SetRef: %v", err)
	}

	runGC(t, store, publish.GCOptions{Keep: 2, Grace: grace(24 * time.Hour), Now: future()})

	if !blobExists(t, store, oldest) {
		t.Fatal("the blob of the live revision was deleted")
	}
	if _, err := store.GetManifest(t.Context(), repo, "1754400000-aaaa"); err != nil {
		t.Fatalf("the live revision's manifest was deleted: %v", err)
	}
}

func TestGCKeepsBlobsSharedWithAnotherRepo(t *testing.T) {
	t.Parallel()

	store := blobstoretest.NewMem()
	shared := putBlob(t, store, []byte("the same package in two publications"))

	putRevision(t, store, "debian/bookworm", "1754400000-aaaa", map[string]string{})
	putRevision(t, store, "ubuntu/noble", "1754400000-bbbb", map[string]string{"a": shared})
	for repo, rev := range map[string]string{
		"debian/bookworm": "1754400000-aaaa",
		"ubuntu/noble":    "1754400000-bbbb",
	} {
		if err := store.SetRef(t.Context(), repo, rev); err != nil {
			t.Fatalf("SetRef: %v", err)
		}
	}

	runGC(t, store, publish.GCOptions{Keep: 5, Grace: grace(24 * time.Hour), Now: future()})

	if !blobExists(t, store, shared) {
		t.Fatal("a blob referenced by another repo was deleted")
	}
}

func TestGCDryRunDeletesNothing(t *testing.T) {
	t.Parallel()

	store := blobstoretest.NewMem()
	orphan := putBlob(t, store, []byte("unreferenced"))
	putRevision(t, store, "debian/bookworm", "1754400000-aaaa", map[string]string{})
	if err := store.SetRef(t.Context(), "debian/bookworm", "1754400000-aaaa"); err != nil {
		t.Fatalf("SetRef: %v", err)
	}

	res := runGC(t, store, publish.GCOptions{
		Keep: 5, Grace: grace(24 * time.Hour), Now: future(), DryRun: true,
	})

	if !blobExists(t, store, orphan) {
		t.Fatal("a dry run deleted a blob")
	}
	if res.DeletedBlobs != 1 {
		t.Fatalf("DeletedBlobs = %d, want the count it would have deleted", res.DeletedBlobs)
	}
}

// A corrupt manifest must stop the run. Treating it as referencing nothing
// would delete every blob it alone protects.
func TestGCRefusesToRunAgainstACorruptManifest(t *testing.T) {
	t.Parallel()

	store := blobstoretest.NewMem()
	const repo = "debian/bookworm"
	body := []byte("this is not a manifest")
	if err := store.PutManifest(t.Context(), repo, "1754400000-aaaa",
		bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	if err := store.SetRef(t.Context(), repo, "1754400000-aaaa"); err != nil {
		t.Fatalf("SetRef: %v", err)
	}
	putBlob(t, store, []byte("would be deleted if the manifest were ignored"))

	if _, err := publish.GC(t.Context(), publish.GCOptions{
		Store: store, Keep: 5, Grace: grace(24 * time.Hour), Now: future(),
	}); err == nil {
		t.Fatal("GC ran despite being unable to read a retained manifest")
	}
}

// grace makes an explicit grace period addressable.
func grace(d time.Duration) *time.Duration { return &d }

// SPEC section 4 makes the grace period mandatory, so an omitted one must fall
// back to the default rather than to no protection at all.
func TestGCWithoutAnExplicitGraceUsesTheDefault(t *testing.T) {
	t.Parallel()

	store := blobstoretest.NewMem()
	orphan := putBlob(t, store, []byte("uploaded just now"))
	putRevision(t, store, "debian/bookworm", "1754400000-aaaa", map[string]string{})
	if err := store.SetRef(t.Context(), "debian/bookworm", "1754400000-aaaa"); err != nil {
		t.Fatalf("SetRef: %v", err)
	}

	res := runGC(t, store, publish.GCOptions{Keep: 5})

	if !blobExists(t, store, orphan) {
		t.Fatal("a zero-value Grace disabled the protection instead of defaulting to it")
	}
	if res.KeptYoung != 1 {
		t.Fatalf("KeptYoung = %d, want 1", res.KeptYoung)
	}
}

// An operator who asks for no grace period must get exactly that; silently
// substituting 24 hours would ignore an explicit instruction.
func TestGCHonoursAnExplicitZeroGrace(t *testing.T) {
	t.Parallel()

	store := blobstoretest.NewMem()
	orphan := putBlob(t, store, []byte("uploaded just now"))
	putRevision(t, store, "debian/bookworm", "1754400000-aaaa", map[string]string{})
	if err := store.SetRef(t.Context(), "debian/bookworm", "1754400000-aaaa"); err != nil {
		t.Fatalf("SetRef: %v", err)
	}

	res := runGC(t, store, publish.GCOptions{Keep: 5, Grace: grace(0)})

	if blobExists(t, store, orphan) {
		t.Fatal("an explicit zero grace period was overridden")
	}
	if res.KeptYoung != 0 {
		t.Fatalf("KeptYoung = %d, want 0", res.KeptYoung)
	}
	if res.DeletedBlobs != 1 {
		t.Fatalf("DeletedBlobs = %d, want 1", res.DeletedBlobs)
	}
}

func TestGCRejectsANegativeGrace(t *testing.T) {
	t.Parallel()

	store := blobstoretest.NewMem()
	if _, err := publish.GC(t.Context(), publish.GCOptions{
		Store: store, Grace: grace(-time.Hour),
	}); err == nil {
		t.Fatal("GC accepted a negative grace period")
	}
}
