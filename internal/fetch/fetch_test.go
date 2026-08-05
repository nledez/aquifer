package fetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- test doubles -----------------------------------------------------------

// fakeSource serves blobs from memory and records how many times each hash was
// opened. That count is the whole point of the coalescer: it must stay at one
// no matter how many concurrent requesters show up.
type fakeSource struct {
	mu    sync.Mutex
	data  map[string][]byte
	opens map[string]int

	// gate, when non-nil for a hash, blocks Open until the channel is closed.
	gate map[string]chan struct{}

	// chunk splits the body into writes of this size, with a pause between
	// them, so that tests can observe partial progress.
	chunk int
	pause time.Duration

	// onProgress, if set, is called after each chunk with the bytes emitted so
	// far. It lets a test join a download at a precise point.
	onProgress func(sent int)

	// openErr is returned by Open instead of a body.
	openErr error

	// corrupt flips one byte of the body so the SHA-256 check fails.
	corrupt bool
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		data:  map[string][]byte{},
		opens: map[string]int{},
		gate:  map[string]chan struct{}{},
	}
}

// put stores a blob and returns its hash.
func (s *fakeSource) put(body []byte) string {
	sum := sha256.Sum256(body)
	h := hex.EncodeToString(sum[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[h] = body
	return h
}

func (s *fakeSource) blockOpen(hash string) func() {
	s.mu.Lock()
	ch := make(chan struct{})
	s.gate[hash] = ch
	s.mu.Unlock()
	return sync.OnceFunc(func() { close(ch) })
}

func (s *fakeSource) openCount(hash string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens[hash]
}

func (s *fakeSource) Open(ctx context.Context, hash string) (io.ReadCloser, error) {
	s.mu.Lock()
	s.opens[hash]++
	body, ok := s.data[hash]
	gate := s.gate[hash]
	chunk, pause, onProgress := s.chunk, s.pause, s.onProgress
	openErr, corrupt := s.openErr, s.corrupt
	s.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if openErr != nil {
		return nil, openErr
	}
	if !ok {
		return nil, fmt.Errorf("fakeSource: no such blob %s", hash)
	}

	body = bytes.Clone(body)
	if corrupt && len(body) > 0 {
		body[len(body)/2] ^= 0xff
	}
	if chunk <= 0 {
		chunk = len(body)
		if chunk == 0 {
			chunk = 1
		}
	}
	return &chunkedReader{
		body:       body,
		chunk:      chunk,
		pause:      pause,
		onProgress: onProgress,
	}, nil
}

// chunkedReader hands out the body in fixed-size pieces so tests can watch a
// download make progress.
type chunkedReader struct {
	body       []byte
	chunk      int
	pause      time.Duration
	onProgress func(sent int)
	pos        int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.body) {
		return 0, io.EOF
	}
	if r.pause > 0 {
		time.Sleep(r.pause)
	}
	n := min(min(r.chunk, len(p)), len(r.body)-r.pos)
	copy(p, r.body[r.pos:r.pos+n])
	r.pos += n
	if r.onProgress != nil {
		r.onProgress(r.pos)
	}
	return n, nil
}

func (r *chunkedReader) Close() error { return nil }

// fakeStore is a cache directory with a switchable admission policy.
type fakeStore struct {
	dir string

	mu       sync.Mutex
	refuse   bool
	admitted map[string]int64
	admitErr error
}

func newFakeStore(t *testing.T) *fakeStore {
	t.Helper()
	return &fakeStore{dir: t.TempDir(), admitted: map[string]int64{}}
}

func (s *fakeStore) setRefuse(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuse = v
}

func (s *fakeStore) has(hash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.admitted[hash]
	return ok
}

func (s *fakeStore) Open(hash string) (io.ReadSeekCloser, bool, error) {
	s.mu.Lock()
	_, ok := s.admitted[hash]
	s.mu.Unlock()
	if !ok {
		return nil, false, nil
	}
	f, err := os.Open(filepath.Join(s.dir, hash))
	if err != nil {
		return nil, false, err
	}
	return f, true, nil
}

func (s *fakeStore) Admit(hash string, size int64, tmpPath string) (bool, error) {
	s.mu.Lock()
	refuse, admitErr := s.refuse, s.admitErr
	s.mu.Unlock()

	if admitErr != nil {
		return false, admitErr
	}
	if refuse {
		return false, nil
	}
	if err := os.Rename(tmpPath, filepath.Join(s.dir, hash)); err != nil {
		return false, err
	}
	s.mu.Lock()
	s.admitted[hash] = size
	s.mu.Unlock()
	return true, nil
}

// --- helpers ----------------------------------------------------------------

func newTestCoalescer(t *testing.T, src Source, store Store) *Coalescer {
	t.Helper()
	c, err := New(Config{
		Source:  src,
		Store:   store,
		TempDir: t.TempDir(),
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func readAll(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}

func payload(size int, seed byte) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = seed + byte(i%251)
	}
	return b
}

// tempFilesIn counts leftovers so tests can assert the coalescer cleans up.
func tempFilesIn(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	return len(entries)
}

// --- tests ------------------------------------------------------------------

// SPEC section 7: 100 concurrent requesters on a missing blob must produce
// exactly one backend GET, and every one of them must get the right bytes.
func TestConcurrentRequestersShareASingleDownload(t *testing.T) {
	t.Parallel()

	src := newFakeSource()
	store := newFakeStore(t)
	body := payload(512*1024, 7)
	hash := src.put(body)
	release := src.blockOpen(hash)

	c := newTestCoalescer(t, src, store)

	const requesters = 100
	readers := make([]io.ReadCloser, requesters)
	var wg sync.WaitGroup
	for i := range requesters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc, err := c.Fetch(t.Context(), hash, int64(len(body)))
			if err != nil {
				t.Errorf("requester %d: Fetch: %v", i, err)
				return
			}
			readers[i] = rc
		}()
	}
	wg.Wait()
	release()

	for i, rc := range readers {
		if rc == nil {
			t.Fatalf("requester %d got no reader", i)
		}
		if got := readAll(t, rc); !bytes.Equal(got, body) {
			t.Fatalf("requester %d: got %d bytes, want %d", i, len(got), len(body))
		}
	}

	if n := src.openCount(hash); n != 1 {
		t.Fatalf("backend GET count = %d, want exactly 1", n)
	}
	if got := c.CoalescedReaders(); got != requesters-1 {
		t.Fatalf("coalesced readers = %d, want %d", got, requesters-1)
	}
	if !store.has(hash) {
		t.Fatal("blob was not admitted to the cache")
	}
}

