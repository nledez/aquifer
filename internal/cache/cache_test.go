package cache_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nledez/aquifer/internal/cache"
	"github.com/nledez/aquifer/internal/fetch"
)

// The cache is what the download coalescer admits into, so it has to satisfy
// that contract exactly.
var _ fetch.Store = (*cache.Cache)(nil)

func newCache(t *testing.T, cfg cache.Config) *cache.Cache {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	if cfg.MaxSize == 0 {
		cfg.MaxSize = 1 << 20
	}
	if cfg.PinnedMaxSize == 0 {
		cfg.PinnedMaxSize = 1 << 20
	}
	c, err := cache.New(cfg)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func digestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// admit stages a body in the cache's temp directory and offers it, exactly the
// way the coalescer does once a download is complete and verified.
func admit(t *testing.T, c *cache.Cache, body []byte) (hash string, accepted bool) {
	t.Helper()

	hash = digestOf(body)
	f, err := os.CreateTemp(c.TempDir(), "blob-*.part")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := f.Write(body); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp: %v", err)
	}

	accepted, err = c.Admit(hash, int64(len(body)), f.Name())
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if !accepted {
		_ = os.Remove(f.Name())
	}
	return hash, accepted
}

func read(t *testing.T, c *cache.Cache, hash string) ([]byte, bool) {
	t.Helper()

	rc, ok, err := c.Open(hash)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !ok {
		return nil, false
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return body, true
}

// waitFor polls until cond holds, since eviction runs in the background and
// must never happen inside a request.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func body(size int, seed byte) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = seed + byte(i%13)
	}
	return b
}

func TestAdmitThenOpenReturnsTheBlob(t *testing.T) {
	t.Parallel()

	c := newCache(t, cache.Config{})
	payload := []byte("a cached package")
	hash, accepted := admit(t, c, payload)
	if !accepted {
		t.Fatal("Admit refused a blob that fits")
	}

	got, ok := read(t, c, hash)
	if !ok {
		t.Fatal("Open missed a blob that was just admitted")
	}
	if string(got) != string(payload) {
		t.Fatalf("content = %q, want %q", got, payload)
	}
}

func TestOpenMissesWhatWasNeverAdmitted(t *testing.T) {
	t.Parallel()

	c := newCache(t, cache.Config{})
	if _, ok := read(t, c, digestOf([]byte("never stored"))); ok {
		t.Fatal("Open reported a blob that was never admitted")
	}
}

