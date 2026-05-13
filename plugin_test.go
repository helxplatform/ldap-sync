package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingPlugin captures every event it receives. Optionally fails the
// first N Apply calls so retry behavior can be exercised.
type recordingPlugin struct {
	name        string
	matchDN     string // if non-empty, only match this DN; otherwise match all
	failuresLeft atomic.Int32
	mu          sync.Mutex
	events      []SyncEvent
}

func (p *recordingPlugin) Name() string { return p.name }

func (p *recordingPlugin) Match(e SyncEvent) bool {
	if p.matchDN == "" {
		return true
	}
	return normalizeDN(e.DN) == normalizeDN(p.matchDN)
}

func (p *recordingPlugin) Apply(_ context.Context, e SyncEvent) error {
	if p.failuresLeft.Load() > 0 {
		p.failuresLeft.Add(-1)
		return errors.New("simulated failure")
	}
	p.mu.Lock()
	p.events = append(p.events, e)
	p.mu.Unlock()
	return nil
}

func (p *recordingPlugin) snapshot() []SyncEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]SyncEvent, len(p.events))
	copy(out, p.events)
	return out
}

// installRegistry swaps in a fresh registry plus the given plugins for the
// duration of t. Retry tuned for fast tests.
func installRegistry(t *testing.T, plugins ...Plugin) *Registry {
	t.Helper()
	reg := NewRegistry(PluginRetry{MaxAttempts: 3, InitialDelayMs: 1, MaxDelayMs: 5})
	for _, p := range plugins {
		reg.Register(p)
	}
	pluginRegistry = reg
	t.Cleanup(func() { pluginRegistry = nil })
	return reg
}

// ---------------------------------------------------------------------------
// Dispatch fires Created exactly once for a new entry.
// ---------------------------------------------------------------------------

func TestPlugin_FiresCreatedOnNewEntry(t *testing.T) {
	resetState(t)
	withMockStore(t)
	p := &recordingPlugin{name: "rec"}
	reg := installRegistry(t, p)

	ds := newDependencyState()
	entry := &TransformedEntry{
		DN:      "cn=eagle,ou=groups,dc=example,dc=org",
		Content: map[string]interface{}{"objectClass": "posixGroup"},
	}
	ds.handleEntry(entry, nil, "groups-search")

	reg.Wait()

	got := p.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 dispatched event; got %d", len(got))
	}
	if got[0].Op != SyncOpCreated {
		t.Errorf("expected SyncOpCreated; got %q", got[0].Op)
	}
	if got[0].DN != entry.DN {
		t.Errorf("DN mismatch: %q", got[0].DN)
	}
	if got[0].SearchID != "groups-search" {
		t.Errorf("SearchID not propagated: %q", got[0].SearchID)
	}
	if got[0].Timestamp.IsZero() {
		t.Errorf("Timestamp should be set")
	}
}

// ---------------------------------------------------------------------------
// A re-sync of the same DN dispatches Updated, not Created. This exercises
// the restart-safety path: after ldap-sync restarts, every search re-runs
// from scratch, and every group will appear fresh in searchResults; but
// target LDAP already has the entry, so storeDestinationLDAP returns
// SyncOpUpdated and plugins must see Updated, not Created again.
// ---------------------------------------------------------------------------

func TestPlugin_ResyncFiresUpdated(t *testing.T) {
	resetState(t)
	withMockStore(t)
	p := &recordingPlugin{name: "rec"}
	reg := installRegistry(t, p)

	ds := newDependencyState()
	entry := &TransformedEntry{
		DN:      "cn=falcon,ou=groups,dc=example,dc=org",
		Content: map[string]interface{}{"objectClass": "posixGroup"},
	}
	ds.handleEntry(entry, nil, "groups-search")
	reg.Wait()

	// Simulate a restart: dependencyState is rebuilt, but target LDAP still
	// has the entry, so the mock store reports SyncOpUpdated on the next write.
	ds = newDependencyState()
	ds.handleEntry(entry, nil, "groups-search")
	reg.Wait()

	got := p.snapshot()
	if len(got) != 2 {
		t.Fatalf("expected 2 events (created + updated); got %d", len(got))
	}
	if got[0].Op != SyncOpCreated {
		t.Errorf("first event should be Created; got %q", got[0].Op)
	}
	if got[1].Op != SyncOpUpdated {
		t.Errorf("second event should be Updated; got %q", got[1].Op)
	}
}

// ---------------------------------------------------------------------------
// Match filters dispatched events. A plugin that only matches one DN must
// not see events for other DNs.
// ---------------------------------------------------------------------------