// SPEC section 7: coalescing must not depend on the blob being cacheable. A
// blob the admission policy will refuse still gets exactly one GET.
func TestConcurrentRequestersShareASingleDownloadWhenAdmissionRefuses(t *testing.T) {
	t.Parallel()

	src := newFakeSource()
	store := newFakeStore(t)
	store.setRefuse(true)
	body := payload(256*1024, 11)
	hash := src.put(body)
	release := src.blockOpen(hash)

	c := newTestCoalescer(t, src, store)

	const requesters = 100
	readers := make([]io.ReadCloser, requesters)
	var wg sync.WaitGroup
	for i := range requesters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc, err := c.Fetch(t.Context(), hash, int64(len(body)))
			if err != nil {
				t.Errorf("requester %d: Fetch: %v", i, err)
				return
			}
			readers[i] = rc
		}()
	}
	wg.Wait()
	release()

	for i, rc := range readers {
		if rc == nil {
			t.Fatalf("requester %d got no reader", i)
		}
		if got := readAll(t, rc); !bytes.Equal(got, body) {
			t.Fatalf("requester %d: content mismatch", i)
		}
	}

	if n := src.openCount(hash); n != 1 {
		t.Fatalf("backend GET count = %d, want exactly 1", n)
	}
	if store.has(hash) {
		t.Fatal("refused blob must not be in the cache")
	}
	if n := tempFilesIn(t, c.TempDir()); n != 0 {
		t.Fatalf("%d temp files left behind, want 0", n)
	}
}

