package cache

import (
	"container/list"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Config describes the on-disk cache.
type Config struct {
	// Dir holds the cache. Blobs and in-flight temp files both live under it,
	// on the same filesystem, so that admission is a rename.
	Dir string
	// MaxSize is the LRU budget. It excludes pinned entries and in-flight
	// temp files.
	MaxSize int64
	// PinnedMaxSize caps the pinned segment. A plan that exceeds it is
	// refused rather than silently filling the disk with data that never
	// leaves.
	PinnedMaxSize int64
	// TempReserve is the headroom in-flight downloads need, outside MaxSize.
	// Twenty concurrent 90 MiB packages are 1.8 GiB in flight that must not
	// provoke an eviction.
	TempReserve int64
	Logger      *slog.Logger
}

// Stats is the cache's accounting. Pinned entries are counted apart because
// they do not consume the LRU budget.
type Stats struct {
	Objects       int
	Bytes         int64
	PinnedObjects int
	PinnedBytes   int64
	Evictions     int64
	TempBytes     int64
}

// evictionLowWatermark is the fraction of MaxSize a background eviction runs
// down to. Evicting exactly to the budget would make the next admission evict
// again, turning every request into an unlink.
const evictionLowWatermark = 9.0 / 10.0

const (
	blobsSubdir = "blobs"
	tempSubdir  = "tmp"
	shardLen    = 2
	sha256Len   = 64
)

// Cache is a size-capped LRU over content-addressed blobs on local disk.
//
// It satisfies the store the download coalescer admits into: Open answers a
// hit, Admit takes ownership of a verified temp file, and a refusal simply
// means the coalescer unlinks what it staged. The served bytes are the same
// either way.
type Cache struct {
	blobsDir string
	tempDir  string

	maxSize     int64
	pinnedMax   int64
	tempReserve int64
	log         *slog.Logger

	mu    sync.Mutex
	lru   *list.List // front is most recently used
	index map[string]*list.Element
	bytes int64
	// pinned is the planned set: what the current revisions say must never be
	// evicted, whether or not it has been downloaded yet.
	pinned map[string]int64
	// resident is the pinned subset actually on disk, which is what the
	// metrics and the readiness probe care about.
	resident    map[string]int64
	pinnedBytes int64

	evictions atomic.Int64

	wake      chan struct{}
	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// lruItem is one unpinned cache entry.
type lruItem struct {
	hash string
	size int64
}

// New opens or creates the cache, rebuilding its accounting from disk.
func New(cfg Config) (*Cache, error) {
	if cfg.Dir == "" {
		return nil, errors.New("cache: dir is required")
	}
	if cfg.MaxSize <= 0 {
		return nil, fmt.Errorf("cache: max_size must be positive, got %d", cfg.MaxSize)
	}
	if cfg.PinnedMaxSize < 0 {
		return nil, fmt.Errorf("cache: pinned_max_size cannot be negative, got %d", cfg.PinnedMaxSize)
	}
	if cfg.TempReserve < 0 {
		return nil, fmt.Errorf("cache: temp_reserve cannot be negative, got %d", cfg.TempReserve)
	}

	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	c := &Cache{
		blobsDir:    filepath.Join(cfg.Dir, blobsSubdir),
		tempDir:     filepath.Join(cfg.Dir, tempSubdir),
		maxSize:     cfg.MaxSize,
		pinnedMax:   cfg.PinnedMaxSize,
		tempReserve: cfg.TempReserve,
		log:         log,
		lru:         list.New(),
		index:       map[string]*list.Element{},
		pinned:      map[string]int64{},
		resident:    map[string]int64{},
		wake:        make(chan struct{}, 1),
		done:        make(chan struct{}),
	}

	for _, dir := range []string{c.blobsDir, c.tempDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("cache: %w", err)
		}
	}

	// A temp file is a download that died with its process. It is not a cache
	// entry, it is not addressable, and nothing will ever come back for it.
	if err := clearDir(c.tempDir); err != nil {
		return nil, err
	}

	if err := c.rebuild(); err != nil {
		return nil, err
	}
	c.checkDiskSpace(cfg.Dir)

	log.Info("cache opened",
		"dir", cfg.Dir, "objects", len(c.index), "bytes", c.bytes,
		"max_size", c.maxSize, "pinned_max_size", c.pinnedMax, "temp_reserve", c.tempReserve)

	c.wg.Add(1)
	go c.evictLoop()
	return c, nil
}

// TempDir is where in-flight downloads are staged. It sits on the same
// filesystem as the blobs so that admission is a rename, and its contents are
// outside the LRU budget.
func (c *Cache) TempDir() string { return c.tempDir }

// Open returns a reader for a cached blob and refreshes its LRU position.
func (c *Cache) Open(hash string) (io.ReadSeekCloser, bool, error) {
	c.mu.Lock()
	_, pinned := c.resident[hash]
	if !pinned {
		elem, ok := c.index[hash]
		if !ok {
			c.mu.Unlock()
			return nil, false, nil
		}
		c.lru.MoveToFront(elem)
	}
	path := c.pathFor(hash)
	c.mu.Unlock()

	// Opening happens outside the lock: it is the only part of a hit that
	// touches the filesystem, and readers must not queue behind each other.
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Evicted, or removed from under us. Drop the stale bookkeeping
			// and report a miss so the caller fetches it again.
			c.forget(hash)
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("cache: open %s: %w", hash, err)
	}
	return f, true, nil
}

