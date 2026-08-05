package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nledez/aquifer/internal/blobstore"
	"github.com/nledez/aquifer/internal/cache"
	"github.com/nledez/aquifer/internal/debian"
	"github.com/nledez/aquifer/internal/fetch"
	"github.com/nledez/aquifer/internal/manifest"
)

// Config assembles an edge.
type Config struct {
	Store     blobstore.Store
	Cache     *cache.Cache
	Coalescer *fetch.Coalescer
	Selector  *cache.Selector
	Routes    []Route

	// PollInterval is how often each repo's ref is checked. Zero uses
	// DefaultPollInterval.
	PollInterval time.Duration
	// WindowSize is how many revisions of each repo stay resolvable.
	WindowSize int
	// PrefetchConcurrency bounds background downloads after a switch.
	PrefetchConcurrency int

	// Authorizer gates every request. Nil allows everything, which is the
	// documented default: there is no authentication, only a seam for one.
	Authorizer Authorizer
	Logger     *slog.Logger
}

const (
	// DefaultPollInterval is how often a ref is polled, with If-None-Match so
	// that an unchanged ref costs nothing.
	DefaultPollInterval = 15 * time.Second

	defaultPrefetchConcurrency = 4
)

// Server serves apt clients from the local cache.
type Server struct {
	store     blobstore.Store
	cache     *cache.Cache
	coalescer *fetch.Coalescer
	selector  *cache.Selector
	router    *Router
	auth      Authorizer
	log       *slog.Logger

	pollInterval        time.Duration
	prefetchConcurrency int

	repos map[string]*repoState
	names []string

	// plans holds each repo's pinned set. The cache takes one set for the
	// whole process, so a change in any repo re-derives the union.
	mu    sync.Mutex
	plans map[string]cache.Plan

	metrics *metrics

	cancel   context.CancelFunc
	wg       sync.WaitGroup
	closeOne sync.Once
}

// repoState is one repo's live view.
type repoState struct {
	name   string
	window *manifest.Window

	mu         sync.Mutex
	revision   string
	etag       string
	createdAt  time.Time
	validUntil map[string]time.Time
}

type repoSnapshot struct {
	revision   string
	createdAt  time.Time
	validUntil map[string]time.Time
}

func (r *repoState) snapshot() repoSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return repoSnapshot{
		revision:   r.revision,
		createdAt:  r.createdAt,
		validUntil: maps(r.validUntil),
	}
}

func maps(in map[string]time.Time) map[string]time.Time {
	out := make(map[string]time.Time, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// New assembles a server without contacting object storage.
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("server: store is required")
	case cfg.Cache == nil:
		return nil, errors.New("server: cache is required")
	case cfg.Coalescer == nil:
		return nil, errors.New("server: coalescer is required")
	case cfg.Selector == nil:
		return nil, errors.New("server: selector is required")
	}

	router, err := NewRouter(cfg.Routes)
	if err != nil {
		return nil, err
	}

	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	interval := cfg.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	prefetch := cfg.PrefetchConcurrency
	if prefetch <= 0 {
		prefetch = defaultPrefetchConcurrency
	}
	auth := cfg.Authorizer
	if auth == nil {
		auth = AllowAll{}
	}

	s := &Server{
		store:               cfg.Store,
		cache:               cfg.Cache,
		coalescer:           cfg.Coalescer,
		selector:            cfg.Selector,
		router:              router,
		auth:                auth,
		log:                 log,
		pollInterval:        interval,
		prefetchConcurrency: prefetch,
		repos:               map[string]*repoState{},
		plans:               map[string]cache.Plan{},
	}

	for _, route := range router.Routes() {
		if _, ok := s.repos[route.Repo]; ok {
			continue
		}
		s.repos[route.Repo] = &repoState{
			name:       route.Repo,
			window:     manifest.NewWindow(cfg.WindowSize),
			validUntil: map[string]time.Time{},
		}
		s.names = append(s.names, route.Repo)
	}
	slices.Sort(s.names)

	s.metrics = newMetrics(s)
	return s, nil
}

// Start loads every repo's current revision and then keeps polling.
//
// The initial load is synchronous and fatal on failure: an edge that came up
// serving 404s because a manifest would not load is worse than one that
// refuses to come up at all, and a pinned set over its cap has to be caught
// here rather than after the disk fills.
func (s *Server) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	for _, name := range s.names {
		if _, err := s.refresh(ctx, s.repos[name]); err != nil {
			cancel()
			return fmt.Errorf("server: initial load of %s: %w", name, err)
		}
	}

	for _, name := range s.names {
		rs := s.repos[name]
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.poll(ctx, rs)
		}()
	}
	return nil
}

// Close stops the pollers.
func (s *Server) Close() error {
	s.closeOne.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		s.wg.Wait()
	})
	return nil
}

