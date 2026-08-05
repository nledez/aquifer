// Package publish turns an aptly publication directory into an immutable
// revision in object storage, and collects what no retained revision still
// references.
//
// The master holds no state of its own. What has already been uploaded it
// learns from a listing, and what a publication contains it learns from the
// indices aptly already wrote. It never recomputes a digest a publication
// already carries.
package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nledez/aquifer/internal/blobstore"
	"github.com/nledez/aquifer/internal/debian"
	"github.com/nledez/aquifer/internal/manifest"
)

// Publication is one aptly publication directory and where it is served from.
type Publication struct {
	// Dir is the archive root: the directory holding dists/ and pool/.
	Dir string
	// Repo names the publication in object storage.
	Repo string
	// Prefix is prepended to every serving path. Empty publishes at the root.
	Prefix string
}

// Options configures a publication run.
type Options struct {
	Store blobstore.Store
	// Concurrency bounds parallel uploads. Zero picks a default from GOMAXPROCS.
	Concurrency int
	// Now mints the revision timestamp. Zero uses time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Result summarises what a run did.
type Result struct {
	Repo          string
	Revision      string
	Entries       int
	Bytes         int64
	Uploaded      int
	UploadedBytes int64
	Skipped       int
	// Hashed counts files no index mentioned, which had to be read to be
	// addressed: exported signing keys, the Release files themselves.
	Hashed int
}

const (
	// byHashDir holds copies of indices addressed by their own digest. The
	// edge resolves those paths straight from the digest in the URL, so they
	// need no manifest entry, and the superseded copies aptly keeps there must
	// not be published as blobs of their own.
	byHashDir = "by-hash"

	distsDir = "dists"
)

// Run publishes pub and returns what it did.
func Run(ctx context.Context, pub Publication, opts Options) (*Result, error) {
	if opts.Store == nil {
		return nil, errors.New("publish: store is required")
	}
	if pub.Repo == "" {
		return nil, errors.New("publish: repo is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = min(runtime.GOMAXPROCS(0), 8)
	}

	root, err := filepath.Abs(pub.Dir)
	if err != nil {
		return nil, fmt.Errorf("publish: %w", err)
	}
	if info, err := os.Stat(filepath.Join(root, distsDir)); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("publish: %s holds no %s/ directory; it is not an archive root",
			root, distsDir)
	}

	files, err := scanFiles(root)
	if err != nil {
		return nil, err
	}
	log.Info("scanned publication", "repo", pub.Repo, "files", len(files))

	known, err := readIndices(root, files)
	if err != nil {
		return nil, err
	}

	entries, hashed, err := resolveEntries(root, pub.Prefix, files, known)
	if err != nil {
		return nil, err
	}

	res := &Result{Repo: pub.Repo, Revision: manifest.NewRevision(now()), Hashed: hashed}
	for _, e := range entries {
		res.Entries++
		res.Bytes += e.Size
	}

	uploaded, skipped, uploadedBytes, err := uploadBlobs(ctx, opts.Store, root, pub.Prefix, entries, concurrency, log)
	if err != nil {
		return nil, err
	}
	res.Uploaded, res.Skipped, res.UploadedBytes = uploaded, skipped, uploadedBytes

	if err := putManifest(ctx, opts.Store, pub, res.Revision, now(), entries); err != nil {
		return nil, err
	}

	// The ref is written last and only last. Until it lands, the publication
	// has not happened: what it leaves behind is orphaned blobs, which the GC
	// collects, and never a ref pointing at a manifest that is not there.
	if err := opts.Store.SetRef(ctx, pub.Repo, res.Revision); err != nil {
		return nil, fmt.Errorf("publish: set ref: %w", err)
	}

	log.Info("published revision",
		"repo", pub.Repo, "revision", res.Revision,
		"entries", res.Entries, "uploaded", res.Uploaded, "skipped", res.Skipped)
	return res, nil
}

// scannedFile is one regular file of the publication.
type scannedFile struct {
	// rel is the archive-relative path, slash separated.
	rel  string
	size int64
}

// scanFiles walks the archive, skipping by-hash trees.
func scanFiles(root string) ([]scannedFile, error) {
	var out []scannedFile

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}

		if d.IsDir() {
			if d.Name() == byHashDir {
				return fs.SkipDir
			}
			return nil
		}

		// aptly links pool files rather than copying them, so follow the link
		// to find out what is really there.
		info, statErr := os.Stat(p)
		if statErr != nil {
			return fmt.Errorf("publish: %s: %w", rel, statErr)
		}
		if info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		out = append(out, scannedFile{rel: rel, size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("publish: scan %s: %w", root, err)
	}

	slices.SortFunc(out, func(a, b scannedFile) int { return strings.Compare(a.rel, b.rel) })
	return out, nil
}

// knownFile is a digest a publication already declared for one of its files.
type knownFile struct {
	sha256 string
	size   int64
	// source names the index that declared it, for error messages.
	source string
}

// readIndices collects every digest the publication states about itself: the
// index digests from each Release, and the pool digests from the Packages and
// Sources indices those Releases point at.
func readIndices(root string, files []scannedFile) (map[string]knownFile, error) {
	present := make(map[string]int64, len(files))
	for _, f := range files {
		present[f.rel] = f.size
	}

	known := map[string]knownFile{}
	for _, suiteDir := range releaseDirs(files) {
		relName, err := preferredRelease(suiteDir, present)
		if err != nil {
			return nil, err
		}

		rel, err := parseReleaseFile(filepath.Join(root, filepath.FromSlash(relName)), relName)
		if err != nil {
			return nil, err
		}

		for _, f := range rel.Files {
			indexPath := path.Join(suiteDir, f.Path)
			if err := record(known, indexPath, knownFile{
				sha256: f.SHA256, size: f.Size, source: relName,
			}); err != nil {
				return nil, err
			}
		}

		if err := readPoolIndices(root, suiteDir, rel, present, known); err != nil {
			return nil, err
		}
	}

	if len(known) == 0 {
		return nil, errors.New("publish: no Release file found under dists/")
	}
	return known, nil
}

// releaseDirs lists the suite directories, that is every directory under
// dists/ holding a Release or InRelease.
func releaseDirs(files []scannedFile) []string {
	seen := map[string]bool{}
	var dirs []string
	for _, f := range files {
		if !strings.HasPrefix(f.rel, distsDir+"/") {
			continue
		}
		base := path.Base(f.rel)
		if base != "Release" && base != "InRelease" {
			continue
		}
		dir := path.Dir(f.rel)
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	slices.Sort(dirs)
	return dirs
}

// preferredRelease picks InRelease over Release: both carry the same index
// list, and the signed one is what apt actually fetches.
func preferredRelease(suiteDir string, present map[string]int64) (string, error) {
	for _, name := range []string{"InRelease", "Release"} {
		candidate := path.Join(suiteDir, name)
		if _, ok := present[candidate]; ok {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("publish: %s holds no Release file", suiteDir)
}

func parseReleaseFile(fullPath, relName string) (*debian.Release, error) {
	f, err := os.Open(fullPath)
	if err != nil {
		return nil, fmt.Errorf("publish: %s: %w", relName, err)
	}
	defer func() { _ = f.Close() }()

	rel, err := debian.ParseRelease(f)
	if err != nil {
		return nil, fmt.Errorf("publish: %s: %w", relName, err)
	}
	return rel, nil
}

// readPoolIndices parses one variant of each Packages and Sources index the
// Release lists, and records the pool digests they declare.
func readPoolIndices(root, suiteDir string, rel *debian.Release, present map[string]int64, known map[string]knownFile) error {
	for _, base := range poolIndexBases(rel) {
		variant, ok := pickVariant(suiteDir, base, present)
		if !ok {
			// The Release lists an index whose every variant is absent. The
			// missing-file check below reports it with the right message.
			continue
		}

		if err := readPoolIndex(root, variant, base, known); err != nil {
			return err
		}
	}
	return nil
}

// poolIndexBases returns the uncompressed names of the Packages and Sources
// indices a Release lists, without duplicates.
func poolIndexBases(rel *debian.Release) []string {
	seen := map[string]bool{}
	var bases []string
	for _, f := range rel.Files {
		base := stripCompression(f.Path)
		name := path.Base(base)
		if name != "Packages" && name != "Sources" {
			continue
		}
		if !seen[base] {
			seen[base] = true
			bases = append(bases, base)
		}
	}
	slices.Sort(bases)
	return bases
}

func stripCompression(p string) string {
	for _, suffix := range []string{".gz", ".bz2", ".xz", ".zst", ".zstd", ".lzma"} {
		if trimmed, ok := strings.CutSuffix(p, suffix); ok {
			return trimmed
		}
	}
	return p
}

// pickVariant chooses the cheapest variant of an index that is on disk.
func pickVariant(suiteDir, base string, present map[string]int64) (string, bool) {
	for _, name := range debian.IndexVariants(base) {
		candidate := path.Join(suiteDir, name)
		if _, ok := present[candidate]; ok {
			return candidate, true
		}
	}
	return "", false
}

func readPoolIndex(root, variant, base string, known map[string]knownFile) error {
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(variant)))
	if err != nil {
		return fmt.Errorf("publish: %s: %w", variant, err)
	}
	defer func() { _ = f.Close() }()

	rc, err := debian.OpenIndex(variant, f)
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	defer func() { _ = rc.Close() }()

	seq := debian.Packages(rc)
	if path.Base(base) == "Sources" {
		seq = debian.Sources(rc)
	}
	for pf, err := range seq {
		if err != nil {
			return fmt.Errorf("publish: %s: %w", variant, err)
		}
		if err := record(known, pf.Path, knownFile{
			sha256: pf.SHA256, size: pf.Size, source: variant,
		}); err != nil {
			return err
		}
	}
	return nil
}

// record adds a declared digest, refusing a second, different claim about the
// same path. Two indices disagreeing means the publication is inconsistent,
// and picking either one would publish a lie.
func record(known map[string]knownFile, p string, kf knownFile) error {
	if existing, ok := known[p]; ok {
		if existing.sha256 != kf.sha256 || existing.size != kf.size {
			return fmt.Errorf("publish: %s: %s declares %s/%d but %s declares %s/%d",
				p, existing.source, existing.sha256, existing.size, kf.source, kf.sha256, kf.size)
		}
		return nil
	}
	known[p] = kf
	return nil
}

// resolveEntries turns the scanned files into manifest entries, trusting the
// declared digests and hashing only what no index mentioned.
func resolveEntries(root, prefix string, files []scannedFile, known map[string]knownFile) ([]manifest.Entry, int, error) {
	entries := make([]manifest.Entry, 0, len(files))
	onDisk := make(map[string]bool, len(files))
	var hashed int

	for _, f := range files {
		onDisk[f.rel] = true

		kf, declared := known[f.rel]
		if declared {
			// Sizes are free to check and catch a stale index without reading
			// 17 GiB back off the disk.
			if kf.size != f.size {
				return nil, 0, fmt.Errorf(
					"publish: %s: %s declares %d bytes but the file holds %d; the index is stale",
					f.rel, kf.source, kf.size, f.size)
			}
			entries = append(entries, manifest.Entry{
				Path:   servingPath(prefix, f.rel),
				SHA256: kf.sha256,
				Size:   f.size,
			})
			continue
		}

		// No index mentions this file: an exported signing key, the Release
		// files themselves. Ordinary blobs, they just have to be read once.
		sum, err := hashFile(filepath.Join(root, filepath.FromSlash(f.rel)))
		if err != nil {
			return nil, 0, err
		}
		hashed++
		entries = append(entries, manifest.Entry{
			Path:   servingPath(prefix, f.rel),
			SHA256: sum,
			Size:   f.size,
		})
	}

	// An index promising a file that is not there would publish a manifest
	// that 404s on the edge, long after anyone could connect the two.
	var missing []string
	for p := range known {
		if !onDisk[p] {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, 0, fmt.Errorf("publish: %d file(s) referenced by an index are missing from disk, first: %s",
			len(missing), missing[0])
	}

	slices.SortFunc(entries, func(a, b manifest.Entry) int { return strings.Compare(a.Path, b.Path) })
	return entries, hashed, nil
}

func servingPath(prefix, rel string) string {
	if prefix == "" {
		return rel
	}
	return path.Join(prefix, rel)
}

func hashFile(fullPath string) (string, error) {
	f, err := os.Open(fullPath)
	if err != nil {
		return "", fmt.Errorf("publish: %w", err)
	}
	defer func() { _ = f.Close() }()

	digest := sha256.New()
	if _, err := io.Copy(digest, f); err != nil {
		return "", fmt.Errorf("publish: hash %s: %w", fullPath, err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// uploadBlobs sends whatever the store does not already hold.
func uploadBlobs(ctx context.Context, store blobstore.Store, root, prefix string,
	entries []manifest.Entry, concurrency int, log *slog.Logger,
) (uploaded, skipped int, uploadedBytes int64, err error) {
	existing, err := store.ListBlobs(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("publish: list blobs: %w", err)
	}
	have := make(map[string]bool, len(existing))
	for _, info := range existing {
		have[info.Hash] = true
	}

	// Several paths can share one digest; each blob is uploaded once.
	type pending struct {
		hash string
		rel  string
		size int64
	}
	var todo []pending
	queued := map[string]bool{}
	for _, e := range entries {
		if queued[e.SHA256] {
			continue
		}
		queued[e.SHA256] = true
		if have[e.SHA256] {
			skipped++
			continue
		}
		todo = append(todo, pending{hash: e.SHA256, rel: archiveRel(prefix, e.Path), size: e.Size})
	}

	log.Info("uploading blobs", "missing", len(todo), "present", skipped)

	work := make(chan pending)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range work {
				if uploadErr := uploadOne(uploadCtx, store, root, p.hash, p.rel, p.size); uploadErr != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = uploadErr
						cancel()
					}
					mu.Unlock()
					return
				}
				mu.Lock()
				uploaded++
				uploadedBytes += p.size
				mu.Unlock()
			}
		}()
	}

	for _, p := range todo {
		select {
		case work <- p:
		case <-uploadCtx.Done():
		}
	}
	close(work)
	wg.Wait()

	if firstErr != nil {
		return 0, 0, 0, firstErr
	}
	return uploaded, skipped, uploadedBytes, nil
}

func uploadOne(ctx context.Context, store blobstore.Store, root, hash, rel string, size int64) error {
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	defer func() { _ = f.Close() }()

	if err := store.PutBlob(ctx, hash, f, size); err != nil {
		return fmt.Errorf("publish: upload %s: %w", rel, err)
	}
	return nil
}

// archiveRel undoes servingPath.
func archiveRel(prefix, servingPath string) string {
	if prefix == "" {
		return servingPath
	}
	return strings.TrimPrefix(servingPath, prefix+"/")
}

func putManifest(ctx context.Context, store blobstore.Store, pub Publication,
	revision string, now time.Time, entries []manifest.Entry,
) error {
	// A manifest is metadata, not a blob: tens of kilobytes compressed, so it
	// is built in memory to give the store the length it needs up front.
	var buf bytes.Buffer
	err := manifest.Write(&buf, manifest.Meta{
		Repo:      pub.Repo,
		Revision:  revision,
		CreatedAt: now.UTC(),
	}, entries)
	if err != nil {
		return fmt.Errorf("publish: build manifest: %w", err)
	}

	if err := store.PutManifest(ctx, pub.Repo, revision, &buf, int64(buf.Len())); err != nil {
		return fmt.Errorf("publish: upload manifest: %w", err)
	}
	return nil
}