// Admit takes ownership of a fully written, verified temp file.
//
// It reports false when the admission policy declines, in which case the
// caller removes the temp file. Declining changes nothing for the requester
// that triggered the download: it is served from the same temp file either
// way.
func (c *Cache) Admit(hash string, size int64, tmpPath string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	_, isPinned := c.pinned[hash]
	if !isPinned && size > c.maxSize {
		// A blob bigger than the whole budget would evict everything else and
		// then itself on the next admission.
		c.log.Debug("refusing a blob larger than the cache budget",
			"hash", hash, "size", size, "max_size", c.maxSize)
		return false, nil
	}

	if _, already := c.index[hash]; already {
		return true, nil
	}
	if _, already := c.resident[hash]; already {
		return true, nil
	}

	dest := c.pathFor(hash)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false, fmt.Errorf("cache: %w", err)
	}
	// Rename is atomic on the same filesystem, so a partially written blob is
	// never visible under its final name.
	if err := os.Rename(tmpPath, dest); err != nil {
		return false, fmt.Errorf("cache: admit %s: %w", hash, err)
	}

	if isPinned {
		c.resident[hash] = size
		c.pinnedBytes += size
		return true, nil
	}

	c.index[hash] = c.lru.PushFront(lruItem{hash: hash, size: size})
	c.bytes += size
	if c.bytes > c.maxSize {
		c.signalEvict()
	}
	return true, nil
}

// SetPinned installs the set of blobs that must never be evicted, replacing
// whatever the previous revision pinned.
//
// It is called on startup and on every revision switch, and it refuses a plan
// larger than pinned_max_size without applying any of it. A pattern wide
// enough to blow that cap has to fail fast and loudly rather than quietly
// filling the disk.
func (c *Cache) SetPinned(pinned map[string]int64) error {
	var total int64
	for _, size := range pinned {
		total += size
	}
	if total > c.pinnedMax {
		return fmt.Errorf("cache: pinned set is %d bytes across %d objects, over the %d byte cap; "+
			"narrow the pinned patterns or raise pinned_max_size", total, len(pinned), c.pinnedMax)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	next := maps.Clone(pinned)
	if next == nil {
		next = map[string]int64{}
	}

	// Blobs that stop being pinned go back to the LRU rather than falling out
	// of every accounting and leaking disk.
	for hash, size := range c.resident {
		if _, still := next[hash]; still {
			continue
		}
		delete(c.resident, hash)
		c.pinnedBytes -= size
		c.index[hash] = c.lru.PushFront(lruItem{hash: hash, size: size})
		c.bytes += size
	}

	// Blobs that are already cached and become pinned move the other way.
	for hash, size := range next {
		elem, ok := c.index[hash]
		if !ok {
			continue
		}
		item, _ := elem.Value.(lruItem)
		c.lru.Remove(elem)
		delete(c.index, hash)
		c.bytes -= item.size
		c.resident[hash] = item.size
		c.pinnedBytes += item.size
		_ = size
	}

	c.pinned = next
	if c.bytes > c.maxSize {
		c.signalEvict()
	}
	return nil
}

// Stats reports the current accounting. Pinned figures cover what is actually
// resident, which is what the readiness probe and the metrics need.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	stats := Stats{
		Objects:       len(c.index),
		Bytes:         c.bytes,
		PinnedObjects: len(c.resident),
		PinnedBytes:   c.pinnedBytes,
		Evictions:     c.evictions.Load(),
	}
	c.mu.Unlock()

	stats.TempBytes = c.tempBytes()
	return stats
}

// PinnedPlanned reports how much the current plan asks to pin, resident or
// not, which is what tells an operator a prefetch is still catching up.
func (c *Cache) PinnedPlanned() (objects int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, size := range c.pinned {
		bytes += size
	}
	return len(c.pinned), bytes
}

// MissingPinned lists pinned blobs that are not on disk yet.
func (c *Cache) MissingPinned() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	var missing []string
	for hash := range c.pinned {
		if _, ok := c.resident[hash]; !ok {
			missing = append(missing, hash)
		}
	}
	slices.Sort(missing)
	return missing
}