// CurrentRevision reports the revision a repo is serving.
func (s *Server) CurrentRevision(repo string) string {
	rs, ok := s.repos[repo]
	if !ok {
		return ""
	}
	return rs.snapshot().revision
}

func (s *Server) repoStates() []*repoState {
	out := make([]*repoState, 0, len(s.names))
	for _, name := range s.names {
		out = append(out, s.repos[name])
	}
	return out
}

func (s *Server) poll(ctx context.Context, rs *repoState) {
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			switched, err := s.refresh(ctx, rs)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				// A failed refresh leaves the previous revision in place. The
				// edge keeps serving what it has rather than going dark.
				s.log.Error("could not refresh a repo", "repo", rs.name, "error", err)
				continue
			}
			if switched {
				s.log.Info("switched revision", "repo", rs.name, "revision", rs.snapshot().revision)
			}
		}
	}
}

// refresh polls a ref and adopts a new revision if there is one.
func (s *Server) refresh(ctx context.Context, rs *repoState) (bool, error) {
	rs.mu.Lock()
	etag := rs.etag
	current := rs.revision
	rs.mu.Unlock()

	ref, err := s.store.GetRef(ctx, rs.name, etag)
	if errors.Is(err, blobstore.ErrNotModified) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if ref.Revision == current {
		rs.mu.Lock()
		rs.etag = ref.ETag
		rs.mu.Unlock()
		return false, nil
	}

	m, err := s.loadManifest(ctx, rs.name, ref.Revision)
	if err != nil {
		return false, err
	}

	plan, stats := s.selector.Select(m.All())
	for _, pattern := range stats.UnmatchedPatterns {
		s.log.Warn("cache pattern matches nothing in this revision",
			"repo", rs.name, "revision", ref.Revision, "pattern", pattern)
	}
	s.log.Info("revision selection",
		"repo", rs.name, "revision", ref.Revision, "entries", m.Len(),
		"pinned_objects", stats.PinnedObjects, "pinned_bytes", stats.PinnedBytes,
		"prefetch_objects", stats.PrefetchObjects, "prefetch_bytes", stats.PrefetchBytes)

	// The pinned set is installed before the revision is adopted. If it does
	// not fit, the edge keeps serving the previous revision instead of
	// half-applying this one.
	if err := s.applyPlan(rs.name, plan); err != nil {
		return false, err
	}

	rs.window.Push(m)
	rs.mu.Lock()
	rs.revision = ref.Revision
	rs.etag = ref.ETag
	rs.createdAt = m.CreatedAt
	rs.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.prefetch(ctx, rs, m, plan)
	}()
	return true, nil
}

func (s *Server) loadManifest(ctx context.Context, repo, revision string) (*manifest.Manifest, error) {
	rc, err := s.store.GetManifest(ctx, repo, revision)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	m, err := manifest.Read(rc)
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", revision, err)
	}
	return m, nil
}

// applyPlan re-derives the union of every repo's pinned set and installs it.
func (s *Server) applyPlan(repo string, plan cache.Plan) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	union := map[string]int64{}
	for name, existing := range s.plans {
		if name == repo {
			continue
		}
		for hash, size := range existing.Pinned {
			union[hash] = size
		}
	}
	for hash, size := range plan.Pinned {
		union[hash] = size
	}

	if err := s.cache.SetPinned(union); err != nil {
		return err
	}
	s.plans[repo] = plan
	return nil
}

// prefetch downloads what the new revision calls for, in the background.
//
// The requests go through the coalescer like any other, so a client asking for
// the same blob mid-prefetch joins that download instead of starting a second.
func (s *Server) prefetch(ctx context.Context, rs *repoState, m *manifest.Manifest, plan cache.Plan) {
	hashes := make([]string, 0, len(plan.Prefetch))
	for hash := range plan.Prefetch {
		hashes = append(hashes, hash)
	}
	slices.Sort(hashes) // deterministic order, so logs of two edges compare

	work := make(chan string)
	var wg sync.WaitGroup
	for range s.prefetchConcurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for hash := range work {
				s.prefetchOne(ctx, plan.Prefetch[hash], hash)
			}
		}()
	}
	for _, hash := range hashes {
		select {
		case work <- hash:
		case <-ctx.Done():
		}
	}
	close(work)
	wg.Wait()

	if ctx.Err() != nil {
		return
	}
	s.readValidUntil(ctx, rs, m)
}

func (s *Server) prefetchOne(ctx context.Context, size int64, hash string) {
	if rc, ok, err := s.cache.Open(hash); err == nil && ok {
		_ = rc.Close()
		return
	}

	rc, err := s.coalescer.Fetch(ctx, hash, size)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("prefetch failed", "hash", hash, "error", err)
		}
		return
	}
	defer func() { _ = rc.Close() }()

	if _, err := io.Copy(io.Discard, rc); err != nil && ctx.Err() == nil {
		s.log.Warn("prefetch failed while streaming", "hash", hash, "error", err)
	}
}

