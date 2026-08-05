package server_test

import (
	"testing"

	"github.com/nledez/aquifer/internal/server"
)

// SPEC section 2: several publications coexist, one of them at the root, so a
// plain prefix map is ambiguous. The longest prefix wins and the root is tried
// last.
func TestRouterPrefersTheLongestPrefixAndFallsBackToTheRoot(t *testing.T) {
	t.Parallel()

	r, err := server.NewRouter([]server.Route{
		{Prefix: "", Repo: "root"},
		{Prefix: "debian", Repo: "debian"},
		{Prefix: "debian/bookworm", Repo: "debian/bookworm"},
		{Prefix: "ubuntu/noble", Repo: "ubuntu/noble"},
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	cases := map[string]string{
		"debian/bookworm/pool/main/n/nginx.deb": "debian/bookworm",
		"debian/trixie/pool/main/a/apt.deb":     "debian",
		"ubuntu/noble/dists/noble/InRelease":    "ubuntu/noble",
		"pool/main/x/x.deb":                     "root",
		"dists/stable/InRelease":                "root",
	}
	for path, want := range cases {
		got, ok := r.Match(path)
		if !ok {
			t.Fatalf("Match(%q) found no repo", path)
		}
		if got != want {
			t.Fatalf("Match(%q) = %q, want %q", path, got, want)
		}
	}
}

// A prefix must match on a path boundary; "debian" must not swallow
// "debiantest".
func TestRouterMatchesOnPathBoundaries(t *testing.T) {
	t.Parallel()

	r, err := server.NewRouter([]server.Route{{Prefix: "debian", Repo: "debian"}})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	if _, ok := r.Match("debiantest/pool/x.deb"); ok {
		t.Fatal("a prefix matched in the middle of a path segment")
	}
	if got, ok := r.Match("debian/pool/x.deb"); !ok || got != "debian" {
		t.Fatalf("Match = %q, %v", got, ok)
	}
	// The prefix itself is a legitimate, if useless, path.
	if _, ok := r.Match("debian"); !ok {
		t.Fatal("the prefix itself did not match")
	}
}

func TestRouterReportsNoRepoWithoutARootRoute(t *testing.T) {
	t.Parallel()

	r, err := server.NewRouter([]server.Route{{Prefix: "debian", Repo: "debian"}})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if _, ok := r.Match("ubuntu/noble/pool/x.deb"); ok {
		t.Fatal("Match invented a repo for an unrouted path")
	}
}

func TestNewRouterRejectsAmbiguousConfiguration(t *testing.T) {
	t.Parallel()

	cases := map[string][]server.Route{
		"duplicate prefix": {
			{Prefix: "debian", Repo: "one"},
			{Prefix: "debian", Repo: "two"},
		},
		"empty repo": {
			{Prefix: "debian", Repo: ""},
		},
		"no routes": {},
	}
	for name, routes := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := server.NewRouter(routes); err == nil {
				t.Fatalf("NewRouter accepted %s", name)
			}
		})
	}
}

// Leading and trailing slashes in a configured prefix are a common mistake and
// must not silently break routing.
func TestRouterNormalisesPrefixes(t *testing.T) {
	t.Parallel()

	r, err := server.NewRouter([]server.Route{{Prefix: "/debian/bookworm/", Repo: "debian/bookworm"}})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if got, ok := r.Match("debian/bookworm/pool/x.deb"); !ok || got != "debian/bookworm" {
		t.Fatalf("Match = %q, %v", got, ok)
	}
}