// The reader must be seekable so that http.ServeContent can answer a Range
// request without the handler doing anything special.
func TestOpenReturnsASeekableReader(t *testing.T) {
	t.Parallel()

	c := newCache(t, cache.Config{})
	payload := body(4096, 3)
	hash, _ := admit(t, c, payload)

	rc, ok, err := c.Open(hash)
	if err != nil || !ok {
		t.Fatalf("Open: %v, ok=%v", err, ok)
	}
	defer func() { _ = rc.Close() }()

	if _, err := rc.Seek(4000, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	tail, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(tail) != string(payload[4000:]) {
		t.Fatal("seeked read returned the wrong bytes")
	}
}

// A blob larger than the whole budget would evict everything else and then
// itself. Refusing it keeps the cache useful; the coalescer still serves it.
func TestAdmitRefusesABlobLargerThanTheBudget(t *testing.T) {
	t.Parallel()

	c := newCache(t, cache.Config{MaxSize: 1024})
	_, accepted := admit(t, c, body(4096, 1))
	if accepted {
		t.Fatal("Admit accepted a blob larger than max_size")
	}

	stats := c.Stats()
	if stats.Objects != 0 || stats.Bytes != 0 {
		t.Fatalf("a refused blob was accounted for: %+v", stats)
	}
	if n := tempFileCount(t, c.TempDir()); n != 0 {
		t.Fatalf("%d temp files left behind, want 0", n)
	}
}

// SPEC section 6: crossing 100% of the budget wakes a background goroutine
// that evicts down to 90%. Nothing is admitted after the trigger here, so the
// low watermark is observable; under a steady stream of admissions the cache
// legitimately sits anywhere between the two marks.
func TestEvictionRunsDownToTheLowWatermark(t *testing.T) {
	t.Parallel()

	const maxSize = 10 * 1024
	c := newCache(t, cache.Config{MaxSize: maxSize})

	var hashes []string
	for i := range 11 { // 11 KiB, just past the budget
		hash, accepted := admit(t, c, body(1024, byte(i)))
		if !accepted {
			t.Fatalf("blob %d was refused", i)
		}
		hashes = append(hashes, hash)
	}

	waitFor(t, "the cache to fall back to the low watermark", func() bool {
		return c.Stats().Bytes <= maxSize*9/10
	})

	if got := c.Stats().Evictions; got == 0 {
		t.Fatal("nothing was evicted")
	}
	if _, ok := read(t, c, hashes[len(hashes)-1]); !ok {
		t.Fatal("the newest blob was evicted")
	}
}

// Under a continuous stream of admissions the invariant is the budget itself:
// the cache may sit above the low watermark, but never above max_size for
// longer than one background pass.
func TestEvictionKeepsTheCacheWithinItsBudget(t *testing.T) {
	t.Parallel()

	const maxSize = 10 * 1024
	c := newCache(t, cache.Config{MaxSize: maxSize})

	for i := range 40 {
		if _, accepted := admit(t, c, body(1024, byte(i))); !accepted {
			t.Fatalf("blob %d was refused", i)
		}
	}

	waitFor(t, "the cache to settle within its budget", func() bool {
		return c.Stats().Bytes <= maxSize
	})
	if got := c.Stats().Evictions; got == 0 {
		t.Fatal("nothing was evicted")
	}
}

func TestEvictionRemovesTheLeastRecentlyUsedFirst(t *testing.T) {
	t.Parallel()

	const maxSize = 4 * 1024
	c := newCache(t, cache.Config{MaxSize: maxSize})

	first, _ := admit(t, c, body(1024, 1))
	second, _ := admit(t, c, body(1024, 2))
	third, _ := admit(t, c, body(1024, 3))

	// Touch the oldest so that the middle one becomes the eviction candidate.
	if _, ok := read(t, c, first); !ok {
		t.Fatal("the first blob is missing")
	}

	for i := range 4 {
		admit(t, c, body(1024, byte(10+i)))
	}

	waitFor(t, "eviction to settle", func() bool { return c.Stats().Bytes <= maxSize })

	if _, ok := read(t, c, second); ok {
		t.Fatal("the least recently used blob survived eviction")
	}
	_ = third
}

// SPEC section 6: pinned entries are never evicted and do not consume
// max_size, so a full cache cannot push metadata out.
func TestPinnedBlobsAreNeverEvictedAndDoNotConsumeTheBudget(t *testing.T) {
	t.Parallel()

	const maxSize = 4 * 1024
	c := newCache(t, cache.Config{MaxSize: maxSize, PinnedMaxSize: 8 * 1024})

	metadata := body(2048, 7)
	metaHash := digestOf(metadata)
	if err := c.SetPinned(map[string]int64{metaHash: int64(len(metadata))}); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}
	if _, accepted := admit(t, c, metadata); !accepted {
		t.Fatal("a pinned blob was refused")
	}

	stats := c.Stats()
	if stats.Bytes != 0 {
		t.Fatalf("pinned bytes leaked into the LRU budget: %+v", stats)
	}
	if stats.PinnedBytes != int64(len(metadata)) || stats.PinnedObjects != 1 {
		t.Fatalf("pinned accounting = %+v", stats)
	}

	// Now flood the unpinned segment well past the budget.
	for i := range 12 {
		admit(t, c, body(1024, byte(i)))
	}
	waitFor(t, "eviction to settle", func() bool { return c.Stats().Bytes <= maxSize })

	if _, ok := read(t, c, metaHash); !ok {
		t.Fatal("a pinned blob was evicted")
	}
	if got := c.Stats().PinnedBytes; got != int64(len(metadata)) {
		t.Fatalf("PinnedBytes = %d after eviction, want %d", got, len(metadata))
	}
}

// A pinned blob is bigger than the LRU budget by design; the budget does not
// apply to it.
func TestPinnedBlobsBypassTheBudgetCheck(t *testing.T) {
	t.Parallel()

	c := newCache(t, cache.Config{MaxSize: 1024, PinnedMaxSize: 1 << 20})
	payload := body(8192, 5)
	hash := digestOf(payload)
	if err := c.SetPinned(map[string]int64{hash: int64(len(payload))}); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}

	if _, accepted := admit(t, c, payload); !accepted {
		t.Fatal("a pinned blob larger than max_size was refused")
	}
}

// SPEC section 6: if the pinned set exceeds pinned_max_size, refuse rather
// than quietly filling the disk with data that never leaves.
func TestSetPinnedRefusesToExceedItsCap(t *testing.T) {
	t.Parallel()

	c := newCache(t, cache.Config{MaxSize: 1 << 20, PinnedMaxSize: 1024})
	err := c.SetPinned(map[string]int64{
		digestOf([]byte("a")): 800,
		digestOf([]byte("b")): 800,
	})
	if err == nil {
		t.Fatal("SetPinned accepted a pinned set larger than pinned_max_size")
	}
	if got := c.Stats().PinnedObjects; got != 0 {
		t.Fatalf("a rejected plan was partially applied: %d pinned objects", got)
	}
}