func TestPlugin_MatchFilters(t *testing.T) {
	resetState(t)
	withMockStore(t)
	wanted := "cn=eagle,ou=groups,dc=example,dc=org"
	p := &recordingPlugin{name: "rec", matchDN: wanted}
	reg := installRegistry(t, p)

	ds := newDependencyState()
	ds.handleEntry(&TransformedEntry{
		DN:      wanted,
		Content: map[string]interface{}{"objectClass": "posixGroup"},
	}, nil, "")
	ds.handleEntry(&TransformedEntry{
		DN:      "uid=alice,ou=users,dc=example,dc=org",
		Content: map[string]interface{}{"objectClass": "inetOrgPerson"},
	}, nil, "")
	reg.Wait()

	got := p.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 matched event; got %d", len(got))
	}
	if got[0].DN != wanted {
		t.Errorf("matched wrong DN: %q", got[0].DN)
	}
}

// ---------------------------------------------------------------------------
// Plugin Apply failures retry up to MaxAttempts and do not block the sync
// pipeline. The mockStore must record the LDAP write immediately, even
// while the plugin is still retrying in the background.
// ---------------------------------------------------------------------------

func TestPlugin_FailureDoesNotBlockSync(t *testing.T) {
	resetState(t)
	ms := withMockStore(t)
	p := &recordingPlugin{name: "rec"}
	p.failuresLeft.Store(2) // two transient failures, succeeds on attempt 3
	reg := installRegistry(t, p)

	ds := newDependencyState()
	entry := &TransformedEntry{
		DN:      "cn=eagle,ou=groups,dc=example,dc=org",
		Content: map[string]interface{}{"objectClass": "posixGroup"},
	}
	ds.handleEntry(entry, nil, "groups-search")

	// Sync write is synchronous in handleEntry — should already be recorded.
	if n := len(ms.entries()); n != 1 {
		t.Fatalf("LDAP write should not be blocked by plugin retries; got %d", n)
	}

	reg.Wait()
	got := p.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected plugin to eventually succeed; got %d events", len(got))
	}
	if p.failuresLeft.Load() != 0 {
		t.Errorf("expected all simulated failures consumed; %d left", p.failuresLeft.Load())
	}
}

// ---------------------------------------------------------------------------
// Plugin permanent failure: exhausts retries and gives up without crashing.
// ---------------------------------------------------------------------------

func TestPlugin_PermanentFailureExhaustsRetries(t *testing.T) {
	resetState(t)
	withMockStore(t)
	p := &recordingPlugin{name: "rec"}
	p.failuresLeft.Store(1000) // far more than MaxAttempts; will never succeed
	reg := installRegistry(t, p)

	ds := newDependencyState()
	ds.handleEntry(&TransformedEntry{
		DN:      "cn=eagle,ou=groups,dc=example,dc=org",
		Content: map[string]interface{}{"objectClass": "posixGroup"},
	}, nil, "groups-search")

	// Wait deterministically rather than sleeping.
	done := make(chan struct{})
	go func() { reg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("registry did not finish after MaxAttempts")
	}

	if len(p.snapshot()) != 0 {
		t.Errorf("plugin should not have recorded any successful event")
	}
}

// ---------------------------------------------------------------------------
// markSyncedAndRelease called for an already-synced DN must not re-dispatch.
// This guards the early-exit path: the second call with op != "" still
// returns before reaching the dispatch site, otherwise plugins would fire
// twice for the same write.
// ---------------------------------------------------------------------------

func TestPlugin_NoDispatchOnAlreadySynced(t *testing.T) {
	resetState(t)
	withMockStore(t)
	p := &recordingPlugin{name: "rec"}
	reg := installRegistry(t, p)

	ds := newDependencyState()
	entry := &TransformedEntry{
		DN:      "cn=eagle,ou=groups,dc=example,dc=org",
		Content: map[string]interface{}{"objectClass": "posixGroup"},
	}
	ds.handleEntry(entry, nil, "groups-search")
	reg.Wait()

	// Direct second call simulating a stale release path.
	ds.markSyncedAndRelease(entry.DN, "groups-search", entry.Content, SyncOpUpdated)
	reg.Wait()

	got := p.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 dispatch despite duplicate markSynced; got %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// Empty op is a sentinel meaning "no real write happened" (e.g. the
// transitive markSync calls used during dependency-release recursion).
// Such calls must not dispatch.
// ---------------------------------------------------------------------------

func TestPlugin_EmptyOpDoesNotDispatch(t *testing.T) {
	resetState(t)
	withMockStore(t)
	p := &recordingPlugin{name: "rec"}
	reg := installRegistry(t, p)

	ds := newDependencyState()
	ds.markSyncedAndRelease("cn=eagle,ou=groups,dc=example,dc=org", "groups-search", nil, "")
	reg.Wait()

	if n := len(p.snapshot()); n != 0 {
		t.Errorf("empty op should not dispatch; got %d events", n)
	}
}
