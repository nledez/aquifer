// Package server is the edge: it resolves an apt client's request to a blob,
// serves it from the local cache, and keeps the revision each repo points at
// up to date.
package server

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Route maps a serving path prefix to a repo.
type Route struct {
	// Prefix is the leading path segments, without slashes. Empty means the
	// publication served at the archive root.
	Prefix string
	Repo   string
}

// Router resolves a serving path to the repo that owns it.
//
// A plain map does not do, because one publication sits at the root and would
// otherwise match everything. Prefixes are tried longest first, so the root
// route is only reached once every more specific one has failed, and a repo
// that does not hold the path yields a plain 404 rather than an ambiguous
// search through the others.
type Router struct {
	routes []Route
}

// NewRouter validates and orders the routing table.
func NewRouter(routes []Route) (*Router, error) {
	if len(routes) == 0 {
		return nil, errors.New("server: at least one route is required")
	}

	normalised := make([]Route, 0, len(routes))
	seen := map[string]string{}
	for _, route := range routes {
		if route.Repo == "" {
			return nil, fmt.Errorf("server: route for prefix %q has no repo", route.Prefix)
		}
		prefix := strings.Trim(route.Prefix, "/")
		if other, dup := seen[prefix]; dup {
			return nil, fmt.Errorf("server: prefix %q is claimed by both %q and %q",
				prefix, other, route.Repo)
		}
		seen[prefix] = route.Repo
		normalised = append(normalised, Route{Prefix: prefix, Repo: route.Repo})
	}

	slices.SortFunc(normalised, func(a, b Route) int {
		if d := len(b.Prefix) - len(a.Prefix); d != 0 {
			return d
		}
		return strings.Compare(a.Prefix, b.Prefix)
	})
	return &Router{routes: normalised}, nil
}

// Match returns the repo owning a serving path.
func (r *Router) Match(path string) (string, bool) {
	for _, route := range r.routes {
		if route.Prefix == "" {
			return route.Repo, true
		}
		if path == route.Prefix || strings.HasPrefix(path, route.Prefix+"/") {
			return route.Repo, true
		}
	}
	return "", false
}

// Routes returns the table in match order.
func (r *Router) Routes() []Route { return slices.Clone(r.routes) }
