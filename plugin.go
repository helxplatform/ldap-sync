package main

import (
	"context"
	"sync"
	"time"
)

type SyncOp string

const (
	SyncOpCreated SyncOp = "created"
	SyncOpUpdated SyncOp = "updated"
)

// SyncEvent is emitted after a successful target-LDAP write. Plugins receive
// it via Registry.Dispatch and may take side effects (e.g. PVC creation).
// Content is the resolved entry written to target LDAP. Plugins must treat
// it as read-only — the same map may be passed to multiple plugins.
type SyncEvent struct {
	SearchID  string
	DN        string
	Content   map[string]interface{}
	Op        SyncOp
	Timestamp time.Time
}

// Plugin runs out-of-band side effects in response to successful target-LDAP
// writes. Implementations must be safe for concurrent use: Apply may be
// invoked from many goroutines simultaneously.
type Plugin interface {
	Name() string
	Match(SyncEvent) bool
	Apply(context.Context, SyncEvent) error
}

// PluginRetry controls per-plugin retry behavior in the registry.
type PluginRetry struct {
	MaxAttempts    int
	InitialDelayMs int
	MaxDelayMs     int
}

func (r PluginRetry) withDefaults() PluginRetry {
	if r.MaxAttempts <= 0 {
		r.MaxAttempts = 5
	}
	if r.InitialDelayMs <= 0 {
		r.InitialDelayMs = 500
	}
	if r.MaxDelayMs <= 0 {
		r.MaxDelayMs = 60000
	}
	return r
}

// Registry holds the active plugins and dispatches events to them.
// Dispatch is non-blocking: each plugin runs in its own goroutine with a
// bounded retry loop. Plugin failures are logged but never affect the
// caller (in particular, never block markSyncedAndRelease or LDAP sync).
type Registry struct {
	mu      sync.RWMutex
	plugins []Plugin
	retry   PluginRetry
	wg      sync.WaitGroup // tracks in-flight Apply goroutines, used by tests
}

func NewRegistry(retry PluginRetry) *Registry {
	return &Registry{retry: retry.withDefaults()}
}

func (r *Registry) Register(p Plugin) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plugins = append(r.plugins, p)
}

// Dispatch fans the event out to every matching plugin in its own goroutine.
// Returns immediately. The caller does not need to (and must not) hold any
// dependency-state mutex while invoking this.
func (r *Registry) Dispatch(event SyncEvent) {
	r.mu.RLock()
	matching := make([]Plugin, 0, len(r.plugins))
	for _, p := range r.plugins {
		if p.Match(event) {
			matching = append(matching, p)
		}
	}
	retry := r.retry
	r.mu.RUnlock()

	if len(matching) == 0 {
		return
	}

	for _, p := range matching {
		p := p
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.runWithRetry(p, event, retry)
		}()
	}
}

// Wait blocks until all in-flight plugin goroutines complete. Test-only.
func (r *Registry) Wait() {
	r.wg.Wait()
}

func (r *Registry) runWithRetry(p Plugin, event SyncEvent, retry PluginRetry) {
	delay := time.Duration(retry.InitialDelayMs) * time.Millisecond
	maxDelay := time.Duration(retry.MaxDelayMs) * time.Millisecond

	for attempt := 1; attempt <= retry.MaxAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := p.Apply(ctx, event)
		cancel()
		if err == nil {
			if logger != nil {
				logger.Debug("Plugin applied",
					"Plugin", p.Name(),
					"DN", event.DN,
					"Op", event.Op,
					"Attempt", attempt,
				)
			}
			return
		}
		if logger != nil {
			logger.Warn("Plugin apply failed",
				"Plugin", p.Name(),
				"DN", event.DN,
				"Op", event.Op,
				"Attempt", attempt,
				"MaxAttempts", retry.MaxAttempts,
				"Err", err,
			)
		}
		if attempt == retry.MaxAttempts {
			if logger != nil {
				logger.Error("Plugin apply exhausted retries",
					"Plugin", p.Name(),
					"DN", event.DN,
					"Op", event.Op,
					"Err", err,
				)
			}
			return
		}
		time.Sleep(delay)
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

// pluginRegistry is the package-level registry consulted by markSyncedAndRelease.
// nil means "no plugins configured" — Dispatch is a no-op.
var pluginRegistry *Registry

// dispatchSyncEvent is the indirection point used by markSyncedAndRelease so
// tests can swap the registry behavior without touching the global.
var dispatchSyncEvent = func(event SyncEvent) {
	if pluginRegistry == nil {
		return
	}
	pluginRegistry.Dispatch(event)
}
