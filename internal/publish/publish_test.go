package publish_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nledez/aquifer/internal/blobstore"
	"github.com/nledez/aquifer/internal/blobstore/blobstoretest"
	"github.com/nledez/aquifer/internal/manifest"
	"github.com/nledez/aquifer/internal/publish"
)

// --- helpers ----------------------------------------------------------------

func run(t *testing.T, store blobstore.Store, dir, repo, prefix string) *publish.Result {
	t.Helper()
	res, err := publish.Run(t.Context(), publish.Publication{
		Dir:    dir,
		Repo:   repo,
		Prefix: prefix,
	}, publish.Options{Store: store, Concurrency: 4})
	if err != nil {
		t.Fatalf("publish.Run: %v", err)
	}
	return res
}

// loadManifest reads back what publish uploaded.
func loadManifest(t *testing.T, store blobstore.Store, repo, revision string) *manifest.Manifest {
	t.Helper()
	rc, err := store.GetManifest(t.Context(), repo, revision)
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	defer func() { _ = rc.Close() }()
	m, err := manifest.Read(rc)
	if err != nil {
		t.Fatalf("manifest.Read: %v", err)
	}
	return m
}

func manifestPaths(m *manifest.Manifest) []string {
	var paths []string
	for e := range m.All() {
		paths = append(paths, e.Path)
	}
	return paths
}

// --- publish ----------------------------------------------------------------

