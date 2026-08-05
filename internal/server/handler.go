package server

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/nledez/aquifer/internal/cache"
	"github.com/nledez/aquifer/internal/manifest"
)

// Authorizer gates every request.
//
// SPEC section 8: there is no authentication, and none is being built. This
// exists so that adding one later is a middleware rather than a rewrite.
type Authorizer interface {
	Authorize(r *http.Request) bool
}

// AuthorizerFunc adapts a function to Authorizer.
type AuthorizerFunc func(r *http.Request) bool

// Authorize calls f.
func (f AuthorizerFunc) Authorize(r *http.Request) bool { return f(r) }

// AllowAll serves everyone, which is the default and the only implementation
// this project ships.
type AllowAll struct{}

// Authorize always allows.
func (AllowAll) Authorize(*http.Request) bool { return true }

// byHashSegment marks a path that addresses an index by its own digest.
const byHashSegment = "/by-hash/SHA256/"

// Handler serves apt clients.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.serveBlob)
	return s.wrap(mux)
}

// AdminHandler serves metrics and probes on a separate port, so that neither
// is reachable from the client-facing address.
func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /readyz", s.serveReadyz)
	return mux
}

func (s *Server) serveReadyz(w http.ResponseWriter, _ *http.Request) {
	report := s.readiness()

	w.Header().Set("Content-Type", "application/json")
	if !report.Ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(report)
}

