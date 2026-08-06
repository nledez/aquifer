// Package fetch shares a single in-flight download between every requester of
// the same blob.
//
// At any instant there is at most one GET to object storage per hash. The
// first requester becomes the leader: it streams the body into a temp file and
// publishes its progress. Every later requester becomes a follower and reads
// that same temp file, waiting whenever it catches up with the leader.
//
// Every miss takes this path, whether or not the blob will end up cached. The
// admission decision happens only once the body is complete and verified, so a
// blob the cache refuses is still coalesced. Without the temp file, followers
// would have nothing to follow.
package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Errors reported to every requester of a failed download.
var (
	// ErrChecksumMismatch means the body did not hash to the expected value.
	ErrChecksumMismatch = errors.New("fetch: sha256 mismatch")
	// ErrSizeMismatch means the body length disagreed with the manifest.
	ErrSizeMismatch = errors.New("fetch: size mismatch")
)

// errCacheRace is internal: the leader found the blob already cached between
// the caller's lookup and its own registration, so the caller should retry.
var errCacheRace = errors.New("fetch: blob became cached, retry")

// Source is the object storage side: it hands out blob bodies by hash.
type Source interface {
	Open(ctx context.Context, hash string) (io.ReadCloser, error)
}

// Store is the local cache seen by the coalescer. It knows nothing about
// eviction or pinning; those live in the cache package.
type Store interface {
	// Open returns a reader for a cached blob, reporting whether it was there.
	Open(hash string) (io.ReadSeekCloser, bool, error)

	// Admit takes ownership of a fully written and verified temp file. It
	// reports whether the admission policy accepted the blob; when it did not,
	// the coalescer removes the temp file itself.
	Admit(hash string, size int64, tmpPath string) (bool, error)
}

// tempFile is the part of *os.File the downloader needs. It exists as an
// interface so that tests can inject write failures part-way through.
type tempFile interface {
	io.WriteCloser
	Name() string
	Sync() error
}

// Config configures a Coalescer.
type Config struct {
	Source  Source
	Store   Store
	TempDir string

	// Timeout bounds a single download. It applies to the leader's own
	// context, which is deliberately detached from any requester's context.
	Timeout time.Duration
}

const (
	defaultTimeout = 30 * time.Minute
	copyBufferSize = 128 * 1024
)

// Coalescer shares in-flight downloads between requesters.
type Coalescer struct {
	source  Source
	store   Store
	tempDir string
	timeout time.Duration

	// createTemp is a seam for tests; production uses os.CreateTemp.
	createTemp func(hash string) (tempFile, error)

	bufs sync.Pool

	mu      sync.Mutex
	entries map[string]*entry

	// downloads tracks the leader goroutines so that a shutdown can wait for
	// them. They run detached from any request context by design, so nothing
	// else would ever join them.
	downloads sync.WaitGroup

	inflight  atomic.Int64
	coalesced atomic.Int64
}