func TestPublishUploadsEveryFileAndCommitsARef(t *testing.T) {
	t.Parallel()

	p := newPublication(t)
	nginx := p.addPackage("nginx", []byte("the nginx package"))
	apt := p.addPackage("apt", []byte("the apt package"))
	p.extras["keys/archive.gpg"] = []byte("an exported signing key")
	dir := p.write(t)

	store := blobstoretest.NewMem()
	res := run(t, store, dir, "debian/bookworm", "debian/bookworm")

	ref, err := store.GetRef(t.Context(), "debian/bookworm", "")
	if err != nil {
		t.Fatalf("GetRef: %v", err)
	}
	if ref.Revision != res.Revision {
		t.Fatalf("ref = %q, want the published revision %q", ref.Revision, res.Revision)
	}
	if !manifest.ValidRevision(res.Revision) {
		t.Fatalf("revision %q does not have the expected shape", res.Revision)
	}

	m := loadManifest(t, store, "debian/bookworm", res.Revision)
	want := []string{
		"debian/bookworm/dists/bookworm/Release",
		"debian/bookworm/dists/bookworm/main/binary-amd64/Packages",
		"debian/bookworm/dists/bookworm/main/binary-amd64/Packages.gz",
		"debian/bookworm/keys/archive.gpg",
		"debian/bookworm/" + apt,
		"debian/bookworm/" + nginx,
	}
	slices.Sort(want)
	if got := manifestPaths(m); !slices.Equal(got, want) {
		t.Fatalf("manifest paths =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// The digest recorded for a pool file must be the one the index declared.
	entry, ok := m.Lookup("debian/bookworm/" + nginx)
	if !ok {
		t.Fatal("nginx is missing from the manifest")
	}
	if entry.SHA256 != digest([]byte("the nginx package")) {
		t.Fatalf("nginx digest = %s", entry.SHA256)
	}

	// Every manifest entry must have a blob behind it.
	for e := range m.All() {
		if _, err := store.StatBlob(t.Context(), e.SHA256); err != nil {
			t.Fatalf("blob for %s is missing: %v", e.Path, err)
		}
	}
}

func TestPublishWithoutAPrefixServesFromTheRoot(t *testing.T) {
	t.Parallel()

	p := newPublication(t)
	p.addPackage("nginx", []byte("body"))
	dir := p.write(t)

	store := blobstoretest.NewMem()
	res := run(t, store, dir, "root", "")

	m := loadManifest(t, store, "root", res.Revision)
	if _, ok := m.Lookup("dists/bookworm/Release"); !ok {
		t.Fatalf("paths are not rooted: %v", manifestPaths(m))
	}
}

// SPEC section 4: aptly already hashed everything, so a second publication of
// unchanged content must upload nothing at all.
func TestRepublishingUnchangedContentUploadsNothing(t *testing.T) {
	t.Parallel()

	p := newPublication(t)
	p.addPackage("nginx", []byte("the nginx package"))
	dir := p.write(t)

	store := blobstoretest.NewMem()
	first := run(t, store, dir, "debian/bookworm", "debian/bookworm")
	if first.Uploaded == 0 {
		t.Fatal("the first publication uploaded nothing")
	}

	second := run(t, store, dir, "debian/bookworm", "debian/bookworm")
	if second.Uploaded != 0 {
		t.Fatalf("the second publication uploaded %d blobs, want 0", second.Uploaded)
	}
	if second.Skipped != first.Uploaded {
		t.Fatalf("skipped %d blobs, want %d", second.Skipped, first.Uploaded)
	}
	if second.Revision == first.Revision {
		t.Fatal("each publication must mint its own revision")
	}
}

// Only the changed file becomes a new blob; content addressing does the rest.
func TestPublishUploadsOnlyWhatChanged(t *testing.T) {
	t.Parallel()

	p := newPublication(t)
	p.addPackage("nginx", []byte("version one"))
	p.addPackage("apt", []byte("unchanged"))
	dir := p.write(t)

	store := blobstoretest.NewMem()
	run(t, store, dir, "debian/bookworm", "debian/bookworm")

	p.pool["pool/main/n/nginx/nginx_1.0_amd64.deb"] = []byte("version two")
	p.write(t)
	second := run(t, store, dir, "debian/bookworm", "debian/bookworm")

	// The changed package, plus the two index variants and the Release that
	// mention it. The unchanged package is not re-uploaded.
	if second.Uploaded != 4 {
		t.Fatalf("uploaded %d blobs, want 4 (package + 2 indices + Release)", second.Uploaded)
	}
}

// SPEC section 5: by-hash paths resolve straight from the digest in the URL,
// so they need no manifest entry, and their stale copies must not be published.
func TestPublishSkipsByHashDirectories(t *testing.T) {
	t.Parallel()

	p := newPublication(t)
	p.byHash = true
	p.addPackage("nginx", []byte("body"))
	dir := p.write(t)

	store := blobstoretest.NewMem()
	res := run(t, store, dir, "debian/bookworm", "debian/bookworm")

	m := loadManifest(t, store, "debian/bookworm", res.Revision)
	for _, path := range manifestPaths(m) {
		if strings.Contains(path, "by-hash") {
			t.Fatalf("manifest contains a by-hash path: %s", path)
		}
	}

	// The stale index copy must not have become a blob of its own.
	stale := digest(append([]byte("Package: stale\n"), p.packagesIndex()...))
	if _, err := store.StatBlob(t.Context(), stale); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatal("a stale by-hash copy was uploaded")
	}
}

func TestPublishReadsInRelease(t *testing.T) {
	t.Parallel()

	p := newPublication(t)
	p.signed = true
	p.addPackage("nginx", []byte("body"))
	dir := p.write(t)

	store := blobstoretest.NewMem()
	res := run(t, store, dir, "debian/bookworm", "debian/bookworm")

	m := loadManifest(t, store, "debian/bookworm", res.Revision)
	if _, ok := m.Lookup("debian/bookworm/dists/bookworm/InRelease"); !ok {
		t.Fatalf("InRelease is missing from the manifest: %v", manifestPaths(m))
	}
	if _, ok := m.Lookup("debian/bookworm/pool/main/n/nginx/nginx_1.0_amd64.deb"); !ok {
		t.Fatal("the pool file behind a signed Release was not published")
	}
}

// A publication whose index disagrees with the disk is broken. Publishing it
// would produce a manifest that 404s on the edge, long after the fact.
func TestPublishRefusesAnIndexThatDisagreesWithDisk(t *testing.T) {
	t.Parallel()

	t.Run("referenced file is missing", func(t *testing.T) {
		t.Parallel()
		p := newPublication(t)
		path := p.addPackage("nginx", []byte("body"))
		dir := p.write(t)
		if err := os.Remove(filepath.Join(dir, path)); err != nil {
			t.Fatalf("remove: %v", err)
		}
		assertPublishFails(t, dir, "debian/bookworm")
	})

	t.Run("referenced file has the wrong size", func(t *testing.T) {
		t.Parallel()
		p := newPublication(t)
		path := p.addPackage("nginx", []byte("body"))
		dir := p.write(t)
		writeFile(t, filepath.Join(dir, path), []byte("a much longer body than the index says"))
		assertPublishFails(t, dir, "debian/bookworm")
	})
}

func assertPublishFails(t *testing.T, dir, repo string) {
	t.Helper()

	store := blobstoretest.NewMem()
	_, err := publish.Run(t.Context(), publish.Publication{
		Dir: dir, Repo: repo, Prefix: repo,
	}, publish.Options{Store: store})
	if err == nil {
		t.Fatal("publish.Run accepted a publication that disagrees with its indices")
	}
	if _, err := store.GetRef(t.Context(), repo, ""); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatal("a failed publication still moved the ref")
	}
}

// SPEC section 4: the ref is the atomic commit. Nothing may reference a
// revision whose manifest did not land.
func TestPublishWritesTheRefLast(t *testing.T) {
	t.Parallel()

	p := newPublication(t)
	p.addPackage("nginx", []byte("body"))
	dir := p.write(t)

	rec := &recordingStore{Store: blobstoretest.NewMem()}
	run(t, rec, dir, "debian/bookworm", "debian/bookworm")

	ops := rec.operations()
	last := ops[len(ops)-1]
	if last != "SetRef" {
		t.Fatalf("last operation was %q, want SetRef; order: %v", last, ops)
	}
	if slices.Index(ops, "PutManifest") > slices.Index(ops, "SetRef") {
		t.Fatalf("the manifest landed after the ref: %v", ops)
	}
}

