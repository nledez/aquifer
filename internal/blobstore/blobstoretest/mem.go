package blobstoretest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/nledez/aquifer/internal/blobstore"
)

// Mem is an in-memory blobstore.Store for tests and local experiments. It is
// deliberately simple: the S3 store is the one that has to be right, and this
// one exists so that the packages above it can be tested without a network.
type Mem struct {
	mu        sync.RWMutex
	blobs     map[string]object
	manifests map[string]map[string]object // repo -> revision -> object
	refs      map[string]blobstore.Ref
	clock     time.Time
}

type object struct {
	body     []byte
	modified time.Time
}

// NewMem returns an empty in-memory store.
func NewMem() *Mem {
	return &Mem{
		blobs:     map[string]object{},
		manifests: map[string]map[string]object{},
		refs:      map[string]blobstore.Ref{},
	}
}

// now returns a monotonically increasing timestamp so that objects written in
// the same nanosecond still order deterministically.
func (m *Mem) now() time.Time {
	if m.clock.IsZero() {
		m.clock = time.Now()
	} else {
		m.clock = m.clock.Add(time.Millisecond)
	}
	return m.clock
}

func (m *Mem) ListBlobs(ctx context.Context) ([]blobstore.BlobInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]blobstore.BlobInfo, 0, len(m.blobs))
	for hash, obj := range m.blobs {
		out = append(out, blobstore.BlobInfo{
			Hash:         hash,
			Size:         int64(len(obj.body)),
			LastModified: obj.modified,
		})
	}
	slices.SortFunc(out, func(a, b blobstore.BlobInfo) int {
		return cmpString(a.Hash, b.Hash)
	})
	return out, nil
}

func (m *Mem) StatBlob(ctx context.Context, hash string) (blobstore.BlobInfo, error) {
	if err := ctx.Err(); err != nil {
		return blobstore.BlobInfo{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	obj, ok := m.blobs[hash]
	if !ok {
		return blobstore.BlobInfo{}, fmt.Errorf("%w: blob %s", blobstore.ErrNotFound, hash)
	}
	return blobstore.BlobInfo{
		Hash:         hash,
		Size:         int64(len(obj.body)),
		LastModified: obj.modified,
	}, nil
}

func (m *Mem) PutBlob(ctx context.Context, hash string, r io.Reader, _ int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blobs[hash] = object{body: body, modified: m.now()}
	return nil
}

func (m *Mem) GetBlob(ctx context.Context, hash string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	obj, ok := m.blobs[hash]
	if !ok {
		return nil, fmt.Errorf("%w: blob %s", blobstore.ErrNotFound, hash)
	}
	return io.NopCloser(bytes.NewReader(obj.body)), nil
}

func (m *Mem) DeleteBlob(ctx context.Context, hash string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.blobs, hash)
	return nil
}

func (m *Mem) PutManifest(ctx context.Context, repo, revision string, r io.Reader, _ int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.manifests[repo] == nil {
		m.manifests[repo] = map[string]object{}
	}
	m.manifests[repo][revision] = object{body: body, modified: m.now()}
	return nil
}

func (m *Mem) GetManifest(ctx context.Context, repo, revision string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	obj, ok := m.manifests[repo][revision]
	if !ok {
		return nil, fmt.Errorf("%w: manifest %s/%s", blobstore.ErrNotFound, repo, revision)
	}
	return io.NopCloser(bytes.NewReader(obj.body)), nil
}

func (m *Mem) ListManifests(ctx context.Context, repo string) ([]blobstore.ManifestInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]blobstore.ManifestInfo, 0, len(m.manifests[repo]))
	for revision, obj := range m.manifests[repo] {
		out = append(out, blobstore.ManifestInfo{
			Revision:     revision,
			Size:         int64(len(obj.body)),
			LastModified: obj.modified,
		})
	}
	slices.SortFunc(out, func(a, b blobstore.ManifestInfo) int {
		return cmpString(a.Revision, b.Revision)
	})
	return out, nil
}

func (m *Mem) DeleteManifest(ctx context.Context, repo, revision string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.manifests[repo], revision)
	return nil
}

func (m *Mem) SetRef(ctx context.Context, repo, revision string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(revision))
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refs[repo] = blobstore.Ref{
		Revision: revision,
		ETag:     hex.EncodeToString(sum[:]),
	}
	return nil
}

func (m *Mem) GetRef(ctx context.Context, repo, ifNoneMatch string) (blobstore.Ref, error) {
	if err := ctx.Err(); err != nil {
		return blobstore.Ref{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	ref, ok := m.refs[repo]
	if !ok {
		return blobstore.Ref{}, fmt.Errorf("%w: ref %s", blobstore.ErrNotFound, repo)
	}
	if ifNoneMatch != "" && ifNoneMatch == ref.ETag {
		return blobstore.Ref{}, blobstore.ErrNotModified
	}
	return ref, nil
}

func (m *Mem) ListRepos(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]string, 0, len(m.refs))
	for repo := range m.refs {
		out = append(out, repo)
	}
	slices.Sort(out)
	return out, nil
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