// SPEC section 7, non-negotiable constraint 1: the leader's download must not
// be tied to the leader's HTTP request context.
func TestFollowersCompleteAfterLeaderCancelsItsContext(t *testing.T) {
	t.Parallel()

	src := newFakeSource()
	src.chunk = 4096
	src.pause = time.Millisecond
	store := newFakeStore(t)
	body := payload(256*1024, 23)
	hash := src.put(body)

	c := newTestCoalescer(t, src, store)

	leaderCtx, cancelLeader := context.WithCancel(t.Context())
	leaderReader, err := c.Fetch(leaderCtx, hash, int64(len(body)))
	if err != nil {
		t.Fatalf("leader Fetch: %v", err)
	}

	followerReader, err := c.Fetch(t.Context(), hash, int64(len(body)))
	if err != nil {
		t.Fatalf("follower Fetch: %v", err)
	}

	// The leader gives up the way an apt client hitting Ctrl-C would.
	cancelLeader()
	_ = leaderReader.Close()

	if got := readAll(t, followerReader); !bytes.Equal(got, body) {
		t.Fatalf("follower got %d bytes, want %d", len(got), len(body))
	}
	if n := src.openCount(hash); n != 1 {
		t.Fatalf("backend GET count = %d, want exactly 1", n)
	}
	if !store.has(hash) {
		t.Fatal("blob should still have been admitted after the leader left")
	}
}

// SPEC section 7: a body whose SHA-256 does not match must fail every reader
// and leave nothing in the cache.
func TestChecksumMismatchFailsAllReadersAndCachesNothing(t *testing.T) {
	t.Parallel()

	src := newFakeSource()
	src.corrupt = true
	store := newFakeStore(t)
	body := payload(64*1024, 31)
	hash := src.put(body)
	release := src.blockOpen(hash)

	c := newTestCoalescer(t, src, store)

	const requesters = 20
	readers := make([]io.ReadCloser, requesters)
	var wg sync.WaitGroup
	for i := range requesters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc, err := c.Fetch(t.Context(), hash, int64(len(body)))
			if err != nil {
				return // failing at Fetch time is an acceptable outcome too
			}
			readers[i] = rc
		}()
	}
	wg.Wait()
	release()

	for i, rc := range readers {
		if rc == nil {
			continue
		}
		_, err := io.ReadAll(rc)
		_ = rc.Close()
		if err == nil {
			t.Fatalf("requester %d read a corrupt blob without error", i)
		}
		if !errors.Is(err, ErrChecksumMismatch) {
			t.Fatalf("requester %d: got %v, want ErrChecksumMismatch", i, err)
		}
	}

	if store.has(hash) {
		t.Fatal("corrupt blob must not be cached")
	}
	if n := tempFilesIn(t, c.TempDir()); n != 0 {
		t.Fatalf("%d temp files left behind, want 0", n)
	}
}

// SPEC section 7: a disk write failure part-way through behaves like any other
// failure - every reader errors out, nothing is cached.
func TestDiskWriteErrorFailsAllReadersAndCachesNothing(t *testing.T) {
	t.Parallel()

	src := newFakeSource()
	src.chunk = 4096
	store := newFakeStore(t)
	body := payload(256*1024, 41)
	hash := src.put(body)
	release := src.blockOpen(hash)

	c := newTestCoalescer(t, src, store)
	// Fail writes after the first few kilobytes have landed, so that followers
	// are already streaming when the disk gives up.
	c.createTemp = failingTempFactory(c.TempDir(), 8192)

	const requesters = 20
	readers := make([]io.ReadCloser, requesters)
	var wg sync.WaitGroup
	for i := range requesters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc, err := c.Fetch(t.Context(), hash, int64(len(body)))
			if err != nil {
				return
			}
			readers[i] = rc
		}()
	}
	wg.Wait()
	release()

	for i, rc := range readers {
		if rc == nil {
			continue
		}
		_, err := io.ReadAll(rc)
		_ = rc.Close()
		if err == nil {
			t.Fatalf("requester %d saw no error despite a disk failure", i)
		}
	}

	if store.has(hash) {
		t.Fatal("blob must not be cached after a write failure")
	}
	if n := tempFilesIn(t, c.TempDir()); n != 0 {
		t.Fatalf("%d temp files left behind, want 0", n)
	}
}

