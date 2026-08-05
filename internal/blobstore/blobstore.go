// Package blobstore is the object storage side of Aquifer: immutable blobs
// addressed by digest, immutable per-revision manifests, and one mutable ref
// per repo.
//
// The master keeps no local state at all. Everything it needs to know about
// what has already been published, it learns from a listing. At the scale this
// project works at that is a single paginated request, which is cheaper than
// any index would be to maintain and impossible to get out of sync.
package blobstore

import (
	"context"
	"errors"
	"io"
	"time"
)

// Sentinel errors every implementation reports.
var (
	// ErrNotFound reports an absent object.
	ErrNotFound = errors.New("blobstore: object not found")
	// ErrNotModified answers a conditional read whose ETag still matches.
	ErrNotModified = errors.New("blobstore: not modified")
)

// BlobInfo describes a stored blob. LastModified is what the GC's grace period
// is measured against: a blob younger than the grace period may belong to a
// publication still in flight, whose ref has not landed yet.
type BlobInfo struct {
	Hash         string
	Size         int64
	LastModified time.Time
}

// ManifestInfo describes one stored revision manifest.
type ManifestInfo struct {
	Revision     string
	Size         int64
	LastModified time.Time
}

// Ref is a repo's current revision pointer, with the ETag that makes polling
// it cheap.
type Ref struct {
	Revision string
	ETag     string
}

// Store is the object storage contract. Implementations are safe for
// concurrent use.
type Store interface {
	// ListBlobs enumerates every blob. This is the master's whole inventory.
	ListBlobs(ctx context.Context) ([]BlobInfo, error)
	StatBlob(ctx context.Context, hash string) (BlobInfo, error)
	PutBlob(ctx context.Context, hash string, r io.Reader, size int64) error
	GetBlob(ctx context.Context, hash string) (io.ReadCloser, error)
	// DeleteBlob succeeds when the blob is already gone, so that a GC run can
	// be interrupted and restarted without bookkeeping.
	DeleteBlob(ctx context.Context, hash string) error

	PutManifest(ctx context.Context, repo, revision string, r io.Reader, size int64) error
	GetManifest(ctx context.Context, repo, revision string) (io.ReadCloser, error)
	// ListManifests returns a repo's revisions in ascending revision order.
	ListManifests(ctx context.Context, repo string) ([]ManifestInfo, error)
	DeleteManifest(ctx context.Context, repo, revision string) error

	// SetRef publishes a revision. It is written last and is the atomic commit
	// of a publication: until it lands, nothing has happened.
	SetRef(ctx context.Context, repo, revision string) error
	// GetRef reads a repo's pointer. When ifNoneMatch is non-empty and still
	// current, it reports ErrNotModified instead of transferring anything.
	GetRef(ctx context.Context, repo, ifNoneMatch string) (Ref, error)
	// ListRepos discovers the published repos from the refs.
	ListRepos(ctx context.Context) ([]string, error)
}

// BlobSource adapts a Store to the blob source an edge's download coalescer
// expects.
type BlobSource struct {
	Store Store
}

// Open streams a blob by digest.
func (b BlobSource) Open(ctx context.Context, hash string) (io.ReadCloser, error) {
	return b.Store.GetBlob(ctx, hash)
}