func TestPublishLeavesTheRefAloneWhenTheManifestFails(t *testing.T) {
	t.Parallel()

	p := newPublication(t)
	p.addPackage("nginx", []byte("body"))
	dir := p.write(t)

	store := &failingManifestStore{Store: blobstoretest.NewMem()}
	_, err := publish.Run(t.Context(), publish.Publication{
		Dir: dir, Repo: "debian/bookworm", Prefix: "debian/bookworm",
	}, publish.Options{Store: store})
	if err == nil {
		t.Fatal("publish.Run succeeded despite a manifest upload failure")
	}
	if _, err := store.GetRef(t.Context(), "debian/bookworm", ""); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatal("the ref moved even though the manifest never landed")
	}
}

func TestPublishRefusesADirectoryWithNoDists(t *testing.T) {
	t.Parallel()

	store := blobstoretest.NewMem()
	_, err := publish.Run(t.Context(), publish.Publication{
		Dir: t.TempDir(), Repo: "empty", Prefix: "",
	}, publish.Options{Store: store})
	if err == nil {
		t.Fatal("publish.Run accepted a directory that holds no publication")
	}
}

// --- test doubles -----------------------------------------------------------

// recordingStore notes the order of the mutating calls.
type recordingStore struct {
	blobstore.Store

	mu  sync.Mutex
	ops []string
}

func (r *recordingStore) note(op string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, op)
}

func (r *recordingStore) operations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ops)
}

func (r *recordingStore) PutBlob(ctx context.Context, hash string, body io.Reader, size int64) error {
	r.note("PutBlob")
	return r.Store.PutBlob(ctx, hash, body, size)
}

func (r *recordingStore) PutManifest(ctx context.Context, repo, revision string, body io.Reader, size int64) error {
	r.note("PutManifest")
	return r.Store.PutManifest(ctx, repo, revision, body, size)
}

func (r *recordingStore) SetRef(ctx context.Context, repo, revision string) error {
	r.note("SetRef")
	return r.Store.SetRef(ctx, repo, revision)
}

// failingManifestStore refuses to store the manifest.
type failingManifestStore struct {
	blobstore.Store
}

var errManifestRefused = errors.New("manifest upload refused")

func (f *failingManifestStore) PutManifest(context.Context, string, string, io.Reader, int64) error {
	return errManifestRefused
}

// apt-ftparchive lists the suite's own Release in the checksum sections it is
// generating, with a size and digest that cannot match the finished file:
// writing the digest into the file changes it. Real repositories are built
// this way, apt ignores the entry, and refusing to publish over it would make
// Aquifer unable to mirror an ordinary Debian archive.
//
// Found by the apt integration test, not by any unit test.
func TestPublishIgnoresAReleaseThatListsItself(t *testing.T) {
	t.Parallel()

	p := newPublication(t)
	p.addPackage("nginx", []byte("body"))
	dir := p.write(t)

	// Append a self-reference whose size and digest are deliberately wrong.
	releasePath := filepath.Join(dir, "dists", "bookworm", "Release")
	existing, err := os.ReadFile(releasePath)
	if err != nil {
		t.Fatalf("read Release: %v", err)
	}
	augmented := append(slices.Clone(existing), []byte(" "+digest([]byte("stale"))+" 111 Release\n")...)
	writeFile(t, releasePath, augmented)

	store := blobstoretest.NewMem()
	res := run(t, store, dir, "debian/bookworm", "debian/bookworm")

	m := loadManifest(t, store, "debian/bookworm", res.Revision)
	entry, ok := m.Lookup("debian/bookworm/dists/bookworm/Release")
	if !ok {
		t.Fatal("the Release file was not published")
	}
	// It is published with its real digest, computed from disk.
	if entry.SHA256 != digest(augmented) || entry.Size != int64(len(augmented)) {
		t.Fatalf("Release entry = %+v, want the digest of what is on disk", entry)
	}
}

// A per-component Release is a real index and must still be checked.
func TestPublishStillChecksAPerComponentRelease(t *testing.T) {
	t.Parallel()

	p := newPublication(t)
	p.addPackage("nginx", []byte("body"))
	dir := p.write(t)

	releasePath := filepath.Join(dir, "dists", "bookworm", "Release")
	existing, err := os.ReadFile(releasePath)
	if err != nil {
		t.Fatalf("read Release: %v", err)
	}
	augmented := append(slices.Clone(existing),
		[]byte(" "+digest([]byte("whatever"))+" 111 main/binary-amd64/Release\n")...)
	writeFile(t, releasePath, augmented)

	assertPublishFails(t, dir, "debian/bookworm")
}