// readValidUntil parses the Release files of a revision so that freshness can
// be alerted on.
//
// It reads only what the cache already holds. Downloading a Release the
// operator did not ask to cache, purely to compute a metric, would ignore the
// policy they configured and put an unrequested fetch on every revision
// switch. The shipped configuration pins metadata, so the metric is there for
// anyone who wants it.
func (s *Server) readValidUntil(_ context.Context, rs *repoState, m *manifest.Manifest) {
	found := map[string]time.Time{}

	for entry := range m.All() {
		base := path.Base(entry.Path)
		if base != "InRelease" && base != "Release" {
			continue
		}
		if !manifest.IsMetadata(entry.Path) {
			continue
		}

		rel, ok := s.parseCachedRelease(entry.SHA256)
		if !ok {
			s.log.Debug("skipping a Release file that is not cached; pin or prefetch it to get "+
				"aquifer_release_valid_until_seconds for this suite",
				"repo", rs.name, "path", entry.Path)
			continue
		}
		suite := rel.Suite
		if suite == "" {
			suite = rel.Codename
		}
		if suite == "" {
			continue
		}
		// InRelease and Release describe the same suite; either will do, and
		// whichever parses is enough.
		if _, seen := found[suite]; !seen || !rel.ValidUntil.IsZero() {
			found[suite] = rel.ValidUntil
		}
	}

	rs.mu.Lock()
	rs.validUntil = found
	rs.mu.Unlock()
}

// parseCachedRelease parses a Release file if, and only if, it is already on
// disk. A miss is not an error: it means the operator did not ask for this
// blob to be cached.
func (s *Server) parseCachedRelease(hash string) (*debian.Release, bool) {
	rc, ok, err := s.cache.Open(hash)
	if err != nil || !ok {
		return nil, false
	}
	defer func() { _ = rc.Close() }()

	rel, err := debian.ParseRelease(rc)
	if err != nil {
		s.log.Warn("a cached Release file did not parse", "hash", hash, "error", err)
		return nil, false
	}
	return rel, true
}

// readiness is what /readyz reports.
type readiness struct {
	Ready  bool            `json:"ready"`
	Repos  []repoReadiness `json:"repos"`
	Cache  cacheReadiness  `json:"cache"`
	Reason string          `json:"reason,omitempty"`
}

type repoReadiness struct {
	Repo       string            `json:"repo"`
	Revision   string            `json:"revision"`
	AgeSeconds float64           `json:"age_seconds"`
	ValidUntil map[string]string `json:"valid_until,omitempty"`
	Expired    []string          `json:"expired_suites,omitempty"`
}

type cacheReadiness struct {
	Bytes         int64 `json:"bytes"`
	Objects       int   `json:"objects"`
	PinnedBytes   int64 `json:"pinned_bytes"`
	PinnedObjects int   `json:"pinned_objects"`
	PinnedMissing int   `json:"pinned_missing"`
}

// readiness reports whether the edge can serve: every manifest loaded, every
// pinned blob present, no suite past its Valid-Until.
func (s *Server) readiness() readiness {
	now := time.Now()
	stats := s.cache.Stats()
	missing := s.cache.MissingPinned()

	out := readiness{
		Ready: true,
		Cache: cacheReadiness{
			Bytes:         stats.Bytes,
			Objects:       stats.Objects,
			PinnedBytes:   stats.PinnedBytes,
			PinnedObjects: stats.PinnedObjects,
			PinnedMissing: len(missing),
		},
	}
	var reasons []string
	if len(missing) > 0 {
		out.Ready = false
		reasons = append(reasons, fmt.Sprintf("%d pinned blob(s) not yet on disk", len(missing)))
	}

	for _, rs := range s.repoStates() {
		snap := rs.snapshot()
		repo := repoReadiness{Repo: rs.name, Revision: snap.revision}
		if snap.revision == "" {
			out.Ready = false
			reasons = append(reasons, rs.name+" has no revision loaded")
			out.Repos = append(out.Repos, repo)
			continue
		}
		repo.AgeSeconds = now.Sub(snap.createdAt).Seconds()

		for suite, validUntil := range snap.validUntil {
			if validUntil.IsZero() {
				continue
			}
			if repo.ValidUntil == nil {
				repo.ValidUntil = map[string]string{}
			}
			repo.ValidUntil[suite] = validUntil.UTC().Format(time.RFC3339)
			if now.After(validUntil) {
				repo.Expired = append(repo.Expired, suite)
				out.Ready = false
				reasons = append(reasons, rs.name+" suite "+suite+" is past its Valid-Until")
			}
		}
		slices.Sort(repo.Expired)
		out.Repos = append(out.Repos, repo)
	}

	out.Reason = strings.Join(reasons, "; ")
	return out
}
