package main

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/go-ldap/ldap/v3"
)

// newPipeConn returns a live *ldap.Conn backed by an in-memory pipe, plus the
// peer end so the caller can keep it open. It never speaks LDAP; it exists so
// tests can exercise connection bookkeeping with real, closable Conn values.
func newPipeConn(t *testing.T) *ldap.Conn {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { server.Close() })

	l := ldap.NewConn(client, false)
	l.Start()
	return l
}

// resetSourceConn clears the shared connection without closing it, so tests
// start from a known state and don't leak state into each other.
func resetSourceConn(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		sourceConnMu.Lock()
		sourceConn = nil
		sourceConnMu.Unlock()
	})

	sourceConnMu.Lock()
	sourceConn = nil
	sourceConnMu.Unlock()
}

func TestIsConnectionError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"network error", ldap.NewError(ldap.ErrorNetwork, errors.New("broken pipe")), true},
		{"unexpected message", ldap.NewError(ldap.ErrorUnexpectedMessage, errors.New("x")), true},
		{"unexpected response", ldap.NewError(ldap.ErrorUnexpectedResponse, errors.New("x")), true},
		{"no such object", ldap.NewError(ldap.LDAPResultNoSuchObject, errors.New("x")), false},
		{"size limit exceeded", ldap.NewError(ldap.LDAPResultSizeLimitExceeded, errors.New("x")), false},
		{"invalid credentials", ldap.NewError(ldap.LDAPResultInvalidCredentials, errors.New("x")), false},
		{"non-ldap error", errors.New("dial tcp: timeout"), true},
		{"wrapped network error", fmt.Errorf("search failed: %w", ldap.NewError(ldap.ErrorNetwork, errors.New("x"))), true},
		{"wrapped server error", fmt.Errorf("search failed: %w", ldap.NewError(ldap.LDAPResultNoSuchObject, errors.New("x"))), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isConnectionError(tt.err); got != tt.want {
				t.Errorf("isConnectionError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestDropSourceConn_ClearsMatching(t *testing.T) {
	resetSourceConn(t)

	l := newPipeConn(t)
	sourceConnMu.Lock()
	sourceConn = l
	sourceConnMu.Unlock()

	dropSourceConn(l)

	sourceConnMu.Lock()
	got := sourceConn
	sourceConnMu.Unlock()
	if got != nil {
		t.Error("dropSourceConn did not clear the shared connection")
	}
}

// Two goroutines can fail on the same stale connection concurrently. The first
// drops and the second redials; the straggler's drop must not discard the
// fresh connection the second one installed.
func TestDropSourceConn_IgnoresReplacement(t *testing.T) {
	resetSourceConn(t)

	stale := newPipeConn(t)
	fresh := newPipeConn(t)

	sourceConnMu.Lock()
	sourceConn = stale
	sourceConnMu.Unlock()

	dropSourceConn(stale) // first goroutine notices the failure

	sourceConnMu.Lock()
	sourceConn = fresh // second goroutine redials
	sourceConnMu.Unlock()

	dropSourceConn(stale) // straggler reports the same stale failure

	sourceConnMu.Lock()
	got := sourceConn
	sourceConnMu.Unlock()
	if got != fresh {
		t.Error("straggler discarded the replacement connection")
	}
}

// A closed connection must not be handed out again.
func TestGetSourceConn_RejectsClosedConn(t *testing.T) {
	resetSourceConn(t)

	l := newPipeConn(t)
	l.Close()

	sourceConnMu.Lock()
	sourceConn = l
	sourceConnMu.Unlock()

	if !l.IsClosing() {
		t.Fatal("precondition: connection should report as closing")
	}

	// config.Source.URL is unset, so the redial fails; the point is that
	// getSourceConn attempts one rather than returning the dead connection.
	got, err := getSourceConn()
	if err == nil && got == l {
		t.Error("getSourceConn returned a closed connection")
	}
}