// wrap applies the request-wide concerns: method filtering, authorization and
// timing.
func (s *Server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "only GET and HEAD are supported", http.StatusMethodNotAllowed)
			return
		}
		if !s.auth.Authorize(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// resolved is everything the serving path needs about a request.
type resolved struct {
	entry    manifest.Entry
	modTime  time.Time
	class    string
	filename string
}

func (s *Server) serveBlob(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	// path.Clean on an absolute path removes any ".." before it can escape the
	// namespace; the result is then relative, the way manifests store it.
	servingPath := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if servingPath == "" || servingPath == "." {
		http.NotFound(w, r)
		return
	}

	res, ok := s.resolve(servingPath)
	if !ok {
		// Nothing is recorded for a path no revision knows: it is not a cache
		// miss, it is a request for something that does not exist.
		http.NotFound(w, r)
		return
	}

	result := s.write(w, r, res)
	s.metrics.requests.WithLabelValues(res.class, result).Inc()
	s.metrics.duration.WithLabelValues(res.class).Observe(time.Since(started).Seconds())
}

// resolve turns a serving path into the blob behind it.
func (s *Server) resolve(servingPath string) (resolved, bool) {
	repo, ok := s.router.Match(servingPath)
	if !ok {
		return resolved{}, false
	}
	rs, ok := s.repos[repo]
	if !ok {
		return resolved{}, false
	}

	entry, m, ok := s.resolveEntry(rs, servingPath)
	if !ok {
		return resolved{}, false
	}

	// The metric's classes are about what an operator reasons over, not about
	// the cache policy: metadata versus packages. A path is metadata either
	// because the operator pinned it or because it lives under dists/.
	class := classPool
	if s.selector.Classify(servingPath) == cache.ClassPinned || manifest.IsMetadata(servingPath) {
		class = classPinned
	}
	return resolved{
		entry:    entry,
		modTime:  m.CreatedAt,
		class:    class,
		filename: path.Base(servingPath),
	}, true
}

// resolveEntry handles both ordinary paths and by-hash paths.
func (s *Server) resolveEntry(rs *repoState, servingPath string) (manifest.Entry, *manifest.Manifest, bool) {
	idx := strings.Index(servingPath, byHashSegment)
	if idx < 0 {
		return rs.window.Resolve(servingPath)
	}

	// SPEC section 5: a by-hash path resolves straight from the digest in the
	// URL. It is still checked against the retained revisions, so that the
	// endpoint cannot be used to make the edge pull arbitrary objects out of
	// object storage.
	//
	// Only SHA256 is served, which is what the spec asks for and all that
	// Aquifer addresses by. apt, however, asks for the strongest digest the
	// Release declares, and both apt-ftparchive and aptly emit SHA512, so an
	// apt configured for by-hash gets a 404 here and falls back to the plain
	// path - correctly, but at the cost of one wasted round trip per index.
	//
	// That gap is deliberate: the access logs of the mirror this replaces show
	// no by-hash request at all, so serving SHA512 would mean carrying a second
	// digest for every index, and roughly 1200 extra manifest entries per
	// revision, for a path nobody takes. Should aptly ever be published with
	// -acquire-by-hash, revisit this: the Release already carries the SHA512 of
	// every index, so publish could emit the by-hash paths as ordinary manifest
	// entries pointing at the same blob, and this special case could go away.
	digest := servingPath[idx+len(byHashSegment):]
	if strings.Contains(digest, "/") || !isSHA256Hex(digest) {
		return manifest.Entry{}, nil, false
	}
	if !manifest.IsMetadata(servingPath[:idx+1]) {
		return manifest.Entry{}, nil, false
	}
	return rs.window.ResolveDigest(digest)
}

// write answers the request and reports how it was served.
func (s *Server) write(w http.ResponseWriter, r *http.Request, res resolved) string {
	etag := `"` + res.entry.SHA256 + `"`
	header := w.Header()
	header.Set("ETag", etag)
	header.Set("Accept-Ranges", "bytes")
	header.Set("Last-Modified", res.modTime.UTC().Format(http.TimeFormat))
	header.Set("Content-Type", contentTypeFor(res.filename))

	// Content addressing makes revalidation exact: a matching ETag cannot be
	// stale, so it is answered without touching the cache or the backend. apt
	// revalidates constantly, and this is what makes that free.
	if matchesETag(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return resultHit
	}

	if r.Method == http.MethodHead {
		// The manifest already states the size. Downloading 90 MiB to report
		// a number we hold would be absurd.
		header.Set("Content-Length", strconv.FormatInt(res.entry.Size, 10))
		w.WriteHeader(http.StatusOK)
		return resultHit
	}

	if rc, ok, err := s.cache.Open(res.entry.SHA256); err == nil && ok {
		defer func() { _ = rc.Close() }()
		http.ServeContent(w, r, res.filename, res.modTime, rc)
		return resultHit
	}

	// SPEC section 7: a Range request on a blob that is still downloading is
	// served by waiting for it, never by bypassing the coalescer with a
	// second, partial GET.
	if r.Header.Get("Range") != "" {
		rs, err := s.coalescer.FetchSeeker(r.Context(), res.entry.SHA256, res.entry.Size)
		if err != nil {
			return s.fail(w, r, res, err)
		}
		defer func() { _ = rs.Close() }()
		http.ServeContent(w, r, res.filename, res.modTime, rs)
		return resultMiss
	}

	rc, err := s.coalescer.Fetch(r.Context(), res.entry.SHA256, res.entry.Size)
	if err != nil {
		return s.fail(w, r, res, err)
	}
	defer func() { _ = rc.Close() }()

	header.Set("Content-Length", strconv.FormatInt(res.entry.Size, 10))
	w.WriteHeader(http.StatusOK)

	// CopyN, not Copy, and the size is what makes it correct.
	//
	// Copy performs one more Read after the last byte, to see EOF. At that
	// moment the leader is typically still verifying the digest and admitting
	// the blob, so the entry is not done and the reader blocks; meanwhile the
	// client, which knows the length, has already closed. That final read then
	// fails with the request's cancellation and a perfectly delivered response
	// looks like an error. CopyN stops at the declared length and never issues
	// that read.
	written, err := io.CopyN(w, rc, res.entry.Size)
	switch {
	case err == nil:
		return resultMiss
	case r.Context().Err() != nil:
		// The client hung up part-way. Nothing failed on this side, and
		// counting it as an error would make the metric alert on apt being
		// interrupted.
		s.log.Debug("client left before the body was complete",
			"path", r.URL.Path, "written", written, "size", res.entry.Size)
		return resultMiss
	default:
		// The status line is already out; all that is left is to stop and let
		// the client see a short body.
		s.log.Warn("streaming a blob failed part-way",
			"path", r.URL.Path, "hash", res.entry.SHA256,
			"written", written, "size", res.entry.Size, "error", err)
		return resultError
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, res resolved, err error) string {
	if errors.Is(err, r.Context().Err()) && r.Context().Err() != nil {
		// The client gave up. Nothing to report and nowhere to report it.
		return resultError
	}
	s.log.Error("could not fetch a blob",
		"path", r.URL.Path, "hash", res.entry.SHA256, "error", err)
	http.Error(w, "upstream fetch failed", http.StatusBadGateway)
	return resultError
}

// matchesETag implements the subset of If-None-Match that matters here: an
// exact tag or "*". Content addressing means weak comparison is irrelevant.
func matchesETag(header, etag string) bool {
	if header == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag {
			return true
		}
		if strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

// contentTypeFor keeps http.ServeContent from sniffing, which would mean
// reading and rewinding the first 512 bytes of every response.
func contentTypeFor(filename string) string {
	switch path.Ext(filename) {
	case ".deb", ".udeb", ".ddeb":
		return "application/vnd.debian.binary-package"
	case ".gz":
		return "application/gzip"
	case ".bz2":
		return "application/x-bzip2"
	case ".xz":
		return "application/x-xz"
	case ".gpg", ".asc":
		return "application/pgp-signature"
	}
	if ct := mime.TypeByExtension(path.Ext(filename)); ct != "" {
		return ct
	}
	switch filename {
	case "Release", "InRelease", "Packages", "Sources":
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
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
