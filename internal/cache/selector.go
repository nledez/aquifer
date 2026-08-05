// Package cache is the edge's local disk cache: a size-capped LRU over blobs
// addressed by digest, with a pinned segment that never leaves.
//
// The two pattern lists are the operator's main tuning lever. Nothing here
// ever expires or is invalidated: a blob is either referenced by a retained
// revision or it is not, and an unreferenced one simply falls out of the LRU.
package cache

import (
	"fmt"
	"iter"
	"slices"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/nledez/aquifer/internal/manifest"
)

// Class says how a path is treated by the cache policy.
type Class int

const (
	// ClassLazy is fetched on demand and managed by the LRU.
	ClassLazy Class = iota
	// ClassPrefetch is fetched in the background after a revision switch,
	// then managed by the LRU like anything else.
	ClassPrefetch
	// ClassPinned is fetched on every revision load and never evicted.
	ClassPinned
)

func (c Class) String() string {
	switch c {
	case ClassPinned:
		return "pinned"
	case ClassPrefetch:
		return "prefetch"
	default:
		return "lazy"
	}
}

// Selector classifies serving paths against the configured patterns.
//
// Patterns are matched against the full serving path, repo prefix included, so
// that an operator can pin one publication's metadata without pinning
// another's.
type Selector struct {
	pinned   []string
	prefetch []string
}

// NewSelector compiles the two pattern lists.
func NewSelector(pinned, prefetch []string) (*Selector, error) {
	for _, group := range []struct {
		name     string
		patterns []string
	}{{"pinned", pinned}, {"prefetch", prefetch}} {
		for _, pattern := range group.patterns {
			if !doublestar.ValidatePattern(pattern) {
				return nil, fmt.Errorf("cache: %s pattern %q is not a valid glob", group.name, pattern)
			}
		}
	}
	return &Selector{pinned: slices.Clone(pinned), prefetch: slices.Clone(prefetch)}, nil
}

// Classify reports how a serving path is treated.
func (s *Selector) Classify(path string) Class {
	if matchAny(s.pinned, path) {
		return ClassPinned
	}
	if matchAny(s.prefetch, path) {
		return ClassPrefetch
	}
	return ClassLazy
}

func matchAny(patterns []string, path string) bool {
	for _, pattern := range patterns {
		// The patterns were validated at construction, so a match error here
		// is impossible; ignoring it keeps the hot path free of dead branches.
		if ok, _ := doublestar.Match(pattern, path); ok {
			return true
		}
	}
	return false
}

// Plan is what a revision switch asks of the cache and of the prefetcher.
// Both maps are keyed by digest, since several paths can share one blob.
type Plan struct {
	Pinned   map[string]int64
	Prefetch map[string]int64
}

// SelectStats is what the patterns actually cover in a given revision. It is
// logged on every switch: a pattern's blast radius is invisible otherwise.
type SelectStats struct {
	PinnedObjects   int
	PinnedBytes     int64
	PrefetchObjects int
	PrefetchBytes   int64
	// UnmatchedPatterns lists patterns that covered nothing, which is almost
	// always a typo in a repo prefix.
	UnmatchedPatterns []string
}

// Select builds the plan for one revision and reports what it covers.
func (s *Selector) Select(seq iter.Seq[manifest.Entry]) (Plan, SelectStats) {
	plan := Plan{
		Pinned:   map[string]int64{},
		Prefetch: map[string]int64{},
	}
	var stats SelectStats

	used := make(map[string]bool, len(s.pinned)+len(s.prefetch))
	for entry := range seq {
		switch {
		case s.markMatches(s.pinned, entry.Path, used):
			// A pinned pattern is implicitly a prefetch pattern: there is no
			// point pinning a blob that is only fetched when someone asks.
			if _, seen := plan.Pinned[entry.SHA256]; !seen {
				plan.Pinned[entry.SHA256] = entry.Size
				stats.PinnedObjects++
				stats.PinnedBytes += entry.Size
			}
			plan.Prefetch[entry.SHA256] = entry.Size

		case s.markMatches(s.prefetch, entry.Path, used):
			if _, seen := plan.Prefetch[entry.SHA256]; !seen {
				stats.PrefetchObjects++
				stats.PrefetchBytes += entry.Size
			}
			plan.Prefetch[entry.SHA256] = entry.Size
		}
	}

	for _, pattern := range slices.Concat(s.pinned, s.prefetch) {
		if !used[pattern] {
			stats.UnmatchedPatterns = append(stats.UnmatchedPatterns, pattern)
		}
	}
	return plan, stats
}

// markMatches reports whether path matches, recording which patterns pulled
// their weight so that dead ones can be called out.
func (s *Selector) markMatches(patterns []string, path string, used map[string]bool) bool {
	var matched bool
	for _, pattern := range patterns {
		if ok, _ := doublestar.Match(pattern, path); ok {
			used[pattern] = true
			matched = true
		}
	}
	return matched
}
