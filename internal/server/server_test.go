package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nledez/aquifer/internal/blobstore"
	"github.com/nledez/aquifer/internal/blobstore/blobstoretest"
	"github.com/nledez/aquifer/internal/cache"
	"github.com/nledez/aquifer/internal/fetch"
	"github.com/nledez/aquifer/internal/manifest"
	"github.com/nledez/aquifer/internal/server"
)

// --- fixtures ---------------------------------------------------------------

func sha256Of(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// countingStore reports how many blob bodies were pulled from object storage,
// which is what proves a request never reached the backend.
type countingStore struct {
	blobstore.Store
	gets atomic.Int64
}

func (c *countingStore) GetBlob(ctx context.Context, hash string) (io.ReadCloser, error) {
	c.gets.Add(1)
	return c.Store.GetBlob(ctx, hash)
}

// publishRevision writes blobs and a manifest, then moves the ref, the way the
// master does.
func publishRevision(t *testing.T, store blobstore.Store, repo, prefix string, files map[string][]byte) string {
	t.Helper()

	ctx := t.Context()
	entries := make([]manifest.Entry, 0, len(files))
	for name, payload := range files {
		hash := sha256Of(payload)
		if err := store.PutBlob(ctx, hash, bytes.NewReader(payload), int64(len(payload))); err != nil {
			t.Fatalf("PutBlob: %v", err)
		}
		path := name
		if prefix != "" {
			path = prefix + "/" + name
		}
		entries = append(entries, manifest.Entry{Path: path, SHA256: hash, Size: int64(len(payload))})
	}

	revision := manifest.NewRevision(time.Now())
	var buf bytes.Buffer
	err := manifest.Write(&buf, manifest.Meta{
		Repo: repo, Revision: revision, CreatedAt: time.Now().UTC().Truncate(time.Second),
	}, entries)
	if err != nil {
		t.Fatalf("manifest.Write: %v", err)
	}
	if err := store.PutManifest(ctx, repo, revision, bytes.NewReader(buf.Bytes()), int64(buf.Len())); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	if err := store.SetRef(ctx, repo, revision); err != nil {
		t.Fatalf("SetRef: %v", err)
	}
	return revision
}

type harness struct {
	store  *countingStore
	cache  *cache.Cache
	server *server.Server
	client *http.Client
	url    string
	admin  *httptest.Server
}

type harnessOptions struct {
	routes        []server.Route
	pinned        []string
	prefetch      []string
	maxSize       int64
	pinnedMaxSize int64
	pollInterval  time.Duration
}

func newHarness(t *testing.T, store *countingStore, opts harnessOptions) *harness {
	t.Helper()

	if opts.maxSize == 0 {
		opts.maxSize = 1 << 20
	}
	if opts.pinnedMaxSize == 0 {
		opts.pinnedMaxSize = 1 << 20
	}
	if opts.pollInterval == 0 {
		opts.pollInterval = 10 * time.Millisecond
	}

	c, err := cache.New(cache.Config{
		Dir:           t.TempDir(),
		MaxSize:       opts.maxSize,
		PinnedMaxSize: opts.pinnedMaxSize,
	})
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	coalescer, err := fetch.New(fetch.Config{
		Source:  blobstore.BlobSource{Store: store},
		Store:   c,
		TempDir: c.TempDir(),
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}

	sel, err := cache.NewSelector(opts.pinned, opts.prefetch)
	if err != nil {
		t.Fatalf("cache.NewSelector: %v", err)
	}

	srv, err := server.New(server.Config{
		Store:        store,
		Cache:        c,
		Coalescer:    coalescer,
		Selector:     sel,
		Routes:       opts.routes,
		PollInterval: opts.pollInterval,
		WindowSize:   3,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server.Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	front := httptest.NewServer(srv.Handler())
	t.Cleanup(front.Close)
	admin := httptest.NewServer(srv.AdminHandler())
	t.Cleanup(admin.Close)

	return &harness{
		store:  store,
		cache:  c,
		server: srv,
		client: front.Client(),
		url:    front.URL,
		admin:  admin,
	}
}

func (h *harness) do(t *testing.T, method, path string, headers map[string]string) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, h.url+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func bodyOf(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

func waitUntil(t *testing.T, what string, cond func() bool) {
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

func defaultRoutes() []server.Route {
	return []server.Route{{Prefix: "debian/bookworm", Repo: "debian/bookworm"}}
}

func defaultFiles() map[string][]byte {
	return map[string][]byte{
		"dists/bookworm/InRelease":                   []byte("Suite: bookworm\nSHA256:\n"),
		"dists/bookworm/main/binary-amd64/Packages":  []byte("Package: nginx\n"),
		"pool/main/n/nginx/nginx_1.24.0-1_amd64.deb": []byte("the nginx package payload"),
		"pool/main/a/apt/apt_2.6.1_amd64.deb":        []byte("the apt package payload"),
	}
}

// --- serving ----------------------------------------------------------------

func TestServesAPoolFileWithItsDigestAsETag(t *testing.T) {
	t.Parallel()

	store := &countingStore{Store: blobstoretest.NewMem()}
	files := defaultFiles()
	publishRevision(t, store, "debian/bookworm", "debian/bookworm", files)
	h := newHarness(t, store, harnessOptions{routes: defaultRoutes()})

	want := files["pool/main/n/nginx/nginx_1.24.0-1_amd64.deb"]
	resp := h.do(t, http.MethodGet, "/debian/bookworm/pool/main/n/nginx/nginx_1.24.0-1_amd64.deb", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := bodyOf(t, resp); !bytes.Equal(got, want) {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if got, expect := resp.Header.Get("ETag"), `"`+sha256Of(want)+`"`; got != expect {
		t.Fatalf("ETag = %q, want %q", got, expect)
	}
	if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q, want bytes", got)
	}
}

func TestUnknownPathsAre404(t *testing.T) {
	t.Parallel()

	store := &countingStore{Store: blobstoretest.NewMem()}
	publishRevision(t, store, "debian/bookworm", "debian/bookworm", defaultFiles())
	h := newHarness(t, store, harnessOptions{routes: defaultRoutes()})

	for _, path := range []string{
		"/debian/bookworm/pool/main/z/zzz/absent.deb",
		"/ubuntu/noble/pool/x.deb", // no route at all
	} {
		resp := h.do(t, http.MethodGet, path, nil)
		_ = bodyOf(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s: status = %d, want 404", path, resp.StatusCode)
		}
	}
}

// A HEAD is answered from the manifest. Downloading 90 MiB to report a size
// the manifest already states would be absurd.
func TestHeadIsAnsweredFromTheManifestWithoutFetching(t *testing.T) {
	t.Parallel()

	store := &countingStore{Store: blobstoretest.NewMem()}
	files := defaultFiles()
	publishRevision(t, store, "debian/bookworm", "debian/bookworm", files)
	h := newHarness(t, store, harnessOptions{routes: defaultRoutes()})

	before := store.gets.Load()
	resp := h.do(t, http.MethodHead, "/debian/bookworm/pool/main/n/nginx/nginx_1.24.0-1_amd64.deb", nil)
	if body := bodyOf(t, resp); len(body) != 0 {
		t.Fatalf("HEAD returned a body of %d bytes", len(body))
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	want := int64(len(files["pool/main/n/nginx/nginx_1.24.0-1_amd64.deb"]))
	if resp.ContentLength != want {
		t.Fatalf("Content-Length = %d, want %d", resp.ContentLength, want)
	}
	if got := store.gets.Load(); got != before {
		t.Fatalf("HEAD pulled %d blob(s) from the backend", got-before)
	}
}

// apt revalidates constantly. A matching ETag must cost nothing.
func TestMatchingETagIs304WithoutFetching(t *testing.T) {
	t.Parallel()

	store := &countingStore{Store: blobstoretest.NewMem()}
	files := defaultFiles()
	publishRevision(t, store, "debian/bookworm", "debian/bookworm", files)
	h := newHarness(t, store, harnessOptions{routes: defaultRoutes()})

	etag := `"` + sha256Of(files["pool/main/a/apt/apt_2.6.1_amd64.deb"]) + `"`
	before := store.gets.Load()
	resp := h.do(t, http.MethodGet, "/debian/bookworm/pool/main/a/apt/apt_2.6.1_amd64.deb",
		map[string]string{"If-None-Match": etag})
	_ = bodyOf(t, resp)

	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", resp.StatusCode)
	}
	if got := store.gets.Load(); got != before {
		t.Fatalf("a 304 pulled %d blob(s) from the backend", got-before)
	}
}

// SPEC section 5: Range support is not optional with 90 MiB objects.
func TestRangeRequestsAreServed(t *testing.T) {
	t.Parallel()

	store := &countingStore{Store: blobstoretest.NewMem()}
	files := defaultFiles()
	payload := bytes.Repeat([]byte("0123456789"), 500)
	files["pool/main/b/big/big_1.0_amd64.deb"] = payload
	publishRevision(t, store, "debian/bookworm", "debian/bookworm", files)
	h := newHarness(t, store, harnessOptions{routes: defaultRoutes()})

	resp := h.do(t, http.MethodGet, "/debian/bookworm/pool/main/b/big/big_1.0_amd64.deb",
		map[string]string{"Range": "bytes=100-199"})
	body := bodyOf(t, resp)

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if !bytes.Equal(body, payload[100:200]) {
		t.Fatalf("range body = %q", body)
	}
	if got := resp.Header.Get("Content-Range"); got != fmt.Sprintf("bytes 100-199/%d", len(payload)) {
		t.Fatalf("Content-Range = %q", got)
	}
}

// SPEC section 5: by-hash paths resolve straight from the digest in the URL.
func TestByHashPathsResolveDirectly(t *testing.T) {
	t.Parallel()

	store := &countingStore{Store: blobstoretest.NewMem()}
	files := defaultFiles()
	publishRevision(t, store, "debian/bookworm", "debian/bookworm", files)
	h := newHarness(t, store, harnessOptions{routes: defaultRoutes()})

	want := files["dists/bookworm/main/binary-amd64/Packages"]
	path := "/debian/bookworm/dists/bookworm/main/binary-amd64/by-hash/SHA256/" + sha256Of(want)
	resp := h.do(t, http.MethodGet, path, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := bodyOf(t, resp); !bytes.Equal(got, want) {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// An unknown digest must not become a way to make the edge pull arbitrary
// objects out of storage on request.
func TestByHashRefusesADigestNoRevisionReferences(t *testing.T) {
	t.Parallel()

	store := &countingStore{Store: blobstoretest.NewMem()}
	publishRevision(t, store, "debian/bookworm", "debian/bookworm", defaultFiles())
	h := newHarness(t, store, harnessOptions{routes: defaultRoutes()})

	unknown := sha256Of([]byte("never published"))
	before := store.gets.Load()
	resp := h.do(t, http.MethodGet,
		"/debian/bookworm/dists/bookworm/main/binary-amd64/by-hash/SHA256/"+unknown, nil)
	_ = bodyOf(t, resp)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := store.gets.Load(); got != before {
		t.Fatalf("an unknown by-hash digest triggered %d backend fetch(es)", got-before)
	}
}

// --- revisions --------------------------------------------------------------

// SPEC section 5: pool paths resolve against the retained window, metadata
// against the current revision alone.
func TestRevisionSwitchKeepsPoolPathsAndRetiresMetadata(t *testing.T) {
	t.Parallel()

	store := &countingStore{Store: blobstoretest.NewMem()}
	first := defaultFiles()
	publishRevision(t, store, "debian/bookworm", "debian/bookworm", first)
	h := newHarness(t, store, harnessOptions{routes: defaultRoutes()})

	// The old package must keep resolving through the switch.
	resp := h.do(t, http.MethodGet, "/debian/bookworm/pool/main/n/nginx/nginx_1.24.0-1_amd64.deb", nil)
	_ = bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initial GET status = %d", resp.StatusCode)
	}

	second := map[string][]byte{
		"dists/bookworm/InRelease":                   []byte("Suite: bookworm\nrevision two\n"),
		"pool/main/n/nginx/nginx_1.26.0-1_amd64.deb": []byte("the newer nginx package"),
	}
	revision := publishRevision(t, store, "debian/bookworm", "debian/bookworm", second)

	waitUntil(t, "the edge to pick up the new revision", func() bool {
		return h.server.CurrentRevision("debian/bookworm") == revision
	})

	// The new package resolves.
	resp = h.do(t, http.MethodGet, "/debian/bookworm/pool/main/n/nginx/nginx_1.26.0-1_amd64.deb", nil)
	if got := bodyOf(t, resp); !bytes.Equal(got, second["pool/main/n/nginx/nginx_1.26.0-1_amd64.deb"]) {
		t.Fatalf("new package body = %q", got)
	}

	// The superseded package still resolves: a client that ran apt update
	// against another edge is mid-install.
	resp = h.do(t, http.MethodGet, "/debian/bookworm/pool/main/n/nginx/nginx_1.24.0-1_amd64.deb", nil)
	if got := bodyOf(t, resp); !bytes.Equal(got, first["pool/main/n/nginx/nginx_1.24.0-1_amd64.deb"]) {
		t.Fatalf("retained package body = %q", got)
	}

	// Metadata the new revision dropped must not be served from the old one.
	resp = h.do(t, http.MethodGet, "/debian/bookworm/dists/bookworm/main/binary-amd64/Packages", nil)
	_ = bodyOf(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stale metadata status = %d, want 404", resp.StatusCode)
	}

	// The current metadata is the new one.
	resp = h.do(t, http.MethodGet, "/debian/bookworm/dists/bookworm/InRelease", nil)
	if got := bodyOf(t, resp); !bytes.Equal(got, second["dists/bookworm/InRelease"]) {
		t.Fatalf("InRelease body = %q, want the current revision's", got)
	}
}

// SPEC section 6: pinned blobs are downloaded when a revision loads, not when
// someone asks for them.
func TestPinnedBlobsArePrefetchedOnLoad(t *testing.T) {
	t.Parallel()

	store := &countingStore{Store: blobstoretest.NewMem()}
	files := defaultFiles()
	publishRevision(t, store, "debian/bookworm", "debian/bookworm", files)
	h := newHarness(t, store, harnessOptions{
		routes: defaultRoutes(),
		pinned: []string{"**/dists/**"},
	})

	waitUntil(t, "the pinned set to become resident", func() bool {
		return h.cache.Stats().PinnedObjects == 2
	})

	stats := h.cache.Stats()
	var want int64
	for name, payload := range files {
		if strings.Contains(name, "dists/") {
			want += int64(len(payload))
		}
	}
	if stats.PinnedBytes != want {
		t.Fatalf("PinnedBytes = %d, want %d", stats.PinnedBytes, want)
	}

	// Serving pinned metadata must not touch the backend again.
	before := store.gets.Load()
	resp := h.do(t, http.MethodGet, "/debian/bookworm/dists/bookworm/InRelease", nil)
	_ = bodyOf(t, resp)
	if got := store.gets.Load(); got != before {
		t.Fatalf("a pinned blob was fetched again: %d extra GETs", got-before)
	}
}

// SPEC section 6: a pinned set over its cap must refuse to start rather than
// quietly fill the disk.
func TestStartRefusesAPinnedSetOverItsCap(t *testing.T) {
	t.Parallel()

	store := &countingStore{Store: blobstoretest.NewMem()}
	publishRevision(t, store, "debian/bookworm", "debian/bookworm", defaultFiles())

	c, err := cache.New(cache.Config{Dir: t.TempDir(), MaxSize: 1 << 20, PinnedMaxSize: 4})
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	defer func() { _ = c.Close() }()

	coalescer, err := fetch.New(fetch.Config{
		Source: blobstore.BlobSource{Store: store}, Store: c, TempDir: c.TempDir(),
	})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}
	sel, err := cache.NewSelector([]string{"**"}, nil)
	if err != nil {
		t.Fatalf("cache.NewSelector: %v", err)
	}

	srv, err := server.New(server.Config{
		Store: store, Cache: c, Coalescer: coalescer, Selector: sel,
		Routes: defaultRoutes(), PollInterval: time.Second, WindowSize: 3,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	defer func() { _ = srv.Close() }()

	if err := srv.Start(t.Context()); err == nil {
		t.Fatal("Start accepted a pinned set larger than pinned_max_size")
	}
}

// --- coalescing through the handler ------------------------------------------

// SPEC section 7, end to end: forty clients colliding on one uncached package
// must produce exactly one GET against object storage.
func TestConcurrentRequestsForOneBlobHitTheBackendOnce(t *testing.T) {
	t.Parallel()

	store := &countingStore{Store: blobstoretest.NewMem()}
	files := defaultFiles()
	payload := bytes.Repeat([]byte("a large package "), 4096)
	files["pool/main/b/big/big_1.0_amd64.deb"] = payload
	publishRevision(t, store, "debian/bookworm", "debian/bookworm", files)
	h := newHarness(t, store, harnessOptions{routes: defaultRoutes()})

	before := store.gets.Load()
	const clients = 40
	var wg sync.WaitGroup
	bodies := make([][]byte, clients)
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := h.do(t, http.MethodGet, "/debian/bookworm/pool/main/b/big/big_1.0_amd64.deb", nil)
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Errorf("client %d: %v", i, err)
				return
			}
			bodies[i] = body
		}()
	}
	wg.Wait()

	for i, body := range bodies {
		if !bytes.Equal(body, payload) {
			t.Fatalf("client %d got %d bytes, want %d", i, len(body), len(payload))
		}
	}
	if got := store.gets.Load() - before; got != 1 {
		t.Fatalf("%d backend GETs for one blob, want exactly 1", got)
	}
}

// --- admin ------------------------------------------------------------------

func TestHealthAndReadiness(t *testing.T) {
	t.Parallel()

	store := &countingStore{Store: blobstoretest.NewMem()}
	publishRevision(t, store, "debian/bookworm", "debian/bookworm", defaultFiles())
	h := newHarness(t, store, harnessOptions{
		routes: defaultRoutes(),
		pinned: []string{"**/dists/**"},
	})

	resp, err := h.admin.Client().Get(h.admin.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	_ = bodyOf(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}

	waitUntil(t, "readiness", func() bool {
		r, err := h.admin.Client().Get(h.admin.URL + "/readyz")
		if err != nil {
			return false
		}
		_ = r.Body.Close()
		return r.StatusCode == http.StatusOK
	})

	r, err := h.admin.Client().Get(h.admin.URL + "/readyz")
	if err != nil {
		t.Fatalf("readyz: %v", err)
	}
	body := bodyOf(t, r)
	if !strings.Contains(string(body), "debian/bookworm") {
		t.Fatalf("readyz body does not mention the repo: %s", body)
	}
}

func TestMetricsExposeTheSpecifiedSeries(t *testing.T) {
	t.Parallel()

	store := &countingStore{Store: blobstoretest.NewMem()}
	publishRevision(t, store, "debian/bookworm", "debian/bookworm", defaultFiles())
	h := newHarness(t, store, harnessOptions{
		routes: defaultRoutes(),
		pinned: []string{"**/dists/**"},
	})

	// Generate one hit and one miss.
	resp := h.do(t, http.MethodGet, "/debian/bookworm/pool/main/n/nginx/nginx_1.24.0-1_amd64.deb", nil)
	_ = bodyOf(t, resp)
	resp = h.do(t, http.MethodGet, "/debian/bookworm/pool/main/n/nginx/nginx_1.24.0-1_amd64.deb", nil)
	_ = bodyOf(t, resp)

	waitUntil(t, "the pinned set to become resident", func() bool {
		return h.cache.Stats().PinnedObjects == 2
	})

	metrics, err := h.admin.Client().Get(h.admin.URL + "/metrics")
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	body := string(bodyOf(t, metrics))

	for _, name := range []string{
		"aquifer_cache_requests_total",
		"aquifer_fetch_coalesced_readers_total",
		"aquifer_fetch_inflight",
		"aquifer_cache_bytes",
		"aquifer_cache_evictions_total",
		"aquifer_cache_pinned_bytes",
		"aquifer_cache_pinned_objects",
		"aquifer_manifest_revision_info",
		"aquifer_manifest_age_seconds",
		"aquifer_request_duration_seconds",
	} {
		if !strings.Contains(body, name) {
			t.Fatalf("metric %s is missing from /metrics", name)
		}
	}

	// The class breakdown is what makes the hit ratio actionable.
	if !strings.Contains(body, `class="pool"`) {
		t.Fatalf("requests are not broken down by class:\n%s", body)
	}
}

// --- authorization seam -------------------------------------------------------

// SPEC section 8: no authentication, but a clean seam for one.
func TestAuthorizerCanRefuseARequest(t *testing.T) {
	t.Parallel()

	store := &countingStore{Store: blobstoretest.NewMem()}
	publishRevision(t, store, "debian/bookworm", "debian/bookworm", defaultFiles())

	c, err := cache.New(cache.Config{Dir: t.TempDir(), MaxSize: 1 << 20, PinnedMaxSize: 1 << 20})
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	defer func() { _ = c.Close() }()
	coalescer, err := fetch.New(fetch.Config{
		Source: blobstore.BlobSource{Store: store}, Store: c, TempDir: c.TempDir(),
	})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}
	sel, _ := cache.NewSelector(nil, nil)

	srv, err := server.New(server.Config{
		Store: store, Cache: c, Coalescer: coalescer, Selector: sel,
		Routes: defaultRoutes(), PollInterval: time.Hour, WindowSize: 3,
		Authorizer: server.AuthorizerFunc(func(*http.Request) bool { return false }),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	defer func() { _ = srv.Close() }()
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	front := httptest.NewServer(srv.Handler())
	defer front.Close()

	resp, err := front.Client().Get(front.URL + "/debian/bookworm/pool/main/n/nginx/nginx_1.24.0-1_amd64.deb")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestOnlyGetAndHeadAreAllowed(t *testing.T) {
	t.Parallel()

	store := &countingStore{Store: blobstoretest.NewMem()}
	publishRevision(t, store, "debian/bookworm", "debian/bookworm", defaultFiles())
	h := newHarness(t, store, harnessOptions{routes: defaultRoutes()})

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		resp := h.do(t, method, "/debian/bookworm/pool/main/n/nginx/nginx_1.24.0-1_amd64.deb", nil)
		_ = bodyOf(t, resp)
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, resp.StatusCode)
		}
	}
}