// New validates cfg and prepares the temp directory.
func New(cfg Config) (*Coalescer, error) {
	if cfg.Source == nil {
		return nil, errors.New("fetch: Source is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("fetch: Store is required")
	}
	if cfg.TempDir == "" {
		return nil, errors.New("fetch: TempDir is required")
	}
	if err := os.MkdirAll(cfg.TempDir, 0o755); err != nil {
		return nil, fmt.Errorf("fetch: temp dir: %w", err)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	c := &Coalescer{
		source:  cfg.Source,
		store:   cfg.Store,
		tempDir: cfg.TempDir,
		timeout: timeout,
		entries: map[string]*entry{},
	}
	c.bufs.New = func() any {
		b := make([]byte, copyBufferSize)
		return &b
	}
	c.createTemp = func(string) (tempFile, error) {
		return os.CreateTemp(c.tempDir, "blob-*.part")
	}
	return c, nil
}

// TempDir reports where in-flight downloads are staged. The bytes living here
// are outside the cache budget and have their own disk reservation.
func (c *Coalescer) TempDir() string { return c.tempDir }

// Inflight reports how many downloads are currently running.
func (c *Coalescer) Inflight() int64 { return c.inflight.Load() }

// CoalescedReaders reports how many requesters were served by an already
// running download. This is the number that proves coalescing works.
func (c *Coalescer) CoalescedReaders() int64 { return c.coalesced.Load() }

// Wait blocks until every in-flight download has finished, or ctx expires.
//
// Downloads deliberately outlive the requests that started them, so a shutdown
// that simply stopped accepting connections would leave goroutines writing
// into a cache directory nobody owns any more. Waiting makes the wind-down
// ordered; the leftover temp files a hard kill leaves behind are cleared at
// the next startup either way.
func (c *Coalescer) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		c.downloads.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Fetch returns the blob body, starting or joining a download as needed. The
// returned reader streams: it may block while the leader catches up.
func (c *Coalescer) Fetch(ctx context.Context, hash string, size int64) (io.ReadCloser, error) {
	return c.fetch(ctx, hash, size, false)
}

// FetchSeeker returns a seekable reader, waiting for the download to complete
// first. Range requests use this: they never bypass the coalescer, they simply
// wait rather than tracking a moving target.
func (c *Coalescer) FetchSeeker(ctx context.Context, hash string, size int64) (io.ReadSeekCloser, error) {
	rc, err := c.fetch(ctx, hash, size, true)
	if err != nil {
		return nil, err
	}
	rs, ok := rc.(io.ReadSeekCloser)
	if !ok {
		_ = rc.Close()
		return nil, errors.New("fetch: reader is not seekable")
	}
	return rs, nil
}

func (c *Coalescer) fetch(ctx context.Context, hash string, size int64, wait bool) (io.ReadCloser, error) {
	// One retry is enough: the only reason to loop is the narrow window where
	// another download admitted this blob while we were registering.
	for range 2 {
		cached, ok, err := c.store.Open(hash)
		if err != nil {
			return nil, err
		}
		if ok {
			return cached, nil
		}

		rc, err := c.join(ctx, hash, size, wait)
		if errors.Is(err, errCacheRace) {
			continue
		}
		return rc, err
	}
	// Losing the race twice in a row would mean the blob is being admitted and
	// evicted in a tight loop, which the cache never does.
	return nil, fmt.Errorf("fetch: %s: gave up after repeated cache races", hash)
}

func (c *Coalescer) join(ctx context.Context, hash string, size int64, wait bool) (io.ReadCloser, error) {
	e, leader := c.register(hash, size)
	if leader {
		// The leader's download must outlive its own request: the first client
		// to hit Ctrl-C must not cancel the download the other clients are
		// following. Keep the request's values, drop its cancellation.
		c.downloads.Add(1)
		go c.download(context.WithoutCancel(ctx), e)
	} else {
		c.coalesced.Add(1)
	}

	select {
	case <-e.ready:
	case <-ctx.Done():
		e.release()
		return nil, ctx.Err()
	}

	if e.startErr != nil {
		err := e.startErr
		e.release()
		return nil, err
	}

	if wait {
		if err := e.waitDone(ctx); err != nil {
			e.release()
			return nil, err
		}
	}
	return &blobReader{ctx: ctx, e: e}, nil
}

// register returns the entry for hash, creating it if this caller is the first
// one in. The reference count is bumped under the same lock that publishes the
// entry, so an entry can never be torn down out from under a joiner.
func (c *Coalescer) register(hash string, size int64) (*entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.entries[hash]; ok {
		e.acquire()
		return e, false
	}
	e := newEntry(hash, size)
	e.acquire() // the caller
	e.acquire() // the download goroutine
	c.entries[hash] = e
	return e, true
}

func (c *Coalescer) unregister(e *entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries[e.hash] == e {
		delete(c.entries, e.hash)
	}
}

// download runs on its own goroutine with a context detached from any request.
func (c *Coalescer) download(parent context.Context, e *entry) {
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()
	defer c.downloads.Done()
	defer e.release()

	// Another download may have admitted this blob between our caller's cache
	// miss and our registration. Checking here, after registering, closes that
	// window without ever reading state before publishing the entry.
	if cached, ok, err := c.store.Open(e.hash); err == nil && ok {
		_ = cached.Close()
		c.unregister(e)
		e.abort(errCacheRace)
		return
	}

	f, err := c.createTemp(e.hash)
	if err != nil {
		c.unregister(e)
		e.abort(fmt.Errorf("fetch: create temp file: %w", err))
		return
	}
	path := f.Name()

	rf, err := os.Open(path)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		c.unregister(e)
		e.abort(fmt.Errorf("fetch: reopen temp file: %w", err))
		return
	}

	// These are published under the lock, not just before closing ready. A
	// requester whose own context died can call release before ready is ever
	// closed, and release reads rf to decide whether to close it.
	e.mu.Lock()
	e.path = path
	e.rf = rf
	e.mu.Unlock()
	close(e.ready)

	c.inflight.Add(1)
	written, err := c.stream(ctx, e, f)
	c.inflight.Add(-1)
	_ = f.Close()

	// From here on no new follower can join, so disposing of the temp file is
	// safe: everyone already reads through the shared descriptor.
	c.unregister(e)

	if err != nil {
		_ = os.Remove(path)
		e.finish(err)
		return
	}

	admitted, aerr := c.store.Admit(e.hash, written, path)
	if aerr != nil || !admitted {
		_ = os.Remove(path)
	}
	e.finish(nil)
}

// stream copies the body into f, publishing progress as it goes and verifying
// the content before declaring success. Nothing is ever buffered whole.
func (c *Coalescer) stream(ctx context.Context, e *entry, f tempFile) (int64, error) {
	body, err := c.source.Open(ctx, e.hash)
	if err != nil {
		return 0, err
	}
	defer func() { _ = body.Close() }()

	bufp, _ := c.bufs.Get().(*[]byte)
	defer c.bufs.Put(bufp)
	buf := *bufp

	digest := sha256.New()
	var written int64
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return written, fmt.Errorf("fetch: write temp file: %w", werr)
			}
			_, _ = digest.Write(buf[:n])
			written += int64(n)
			e.advanceTo(written)
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return written, fmt.Errorf("fetch: read body: %w", rerr)
		}
		if cerr := ctx.Err(); cerr != nil {
			return written, cerr
		}
	}

	if e.size > 0 && written != e.size {
		return written, fmt.Errorf("%w: got %d bytes, want %d", ErrSizeMismatch, written, e.size)
	}
	if sum := hex.EncodeToString(digest.Sum(nil)); sum != e.hash {
		return written, fmt.Errorf("%w: got %s, want %s", ErrChecksumMismatch, sum, e.hash)
	}
	if err := f.Sync(); err != nil {
		return written, fmt.Errorf("fetch: sync temp file: %w", err)
	}
	return written, nil
}

