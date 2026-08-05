package server

import (
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Result classifies how a request was answered.
const (
	resultHit   = "hit"
	resultMiss  = "miss"
	resultError = "error"
	// resultNotFound is a path no revision knows. It is neither a hit nor a
	// miss, but leaving it uncounted made a misconfigured prefix produce
	// silent 404 storms.
	resultNotFound = "notfound"
)

// Class buckets a request by what kind of object it asked for.
//
// The breakdown is what makes a hit ratio actionable: an overall 85% can hide
// metadata at 100% and pool at 40%, and those two situations call for opposite
// responses.
const (
	classPinned = "pinned"
	classPool   = "pool"
)

type metrics struct {
	registry *prometheus.Registry

	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newMetrics(s *Server) *metrics {
	m := &metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aquifer_cache_requests_total",
			Help: "Requests served, by object class and cache result.",
		}, []string{"class", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "aquifer_request_duration_seconds",
			Help:    "Request duration, by object class.",
			Buckets: []float64{.001, .005, .01, .05, .1, .5, 1, 5, 10, 30, 60},
		}, []string{"class"}),
	}

	m.registry.MustRegister(m.requests, m.duration, &stateCollector{server: s})

	// Pre-create the series so that a scrape before the first request still
	// shows the breakdown rather than an empty result.
	for _, class := range []string{classPinned, classPool} {
		for _, result := range []string{resultHit, resultMiss, resultError, resultNotFound} {
			m.requests.WithLabelValues(class, result)
		}
		m.duration.WithLabelValues(class)
	}
	return m
}

// stateCollector reports the live state on every scrape rather than from a
// snapshot kept up to date by a goroutine. Manifest age in particular changes
// continuously, so anything cached would be wrong by construction.
type stateCollector struct {
	server *Server
}

var (
	descCoalesced = prometheus.NewDesc(
		"aquifer_fetch_coalesced_readers_total",
		"Requesters served by an already running download. This is the number that proves coalescing works.",
		nil, nil)
	descInflight = prometheus.NewDesc(
		"aquifer_fetch_inflight", "Downloads currently running.", nil, nil)
	descCacheBytes = prometheus.NewDesc(
		"aquifer_cache_bytes", "Bytes held by the evictable segment of the cache.", nil, nil)
	descCacheObjects = prometheus.NewDesc(
		"aquifer_cache_objects", "Objects held by the evictable segment of the cache.", nil, nil)
	descEvictions = prometheus.NewDesc(
		"aquifer_cache_evictions_total", "Blobs evicted since start.", nil, nil)
	descPinnedBytes = prometheus.NewDesc(
		"aquifer_cache_pinned_bytes", "Bytes held by pinned blobs, outside the LRU budget.", nil, nil)
	descPinnedObjects = prometheus.NewDesc(
		"aquifer_cache_pinned_objects", "Pinned blobs resident on disk.", nil, nil)
	descPinnedPlanned = prometheus.NewDesc(
		"aquifer_cache_pinned_planned_objects",
		"Pinned blobs the current revisions call for, resident or not.", nil, nil)
	descTempBytes = prometheus.NewDesc(
		"aquifer_cache_temp_bytes", "Bytes staged by in-flight downloads, outside the LRU budget.", nil, nil)
	descRevisionInfo = prometheus.NewDesc(
		"aquifer_manifest_revision_info",
		"The revision each repo is serving. Alert on divergence between edges.",
		[]string{"repo", "revision"}, nil)
	descManifestAge = prometheus.NewDesc(
		"aquifer_manifest_age_seconds",
		"Seconds since the current revision's manifest was created.",
		[]string{"repo"}, nil)
	descValidUntil = prometheus.NewDesc(
		"aquifer_release_valid_until_seconds",
		"Seconds left before a suite's Valid-Until expires. At zero, apt refuses the repository. "+
			"Reported as +Inf for a Release that declares no expiry, which is what Debian stable does.",
		[]string{"repo", "suite"}, nil)
)

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		descCoalesced, descInflight, descCacheBytes, descCacheObjects, descEvictions,
		descPinnedBytes, descPinnedObjects, descPinnedPlanned, descTempBytes,
		descRevisionInfo, descManifestAge, descValidUntil,
	} {
		ch <- d
	}
}

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.server

	ch <- prometheus.MustNewConstMetric(descCoalesced, prometheus.CounterValue,
		float64(s.coalescer.CoalescedReaders()))
	ch <- prometheus.MustNewConstMetric(descInflight, prometheus.GaugeValue,
		float64(s.coalescer.Inflight()))

	stats := s.cache.Stats()
	ch <- prometheus.MustNewConstMetric(descCacheBytes, prometheus.GaugeValue, float64(stats.Bytes))
	ch <- prometheus.MustNewConstMetric(descCacheObjects, prometheus.GaugeValue, float64(stats.Objects))
	ch <- prometheus.MustNewConstMetric(descEvictions, prometheus.CounterValue, float64(stats.Evictions))
	ch <- prometheus.MustNewConstMetric(descPinnedBytes, prometheus.GaugeValue, float64(stats.PinnedBytes))
	ch <- prometheus.MustNewConstMetric(descPinnedObjects, prometheus.GaugeValue, float64(stats.PinnedObjects))
	ch <- prometheus.MustNewConstMetric(descTempBytes, prometheus.GaugeValue, float64(stats.TempBytes))

	planned, _ := s.cache.PinnedPlanned()
	ch <- prometheus.MustNewConstMetric(descPinnedPlanned, prometheus.GaugeValue, float64(planned))

	now := time.Now()
	for _, rs := range s.repoStates() {
		snap := rs.snapshot()
		if snap.revision == "" {
			continue
		}
		ch <- prometheus.MustNewConstMetric(descRevisionInfo, prometheus.GaugeValue, 1,
			rs.name, snap.revision)
		ch <- prometheus.MustNewConstMetric(descManifestAge, prometheus.GaugeValue,
			now.Sub(snap.createdAt).Seconds(), rs.name)

		for suite, validUntil := range snap.validUntil {
			seconds := math.Inf(1)
			if !validUntil.IsZero() {
				seconds = math.Max(0, validUntil.Sub(now).Seconds())
			}
			ch <- prometheus.MustNewConstMetric(descValidUntil, prometheus.GaugeValue,
				seconds, rs.name, suite)
		}
	}
}
