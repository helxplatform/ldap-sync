package main

import (
	"log/slog"
	"os"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// Test bootstrap
// ---------------------------------------------------------------------------

func TestMain(m *testing.M) {
	// Use error-level logging during tests to keep output quiet.
	h := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError})
	logger = slog.New(h)
	os.Exit(m.Run())
}

// resetState wipes all package-level mutable state so tests don't bleed into
// each other. Call at the start of any test that touches global maps.
func resetState(t *testing.T) {
	t.Helper()
	searchesMu.Lock()
	searches = make(map[string]*SearchSpec)
	searchesMu.Unlock()

	searchResultsMu.Lock()
	searchResults = make(map[string]map[string]LDAPResult)
	searchResultsMu.Unlock()

	bindingsMu.Lock()
	bindings = make(map[string]string)
	nullBindings = make(map[string]struct{})
	bindingsMu.Unlock()

	dependencyTracker = newDependencyState()
	dnLocks = sync.Map{}
	db = nil

	pluginRegistry = nil
	dispatchSyncEvent = func(event SyncEvent) {
		if pluginRegistry == nil {
			return
		}
		pluginRegistry.Dispatch(event)
	}
}

// mockStore captures calls to ldapStore and records the entries written.
// The reported SyncOp is Created the first time a DN is seen and Updated
// thereafter, mirroring storeDestinationLDAP's Add-vs-Modify branch.
type mockStore struct {
	mu      sync.Mutex
	written []*TransformedEntry
	seen    map[string]struct{}
	err     error // returned on every call if set
}

func (s *mockStore) store(e *TransformedEntry) (SyncOp, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Deep-copy content so concurrent mutations don't corrupt the snapshot.
	copied := make(map[string]interface{}, len(e.Content))
	for k, v := range e.Content {
		copied[k] = v
	}
	s.written = append(s.written, &TransformedEntry{DN: e.DN, Content: copied})
	if s.err != nil {
		return "", s.err
	}
	if s.seen == nil {
		s.seen = make(map[string]struct{})
	}
	key := normalizeDN(e.DN)
	if _, ok := s.seen[key]; ok {
		return SyncOpUpdated, nil
	}
	s.seen[key] = struct{}{}
	return SyncOpCreated, nil
}

func (s *mockStore) entries() []*TransformedEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*TransformedEntry, len(s.written))
	copy(out, s.written)
	return out
}

// withMockStore replaces ldapStore for the duration of t and restores it
// afterward.  It also ensures ldapStore is never nil (it's nil until main()
// sets it, which tests never call).
func withMockStore(t *testing.T) *mockStore {
	t.Helper()
	ms := &mockStore{}
	ldapStore = ms.store
	t.Cleanup(func() { ldapStore = nil })
	return ms
}
