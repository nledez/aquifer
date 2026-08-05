package manifest

import (
	"slices"
	"sync"
	"sync/atomic"
)

// DefaultWindowSize is how many revisions of a repo an edge keeps resolvable.
const DefaultWindowSize = 5

// Window holds the most recent revisions of one repo.
//
// Keeping more than the current revision resolvable is what stops a client
// from getting a 404 when it runs "apt update" against one edge and
// "apt install" against another while a switchover is in flight. Pool paths
// resolve against every retained revision; metadata resolves against the
// current one alone, so a stale index can never be served.
//
// Reads go through an atomically swapped snapshot, so the serving path takes
// no lock at all.
type Window struct {
	keep int

	mu    sync.Mutex // serialises Push
	state atomic.Pointer[windowState]
}

type windowState struct {
	// retained is newest first; retained[0] is the current revision.
	retained []*Manifest
}

// NewWindow returns a window keeping the given number of revisions. A
// non-positive size falls back to DefaultWindowSize.
func NewWindow(keep int) *Window {
	if keep <= 0 {
		keep = DefaultWindowSize
	}
	w := &Window{keep: keep}
	w.state.Store(&windowState{})
	return w
}

// Push makes m the current revision and drops whatever falls out of the
// window. The swap is atomic: a reader sees either the old set or the new one.
func (w *Window) Push(m *Manifest) {
	w.mu.Lock()
	defer w.mu.Unlock()

	old := w.state.Load()
	retained := make([]*Manifest, 0, w.keep)
	retained = append(retained, m)
	for _, prev := range old.retained {
		if len(retained) == w.keep {
			break
		}
		if prev.Revision == m.Revision {
			continue
		}
		retained = append(retained, prev)
	}
	w.state.Store(&windowState{retained: retained})
}

// Current returns the newest revision, or nil if none has been pushed.
func (w *Window) Current() *Manifest {
	retained := w.state.Load().retained
	if len(retained) == 0 {
		return nil
	}
	return retained[0]
}

// Retained returns the revisions still resolvable, newest first.
func (w *Window) Retained() []*Manifest {
	return slices.Clone(w.state.Load().retained)
}

// Lookup resolves a serving path against the window.
func (w *Window) Lookup(path string) (Entry, bool) {
	retained := w.state.Load().retained
	if len(retained) == 0 {
		return Entry{}, false
	}
	if IsMetadata(path) {
		return retained[0].Lookup(path)
	}
	for _, m := range retained {
		if e, ok := m.Lookup(path); ok {
			return e, true
		}
	}
	return Entry{}, false
}