// SPEC section 7: joining at 99% progress still yields the whole blob.
func TestLateRequesterGetsTheCompleteBlob(t *testing.T) {
	t.Parallel()

	src := newFakeSource()
	store := newFakeStore(t)
	body := payload(100*1024, 53)
	hash := src.put(body)

	// Hold the download at 99% until a late requester has joined.
	almostThere := make(chan struct{})
	var signalled atomic.Bool
	proceed := make(chan struct{})
	src.chunk = 1024
	src.onProgress = func(sent int) {
		if sent >= len(body)-1024 && signalled.CompareAndSwap(false, true) {
			close(almostThere)
			<-proceed
		}
	}

	c := newTestCoalescer(t, src, store)

	leader, err := c.Fetch(t.Context(), hash, int64(len(body)))
	if err != nil {
		t.Fatalf("leader Fetch: %v", err)
	}
	defer func() { _ = leader.Close() }()

	<-almostThere
	late, err := c.Fetch(t.Context(), hash, int64(len(body)))
	if err != nil {
		t.Fatalf("late Fetch: %v", err)
	}
	close(proceed)

	if got := readAll(t, late); !bytes.Equal(got, body) {
		t.Fatalf("late requester got %d bytes, want %d", len(got), len(body))
	}
	if n := src.openCount(hash); n != 1 {
		t.Fatalf("backend GET count = %d, want exactly 1", n)
	}
}