// Close stops the background evictor.
func (c *Cache) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		c.wg.Wait()
	})
	return nil
}

func (c *Cache) pathFor(hash string) string {
	return filepath.Join(c.blobsDir, hash[:shardLen], hash[shardLen:2*shardLen], hash)
}

func (c *Cache) forget(hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.index[hash]; ok {
		item, _ := elem.Value.(lruItem)
		c.lru.Remove(elem)
		delete(c.index, hash)
		c.bytes -= item.size
	}
	if size, ok := c.resident[hash]; ok {
		delete(c.resident, hash)
		c.pinnedBytes -= size
	}
}

func (c *Cache) signalEvict() {
	select {
	case c.wake <- struct{}{}:
	default: // an eviction pass is already pending
	}
}

func (c *Cache) evictLoop() {
	defer c.wg.Done()
	for {
		select {
		case <-c.done:
			return
		case <-c.wake:
			c.evictOnce()
		}
	}
}

// evictOnce brings the unpinned segment back under the low watermark.
//
// Eviction never runs inside a request. Unlinking a 90 MiB file in the middle
// of an HTTP response is exactly what this exists to avoid, so the victims are
// chosen under the lock and removed from disk outside it.
func (c *Cache) evictOnce() {
	target := int64(float64(c.maxSize) * evictionLowWatermark)

	c.mu.Lock()
	if c.bytes <= c.maxSize {
		c.mu.Unlock()
		return
	}

	var victims []string
	for c.bytes > target {
		elem := c.lru.Back()
		if elem == nil {
			break
		}
		item, _ := elem.Value.(lruItem)
		c.lru.Remove(elem)
		delete(c.index, item.hash)
		c.bytes -= item.size
		victims = append(victims, c.pathFor(item.hash))
	}
	remaining := c.bytes
	// The counter moves with the accounting it describes. Bumping it after the
	// unlinks below would let an observer see a cache back inside its budget
	// while still reporting that nothing was evicted.
	c.evictions.Add(int64(len(victims)))
	c.mu.Unlock()

	for _, path := range victims {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			c.log.Warn("could not remove an evicted blob", "path", path, "error", err)
		}
	}
	if len(victims) > 0 {
		c.log.Info("evicted blobs", "count", len(victims), "bytes_now", remaining, "max_size", c.maxSize)
	}
}

// rebuild reconstructs the LRU from what is on disk. Nothing else survives a
// restart, and an edge that forgot its cache would re-download everything.
func (c *Cache) rebuild() error {
	type found struct {
		hash    string
		size    int64
		modTime time.Time
	}
	var entries []found

	err := filepath.WalkDir(c.blobsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		name := d.Name()
		if !isSHA256Hex(name) || path != c.pathFor(name) {
			// Not something this cache wrote. Leaving it would consume disk
			// that no accounting knows about.
			c.log.Warn("removing an unrecognised file from the cache directory", "path", path)
			if rmErr := os.Remove(path); rmErr != nil {
				c.log.Warn("could not remove it", "path", path, "error", rmErr)
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		entries = append(entries, found{hash: name, size: info.Size(), modTime: info.ModTime()})
		return nil
	})
	if err != nil {
		return fmt.Errorf("cache: scan %s: %w", c.blobsDir, err)
	}

	// Modification time is the only ordering a restart can recover. It is a
	// coarse stand-in for recency, and it only matters until traffic warms the
	// real ordering back up.
	slices.SortFunc(entries, func(a, b found) int { return a.modTime.Compare(b.modTime) })
	for _, e := range entries {
		c.index[e.hash] = c.lru.PushFront(lruItem{hash: e.hash, size: e.size})
		c.bytes += e.size
	}
	return nil
}

func (c *Cache) tempBytes() int64 {
	list, err := os.ReadDir(c.tempDir)
	if err != nil {
		return 0
	}
	var total int64
	for _, entry := range list {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total
}

// checkDiskSpace warns when the volume cannot hold what the configuration
// promises. It warns rather than refuses: a volume can grow, and an edge that
// will not start is worse than one that runs tight.
func (c *Cache) checkDiskSpace(dir string) {
	available, ok := availableBytes(dir)
	if !ok {
		return
	}
	needed := c.maxSize + c.pinnedMax + c.tempReserve
	if available < needed {
		c.log.Warn("the cache volume is smaller than the configuration needs",
			"available", available, "needed", needed,
			"max_size", c.maxSize, "pinned_max_size", c.pinnedMax, "temp_reserve", c.tempReserve)
	}
}

func clearDir(dir string) error {
	list, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	for _, entry := range list {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return fmt.Errorf("cache: clear %s: %w", dir, err)
		}
	}
	return nil
}

func isSHA256Hex(s string) bool {
	if len(s) != sha256Len {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
