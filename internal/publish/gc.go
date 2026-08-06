package publish

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/nledez/aquifer/internal/blobstore"
	"github.com/nledez/aquifer/internal/manifest"
)

// GCOptions configures a garbage collection run.
type GCOptions struct {
	Store blobstore.Store
	// Keep is how many revisions of each repo stay resolvable. Zero uses the
	// manifest package's default window.
	Keep int
	// Grace protects recently written blobs. A nil pointer uses DefaultGrace;
	// a zero duration genuinely disables the protection.
	//
	// It is a pointer precisely so that those two cases stay distinguishable.
	// Silently turning an explicit zero into 24 hours would ignore an
	// operator's instruction on a destructive command, and defaulting the zero
	// value to no protection at all would arm the footgun the grace period
	// exists to disarm.
	Grace *time.Duration
	// Now is the clock the grace period is measured against.
	Now    func() time.Time
	DryRun bool
	Logger *slog.Logger
}

// GCResult summarises a run. In a dry run the deletion counts report what
// would have been removed.
type GCResult struct {
	Repos            int
	ScannedBlobs     int
	ReferencedBlobs  int
	DeletedBlobs     int
	DeletedBytes     int64
	KeptYoung        int
	DeletedManifests int
}

// DefaultGrace is how long a blob is protected from collection regardless of
// whether anything references it.
//
// A publication uploads its blobs before it writes its ref, so between those
// two moments its blobs look exactly like orphans. Collecting them would
// destroy a publication that is still running.
const DefaultGrace = 24 * time.Hour

// GC deletes the blobs no retained revision references, and the manifests that
// have fallen out of every repo's window.
func GC(ctx context.Context, opts GCOptions) (*GCResult, error) {
	if opts.Store == nil {
		return nil, errors.New("publish: store is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	keep := opts.Keep
	if keep <= 0 {
		keep = manifest.DefaultWindowSize
	}
	grace := DefaultGrace
	if opts.Grace != nil {
		grace = *opts.Grace
	}
	if grace < 0 {
		return nil, fmt.Errorf("publish: grace period cannot be negative, got %s", grace)
	}
	if grace < DefaultGrace {
		log.Warn("grace period is shorter than the default; a publication in flight may lose its blobs",
			"grace", grace, "default", DefaultGrace)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	repos, err := opts.Store.ListRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("publish: list repos: %w", err)
	}

	res := &GCResult{Repos: len(repos)}
	referenced := map[string]bool{}

	for _, repo := range repos {
		retained, dropped, planErr := planRepo(ctx, opts.Store, repo, keep)
		if planErr != nil {
			return nil, planErr
		}

		// Read every retained manifest before deleting anything. A manifest we
		// cannot read is a hard failure: treating it as referencing nothing
		// would delete precisely the blobs it alone protects.
		for _, revision := range retained {
			if refErr := collectReferences(ctx, opts.Store, repo, revision, referenced); refErr != nil {
				return nil, refErr
			}
		}

		for _, revision := range dropped {
			log.Info("dropping manifest", "repo", repo, "revision", revision, "dry_run", opts.DryRun)
			res.DeletedManifests++
			if opts.DryRun {
				continue
			}
			if delErr := opts.Store.DeleteManifest(ctx, repo, revision); delErr != nil {
				return nil, fmt.Errorf("publish: delete manifest %s/%s: %w", repo, revision, delErr)
			}
		}
	}
	res.ReferencedBlobs = len(referenced)

	blobs, err := opts.Store.ListBlobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("publish: list blobs: %w", err)
	}
	res.ScannedBlobs = len(blobs)

	cutoff := now().Add(-grace)
	for _, info := range blobs {
		if referenced[info.Hash] {
			continue
		}
		if info.LastModified.After(cutoff) {
			// Possibly a publication in flight. Leave it for the next run.
			res.KeptYoung++
			continue
		}

		res.DeletedBlobs++
		res.DeletedBytes += info.Size
		if opts.DryRun {
			continue
		}
		if err := opts.Store.DeleteBlob(ctx, info.Hash); err != nil {
			return nil, fmt.Errorf("publish: delete blob %s: %w", info.Hash, err)
		}
	}

	log.Info("garbage collection complete",
		"repos", res.Repos, "scanned", res.ScannedBlobs, "referenced", res.ReferencedBlobs,
		"deleted", res.DeletedBlobs, "kept_young", res.KeptYoung,
		"deleted_manifests", res.DeletedManifests, "dry_run", opts.DryRun)
	return res, nil
}

// planRepo splits a repo's revisions into those that stay resolvable and those
// that fall out of the window.
func planRepo(ctx context.Context, store blobstore.Store, repo string, keep int) (retained, dropped []string, err error) {
	infos, err := store.ListManifests(ctx, repo)
	if err != nil {
		return nil, nil, fmt.Errorf("publish: list manifests of %s: %w", repo, err)
	}

	revisions := make([]string, len(infos))
	for i, info := range infos {
		revisions[i] = info.Revision
	}
	slices.Sort(revisions) // revisions sort lexicographically by time

	keepSet := map[string]bool{}
	for _, revision := range revisions[max(0, len(revisions)-keep):] {
		keepSet[revision] = true
	}

	// Whatever the window says, the revision the edges are serving right now
	// has to survive. An operator who rolled back to an older revision would
	// otherwise have it collected under them.
	switch ref, err := store.GetRef(ctx, repo, ""); {
	case err == nil:
		keepSet[ref.Revision] = true
	case errors.Is(err, blobstore.ErrNotFound):
		// A repo with manifests but no ref was never committed. Its revisions
		// age out normally.
	default:
		return nil, nil, fmt.Errorf("publish: read ref of %s: %w", repo, err)
	}

	for _, revision := range revisions {
		if keepSet[revision] {
			retained = append(retained, revision)
			continue
		}
		dropped = append(dropped, revision)
	}
	return retained, dropped, nil
}

func collectReferences(ctx context.Context, store blobstore.Store, repo, revision string, into map[string]bool) error {
	rc, err := store.GetManifest(ctx, repo, revision)
	if err != nil {
		return fmt.Errorf("publish: read manifest %s/%s: %w", repo, revision, err)
	}
	defer func() { _ = rc.Close() }()

	m, err := manifest.Read(rc)
	if err != nil {
		return fmt.Errorf("publish: parse manifest %s/%s: %w", repo, revision, err)
	}
	for e := range m.All() {
		into[e.SHA256] = true
	}
	return nil
}