// SPEC section 7: distinct hashes must not serialise behind each other.
func TestDifferentHashesDoNotBlockEachOther(t *testing.T) {
	t.Parallel()

	src := newFakeSource()
	store := newFakeStore(t)
	slowBody := payload(4096, 61)
	fastBody := payload(4096, 67)
	slowHash := src.put(slowBody)
	fastHash := src.put(fastBody)
	release := src.blockOpen(slowHash)

	c := newTestCoalescer(t, src, store)

	blocked, err := c.Fetch(t.Context(), slowHash, int64(len(slowBody)))
	if err != nil {
		t.Fatalf("Fetch(slow): %v", err)
	}
	// The held download deliberately outlives the request that started it, so
	// let it finish before the test tears its directories down.
	defer func() {
		release()
		_, _ = io.ReadAll(blocked)
		_ = blocked.Close()
	}()

	done := make(chan []byte, 1)
	go func() {
		rc, err := c.Fetch(t.Context(), fastHash, int64(len(fastBody)))
		if err != nil {
			done <- nil
			return
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		done <- b
	}()

	select {
	case got := <-done:
		if !bytes.Equal(got, fastBody) {
			t.Fatal("second hash returned wrong content")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a blocked download of one hash blocked an unrelated hash")
	}
}

// A blob already in the cache must never reach the backend.
func TestCachedBlobIsServedWithoutTouchingTheSource(t *testing.T) {
	t.Parallel()

	src := newFakeSource()
	store := newFakeStore(t)
	body := payload(8192, 71)
	hash := src.put(body)

	c := newTestCoalescer(t, src, store)

	first, err := c.Fetch(t.Context(), hash, int64(len(body)))
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	if got := readAll(t, first); !bytes.Equal(got, body) {
		t.Fatal("first fetch content mismatch")
	}

	second, err := c.Fetch(t.Context(), hash, int64(len(body)))
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if got := readAll(t, second); !bytes.Equal(got, body) {
		t.Fatal("second fetch content mismatch")
	}

	if n := src.openCount(hash); n != 1 {
		t.Fatalf("backend GET count = %d, want 1 (second read should hit the cache)", n)
	}
}

// SPEC section 7 point 5: once a refused download has finished, a new
// requester triggers a fresh download rather than reusing a dead entry.
func TestRequesterAfterRefusedAdmissionTriggersANewDownload(t *testing.T) {
	t.Parallel()

	src := newFakeSource()
	store := newFakeStore(t)
	store.setRefuse(true)
	body := payload(8192, 73)
	hash := src.put(body)

	c := newTestCoalescer(t, src, store)

	for i := range 3 {
		rc, err := c.Fetch(t.Context(), hash, int64(len(body)))
		if err != nil {
			t.Fatalf("Fetch %d: %v", i, err)
		}
		if got := readAll(t, rc); !bytes.Equal(got, body) {
			t.Fatalf("fetch %d: content mismatch", i)
		}
	}

	if n := src.openCount(hash); n != 3 {
		t.Fatalf("backend GET count = %d, want 3 (nothing is cached)", n)
	}
	if n := tempFilesIn(t, c.TempDir()); n != 0 {
		t.Fatalf("%d temp files left behind, want 0", n)
	}
}

// SPEC section 7 point 3: Range requests never bypass the coalescer. They wait
// for the download to finish and are then served from a seekable file.
func TestFetchSeekerWaitsForCompletionAndSeeks(t *testing.T) {
	t.Parallel()

	src := newFakeSource()
	src.chunk = 4096
	store := newFakeStore(t)
	store.setRefuse(true) // even a non-cacheable blob must be seekable
	body := payload(64*1024, 79)
	hash := src.put(body)

	c := newTestCoalescer(t, src, store)

	rs, err := c.FetchSeeker(t.Context(), hash, int64(len(body)))
	if err != nil {
		t.Fatalf("FetchSeeker: %v", err)
	}
	defer func() { _ = rs.Close() }()

	if _, err := rs.Seek(int64(len(body))-16, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	tail := make([]byte, 16)
	if _, err := io.ReadFull(rs, tail); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(tail, body[len(body)-16:]) {
		t.Fatal("seeked read returned the wrong bytes")
	}
	if n := src.openCount(hash); n != 1 {
		t.Fatalf("backend GET count = %d, want 1", n)
	}
}

// A backend that fails to open must surface the error to every requester.
func TestSourceOpenErrorFailsAllRequesters(t *testing.T) {
	t.Parallel()

	src := newFakeSource()
	store := newFakeStore(t)
	body := payload(4096, 83)
	hash := src.put(body)
	src.openErr = errors.New("backend unavailable")
	release := src.blockOpen(hash)

	c := newTestCoalescer(t, src, store)

	// Phase 1: everyone joins the download while the backend is still held.
	const requesters = 10
	readers := make([]io.ReadCloser, requesters)
	joinErrs := make([]error, requesters)
	var wg sync.WaitGroup
	for i := range requesters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			readers[i], joinErrs[i] = c.Fetch(t.Context(), hash, int64(len(body)))
		}()
	}
	wg.Wait()

	// Phase 2: the backend fails, and the failure must reach every requester.
	release()

	for i, rc := range readers {
		if rc == nil {
			if joinErrs[i] == nil {
				t.Fatalf("requester %d got neither reader nor error", i)
			}
			continue
		}
		_, err := io.ReadAll(rc)
		_ = rc.Close()
		if err == nil {
			t.Fatalf("requester %d saw no error", i)
		}
	}
	if n := tempFilesIn(t, c.TempDir()); n != 0 {
		t.Fatalf("%d temp files left behind, want 0", n)
	}
}

// --- disk failure injection -------------------------------------------------

// failingTempFactory returns a temp file factory whose files stop accepting
// writes after limit bytes, simulating a full or failing disk. The bytes
// written before the failure land on a real file so that followers can open
// and read it exactly as they would in production.
func failingTempFactory(dir string, limit int64) func(hash string) (tempFile, error) {
	return func(hash string) (tempFile, error) {
		f, err := os.CreateTemp(dir, "blob-*.part")
		if err != nil {
			return nil, err
		}
		return &failingTempFile{File: f, limit: limit}, nil
	}
}

type failingTempFile struct {
	*os.File
	limit   int64
	written int64
}

var errDiskFull = errors.New("simulated disk failure")

func (f *failingTempFile) Write(p []byte) (int, error) {
	if f.written >= f.limit {
		return 0, errDiskFull
	}
	truncated := false
	if f.written+int64(len(p)) > f.limit {
		p = p[:f.limit-f.written]
		truncated = true
	}
	n, err := f.File.Write(p)
	f.written += int64(n)
	if err != nil {
		return n, err
	}
	if truncated {
		return n, errDiskFull
	}
	return n, nil
}
