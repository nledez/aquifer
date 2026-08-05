package cache_test

import (
	"slices"
	"testing"

	"github.com/nledez/aquifer/internal/cache"
	"github.com/nledez/aquifer/internal/manifest"
)

func entries(paths ...string) []manifest.Entry {
	out := make([]manifest.Entry, len(paths))
	for i, p := range paths {
		out[i] = manifest.Entry{Path: p, SHA256: hashFor(p), Size: int64(100 + i)}
	}
	return out
}

// hashFor derives a stable, valid-looking digest from a path.
func hashFor(path string) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 64)
	var acc byte
	for i := range out {
		if i < len(path) {
			acc += path[i]
		}
		acc = acc*31 + byte(i)
		out[i] = digits[acc&0x0f]
	}
	return string(out)
}

func seq(list []manifest.Entry) func(func(manifest.Entry) bool) {
	return func(yield func(manifest.Entry) bool) {
		for _, e := range list {
			if !yield(e) {
				return
			}
		}
	}
}

// SPEC section 6: patterns are matched against the full serving path, repo
// prefix included, and ** has to work, which filepath.Match cannot do.
func TestSelectorMatchesFullServingPathsWithDoubleStar(t *testing.T) {
	t.Parallel()

	sel, err := cache.NewSelector([]string{"**/dists/**", "dists/**"}, nil)
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}

	pinned := []string{
		"dists/bookworm/InRelease",
		"debian/bookworm/dists/bookworm/main/binary-amd64/Packages",
		"ubuntu/noble/dists/noble/Release",
	}
	for _, p := range pinned {
		if got := sel.Classify(p); got != cache.ClassPinned {
			t.Fatalf("Classify(%q) = %v, want ClassPinned", p, got)
		}
	}

	lazy := []string{
		"debian/bookworm/pool/main/n/nginx/nginx.deb",
		"pool/main/a/apt/apt.deb",
		"keys/archive.gpg",
	}
	for _, p := range lazy {
		if got := sel.Classify(p); got != cache.ClassLazy {
			t.Fatalf("Classify(%q) = %v, want ClassLazy", p, got)
		}
	}
}

// SPEC section 6: a pinned pattern is implicitly prefetched.
func TestPinnedPatternsAreImplicitlyPrefetched(t *testing.T) {
	t.Parallel()

	sel, err := cache.NewSelector([]string{"**/dists/**"}, []string{"**/pool/main/a/**"})
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}

	list := entries(
		"debian/bookworm/dists/bookworm/InRelease",
		"debian/bookworm/pool/main/a/apt/apt.deb",
		"debian/bookworm/pool/main/n/nginx/nginx.deb",
	)
	plan, _ := sel.Select(seq(list))

	pinnedHash := hashFor("debian/bookworm/dists/bookworm/InRelease")
	if _, ok := plan.Pinned[pinnedHash]; !ok {
		t.Fatal("the metadata blob is not pinned")
	}
	if _, ok := plan.Prefetch[pinnedHash]; !ok {
		t.Fatal("a pinned blob must also be prefetched")
	}

	aptHash := hashFor("debian/bookworm/pool/main/a/apt/apt.deb")
	if _, ok := plan.Prefetch[aptHash]; !ok {
		t.Fatal("an explicitly prefetched blob is missing from the plan")
	}
	if _, ok := plan.Pinned[aptHash]; ok {
		t.Fatal("prefetching must not pin")
	}

	nginxHash := hashFor("debian/bookworm/pool/main/n/nginx/nginx.deb")
	if _, ok := plan.Prefetch[nginxHash]; ok {
		t.Fatal("an unmatched blob must be fetched lazily, not prefetched")
	}
}

func TestSelectorReportsWhatEachPatternCovers(t *testing.T) {
	t.Parallel()

	sel, err := cache.NewSelector([]string{"**/dists/**"}, nil)
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}

	list := entries(
		"debian/bookworm/dists/bookworm/InRelease",
		"debian/bookworm/dists/bookworm/main/binary-amd64/Packages",
		"debian/bookworm/pool/main/n/nginx/nginx.deb",
	)
	_, stats := sel.Select(seq(list))

	if stats.PinnedObjects != 2 {
		t.Fatalf("PinnedObjects = %d, want 2", stats.PinnedObjects)
	}
	want := list[0].Size + list[1].Size
	if stats.PinnedBytes != want {
		t.Fatalf("PinnedBytes = %d, want %d", stats.PinnedBytes, want)
	}
}

// A pattern that matches nothing is usually a typo in a repo prefix, and it
// fails silently unless it is called out.
func TestSelectorReportsPatternsThatMatchNothing(t *testing.T) {
	t.Parallel()

	sel, err := cache.NewSelector([]string{"**/dists/**", "typo/**"}, []string{"also-wrong/**"})
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}

	_, stats := sel.Select(seq(entries("debian/bookworm/dists/bookworm/InRelease")))

	want := []string{"also-wrong/**", "typo/**"}
	got := slices.Clone(stats.UnmatchedPatterns)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("UnmatchedPatterns = %v, want %v", got, want)
	}
}

// A pattern this wide would fill the disk with pinned data that never leaves.
// It has to fail fast and loudly rather than quietly.
func TestSelectorRejectsAMalformedPattern(t *testing.T) {
	t.Parallel()

	if _, err := cache.NewSelector([]string{"["}, nil); err == nil {
		t.Fatal("NewSelector accepted a malformed glob")
	}
	if _, err := cache.NewSelector(nil, []string{"[a-"}); err == nil {
		t.Fatal("NewSelector accepted a malformed prefetch glob")
	}
}

// Several paths can share one blob; the plan is keyed by digest, and the byte
// count must not double-count it.
func TestSelectorCountsAsharedBlobOnce(t *testing.T) {
	t.Parallel()

	sel, err := cache.NewSelector([]string{"**/dists/**"}, nil)
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}

	shared := manifest.Entry{Path: "a/dists/x/Release", SHA256: hashFor("shared"), Size: 500}
	same := manifest.Entry{Path: "b/dists/x/Release", SHA256: hashFor("shared"), Size: 500}
	plan, stats := sel.Select(seq([]manifest.Entry{shared, same}))

	if len(plan.Pinned) != 1 {
		t.Fatalf("plan holds %d pinned blobs, want 1", len(plan.Pinned))
	}
	if stats.PinnedBytes != 500 {
		t.Fatalf("PinnedBytes = %d, want 500", stats.PinnedBytes)
	}
	if stats.PinnedObjects != 1 {
		t.Fatalf("PinnedObjects = %d, want 1", stats.PinnedObjects)
	}
}