// A revision switch can pin a blob that is already sitting in the LRU segment.
func TestSetPinnedPromotesAnAlreadyCachedBlob(t *testing.T) {
	t.Parallel()

	c := newCache(t, cache.Config{MaxSize: 1 << 20, PinnedMaxSize: 1 << 20})
	payload := body(2048, 9)
	hash, _ := admit(t, c, payload)

	if got := c.Stats().Bytes; got != int64(len(payload)) {
		t.Fatalf("Bytes = %d before pinning, want %d", got, len(payload))
	}
	if err := c.SetPinned(map[string]int64{hash: int64(len(payload))}); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}

	stats := c.Stats()
	if stats.Bytes != 0 {
		t.Fatalf("Bytes = %d after promotion, want 0", stats.Bytes)
	}
	if stats.PinnedObjects != 1 || stats.PinnedBytes != int64(len(payload)) {
		t.Fatalf("pinned accounting after promotion = %+v", stats)
	}
	if _, ok := read(t, c, hash); !ok {
		t.Fatal("the promoted blob is no longer readable")
	}
}

// A revision that stops pinning a blob returns it to the LRU rather than
// leaking it out of every accounting.
func TestUnpinningReturnsABlobToTheLRU(t *testing.T) {
	t.Parallel()

	c := newCache(t, cache.Config{MaxSize: 1 << 20, PinnedMaxSize: 1 << 20})
	payload := body(2048, 11)
	hash := digestOf(payload)
	if err := c.SetPinned(map[string]int64{hash: int64(len(payload))}); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}
	admit(t, c, payload)

	if err := c.SetPinned(map[string]int64{}); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}

	stats := c.Stats()
	if stats.PinnedObjects != 0 || stats.PinnedBytes != 0 {
		t.Fatalf("pinned accounting after unpinning = %+v", stats)
	}
	if stats.Bytes != int64(len(payload)) {
		t.Fatalf("Bytes = %d after unpinning, want %d", stats.Bytes, len(payload))
	}
	if _, ok := read(t, c, hash); !ok {
		t.Fatal("the unpinned blob disappeared")
	}
}

// An edge restarts with a full cache directory; the accounting has to come
// back from what is on disk, since nothing else survives the process.
func TestNewRebuildsItsStateFromDisk(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := newCache(t, cache.Config{Dir: dir, MaxSize: 1 << 20, PinnedMaxSize: 1 << 20})

	payload := body(3000, 13)
	hash, _ := admit(t, first, payload)
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := newCache(t, cache.Config{Dir: dir, MaxSize: 1 << 20, PinnedMaxSize: 1 << 20})
	stats := second.Stats()
	if stats.Objects != 1 || stats.Bytes != int64(len(payload)) {
		t.Fatalf("state after restart = %+v, want 1 object of %d bytes", stats, len(payload))
	}
	got, ok := read(t, second, hash)
	if !ok || string(got) != string(payload) {
		t.Fatal("a blob did not survive the restart")
	}
}

// Leftover temp files from a process that died mid-download are not cache
// entries and must not be counted or served.
func TestNewClearsStaleTempFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := newCache(t, cache.Config{Dir: dir})
	stale := filepath.Join(first.TempDir(), "blob-interrupted.part")
	if err := os.WriteFile(stale, []byte("half a download"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := newCache(t, cache.Config{Dir: dir})
	if n := tempFileCount(t, second.TempDir()); n != 0 {
		t.Fatalf("%d stale temp files survived startup, want 0", n)
	}
	if got := second.Stats().Objects; got != 0 {
		t.Fatalf("a temp file was counted as a cache entry: %d objects", got)
	}
}

func TestConcurrentAdmitAndOpenAreSafe(t *testing.T) {
	t.Parallel()

	c := newCache(t, cache.Config{MaxSize: 32 * 1024})

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := body(512, byte(i))
			hash := digestOf(payload)

			f, err := os.CreateTemp(c.TempDir(), "blob-*.part")
			if err != nil {
				t.Errorf("CreateTemp: %v", err)
				return
			}
			if _, err := f.Write(payload); err != nil {
				t.Errorf("write: %v", err)
				return
			}
			_ = f.Close()

			if _, err := c.Admit(hash, int64(len(payload)), f.Name()); err != nil {
				t.Errorf("Admit: %v", err)
				return
			}
			if rc, ok, err := c.Open(hash); err == nil && ok {
				_, _ = io.Copy(io.Discard, rc)
				_ = rc.Close()
			}
		}()
	}
	wg.Wait()
}

func TestNewRejectsAnImpossibleConfiguration(t *testing.T) {
	t.Parallel()

	cases := map[string]cache.Config{
		"no directory":     {MaxSize: 1024, PinnedMaxSize: 1024},
		"no budget":        {Dir: t.TempDir(), PinnedMaxSize: 1024},
		"negative budget":  {Dir: t.TempDir(), MaxSize: -1, PinnedMaxSize: 1024},
		"negative reserve": {Dir: t.TempDir(), MaxSize: 1024, PinnedMaxSize: 1024, TempReserve: -1},
		"negative pin cap": {Dir: t.TempDir(), MaxSize: 1024, PinnedMaxSize: -1},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := cache.New(cfg); err == nil {
				t.Fatalf("cache.New accepted a config with %s", name)
			}
		})
	}
}

func tempFileCount(t *testing.T, dir string) int {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	return len(list)
}