// entry is one in-flight download, shared by its leader and its followers.
type entry struct {
	hash string
	size int64

	// ready is closed once path and rf are set, or startErr explains why they
	// never will be.
	ready    chan struct{}
	startErr error
	path     string
	rf       *os.File

	// written advances as the leader makes progress. Readers poll it without
	// taking a lock, which keeps the hot path lock-free.
	written atomic.Int64

	mu sync.Mutex
	// advance is replaced and closed on every advance. A channel rather than a
	// sync.Cond because it composes with ctx.Done().
	advance chan struct{}
	done    bool
	err     error
	refs    int
}

func newEntry(hash string, size int64) *entry {
	return &entry{
		hash:    hash,
		size:    size,
		ready:   make(chan struct{}),
		advance: make(chan struct{}),
	}
}

func (e *entry) acquire() {
	e.mu.Lock()
	e.refs++
	e.mu.Unlock()
}

// release drops one reference and closes the shared descriptor once the leader
// has finished and every follower has gone.
func (e *entry) release() {
	e.mu.Lock()
	e.refs--
	last := e.refs == 0 && e.done
	rf := e.rf
	e.mu.Unlock()

	if last && rf != nil {
		_ = rf.Close()
	}
}

// abort reports that the download never started.
func (e *entry) abort(err error) {
	e.startErr = err
	close(e.ready)
	e.finish(err)
}

func (e *entry) advanceTo(n int64) {
	e.written.Store(n)

	e.mu.Lock()
	ch := e.advance
	e.advance = make(chan struct{})
	e.mu.Unlock()

	close(ch)
}

func (e *entry) finish(err error) {
	e.mu.Lock()
	e.done = true
	e.err = err
	ch := e.advance
	e.advance = make(chan struct{})
	e.mu.Unlock()

	close(ch)
}

// state snapshots what a reader needs in one lock acquisition.
func (e *entry) state() (done bool, advance chan struct{}, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.done, e.advance, e.err
}

func (e *entry) waitDone(ctx context.Context) error {
	for {
		done, advance, err := e.state()
		if done {
			return err
		}
		select {
		case <-advance:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// blobReader is one requester's view of the shared temp file. Readers share a
// single descriptor through ReadAt, so no one has to reopen a path that may
// already have been renamed into the cache or unlinked.
type blobReader struct {
	ctx    context.Context
	e      *entry
	pos    int64
	closed bool
}

func (r *blobReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, os.ErrClosed
	}
	for {
		if available := r.e.written.Load() - r.pos; available > 0 {
			if int64(len(p)) > available {
				p = p[:available]
			}
			n, err := r.e.rf.ReadAt(p, r.pos)
			r.pos += int64(n)
			if n > 0 {
				return n, nil
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return 0, err
			}
			continue
		}

		done, advance, err := r.e.state()
		if r.e.written.Load() > r.pos {
			continue // progressed while we were reading the state
		}
		if done {
			if err != nil {
				return 0, err
			}
			return 0, io.EOF
		}

		select {
		case <-advance:
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
}

// Seek is meaningful once the download has completed, which is what
// FetchSeeker guarantees before handing this reader out.
func (r *blobReader) Seek(offset int64, whence int) (int64, error) {
	if r.closed {
		return 0, os.ErrClosed
	}
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		abs = r.e.written.Load() + offset
	default:
		return 0, fmt.Errorf("fetch: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, errors.New("fetch: negative position")
	}
	r.pos = abs
	return abs, nil
}

func (r *blobReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.e.release()
	return nil
}
