package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakePVCClient is a minimal in-memory pvcClient for tests. It enforces:
// label-selector listing, AlreadyExists on duplicate names, and concurrent
// safety so the race tests run deterministically.
type fakePVCClient struct {
	mu     sync.Mutex
	pvcs   map[string]*corev1.PersistentVolumeClaim // keyed by ns/name
	listErr error
	createErr error
}

func newFakeClient() *fakePVCClient {
	return &fakePVCClient{pvcs: map[string]*corev1.PersistentVolumeClaim{}}
}

func (f *fakePVCClient) key(namespace, name string) string { return namespace + "/" + name }

func (f *fakePVCClient) List(_ context.Context, namespace, selector string) ([]corev1.PersistentVolumeClaim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}

	wantKey, wantVal, hasSelector := strings.Cut(selector, "=")
	out := []corev1.PersistentVolumeClaim{}
	for _, pvc := range f.pvcs {
		if pvc.Namespace != namespace {
			continue
		}
		if hasSelector {
			if pvc.Labels[wantKey] != wantVal {
				continue
			}
		}
		out = append(out, *pvc)
	}
	return out, nil
}

func (f *fakePVCClient) Create(_ context.Context, namespace string, pvc *corev1.PersistentVolumeClaim) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	k := f.key(namespace, pvc.Name)
	if _, exists := f.pvcs[k]; exists {
		return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "persistentvolumeclaims"}, pvc.Name)
	}
	cp := pvc.DeepCopy()
	cp.Namespace = namespace
	f.pvcs[k] = cp
	return nil
}

func (f *fakePVCClient) snapshot() []corev1.PersistentVolumeClaim {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]corev1.PersistentVolumeClaim, 0, len(f.pvcs))
	for _, p := range f.pvcs {
		out = append(out, *p)
	}
	return out
}

func newTestPlugin(t *testing.T, client pvcClient, overrides ...func(*pvcGroupConfig)) *pvcGroupPlugin {
	t.Helper()
	cfg := pvcGroupConfig{Namespace: "user-workspaces"}
	for _, fn := range overrides {
		fn(&cfg)
	}
	p, err := newPVCGroupPlugin(cfg, client)
	if err != nil {
		t.Fatalf("newPVCGroupPlugin: %v", err)
	}
	return p
}

func eventForGroup(name string, op SyncOp) SyncEvent {
	return SyncEvent{
		DN: "cn=" + name + ",ou=groups,dc=example,dc=org",
		Content: map[string]interface{}{
			"cn":          name,
			"objectClass": []string{"top", "groupOfNames"},
		},
		Op: op,
	}
}

// ---------------------------------------------------------------------------
// Match
// ---------------------------------------------------------------------------

func TestPVC_Match_OnlyGroupOfNames(t *testing.T) {
	p := newTestPlugin(t, newFakeClient())

	if !p.Match(eventForGroup("eagle", SyncOpCreated)) {
		t.Errorf("groupOfNames entry should match")
	}

	posix := SyncEvent{
		DN: "cn=eagle,ou=groups,dc=example,dc=org",
		Content: map[string]interface{}{
			"cn":          "eagle",
			"objectClass": []string{"top", "posixGroup"},
		},
		Op: SyncOpCreated,
	}
	if p.Match(posix) {
		t.Errorf("posixGroup entry must NOT match — that's the wrong attribute")
	}

	user := SyncEvent{
		DN: "uid=alice,ou=users,dc=example,dc=org",
		Content: map[string]interface{}{
			"cn":          "Alice",
			"objectClass": []string{"top", "inetOrgPerson"},
		},
		Op: SyncOpCreated,
	}
	if p.Match(user) {
		t.Errorf("user entry must NOT match")
	}
}

func TestPVC_Match_ObjectClassShapes(t *testing.T) {
	p := newTestPlugin(t, newFakeClient())

	cases := []struct {
		name string
		oc   interface{}
		want bool
	}{
		{"slice-of-string", []string{"top", "groupOfNames"}, true},
		{"slice-of-interface", []interface{}{"top", "groupOfNames"}, true},
		{"bare-string", "groupOfNames", true},
		{"case-insensitive", []string{"GroupOfNames"}, true},
		{"no-match", []string{"top", "device"}, false},
		{"missing", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := map[string]interface{}{"cn": "x"}
			if tc.oc != nil {
				content["objectClass"] = tc.oc
			}
			ev := SyncEvent{Op: SyncOpCreated, DN: "cn=x,ou=groups", Content: content}
			if got := p.Match(ev); got != tc.want {
				t.Errorf("Match(%v) = %v; want %v", tc.oc, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Apply: create on first sync
// ---------------------------------------------------------------------------

func TestPVC_Apply_CreatesWithCorrectLabel(t *testing.T) {
	resetState(t)
	fc := newFakeClient()
	p := newTestPlugin(t, fc, func(c *pvcGroupConfig) {
		c.StorageClass = "standard"
		c.Size = "5Gi"
	})

	if err := p.Apply(context.Background(), eventForGroup("eagle", SyncOpCreated)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := fc.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 PVC; got %d", len(got))
	}
	pvc := got[0]
	if pvc.Namespace != "user-workspaces" {
		t.Errorf("namespace = %q", pvc.Namespace)
	}
	if pvc.Name != "group-eagle" {
		t.Errorf("name = %q; want group-eagle", pvc.Name)
	}
	if pvc.Labels[defaultGroupLabelKey] != "eagle" {
		t.Errorf("label %s = %q; want eagle", defaultGroupLabelKey, pvc.Labels[defaultGroupLabelKey])
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "standard" {
		t.Errorf("storageClassName = %v", pvc.Spec.StorageClassName)
	}
	q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if q.String() != "5Gi" {
		t.Errorf("storage request = %q; want 5Gi", q.String())
	}
}

// ---------------------------------------------------------------------------
// Apply: idempotent — second sync of the same group does not duplicate.
// This is THE key requirement: existence determined by label, not by name.
// ---------------------------------------------------------------------------

func TestPVC_Apply_IdempotentByLabel(t *testing.T) {
	fc := newFakeClient()
	p := newTestPlugin(t, fc)

	// First sync creates.
	if err := p.Apply(context.Background(), eventForGroup("falcon", SyncOpCreated)); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	// Second sync (e.g. after a restart) reports SyncOpUpdated.
	if err := p.Apply(context.Background(), eventForGroup("falcon", SyncOpUpdated)); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	got := fc.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 PVC after re-sync; got %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// Apply: pre-existing PVC with the right label but a different name is
// respected. This guards against treating the name as the source of truth.
// ---------------------------------------------------------------------------

func TestPVC_Apply_RespectsPreExistingByLabel(t *testing.T) {
	fc := newFakeClient()
	// Pre-seed an out-of-band PVC with the right label but a custom name.
	fc.pvcs["user-workspaces/legacy-eagle-volume"] = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "legacy-eagle-volume",
			Namespace: "user-workspaces",
			Labels:    map[string]string{defaultGroupLabelKey: "eagle"},
		},
	}

	p := newTestPlugin(t, fc)
	if err := p.Apply(context.Background(), eventForGroup("eagle", SyncOpCreated)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := fc.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 PVC (pre-existing one); got %d", len(got))
	}
	if got[0].Name != "legacy-eagle-volume" {
		t.Errorf("expected pre-existing PVC to be kept; got %q", got[0].Name)
	}
}

// ---------------------------------------------------------------------------
// Apply: AlreadyExists on Create is treated as success. Reproduces the race
// where two dispatches simultaneously list (both see zero) then both Create
// — one wins, the other gets AlreadyExists, and we must not return an error.
// ---------------------------------------------------------------------------

func TestPVC_Apply_AlreadyExistsIsTolerated(t *testing.T) {
	fc := newFakeClient()
	// Seed a PVC with the same name but a DIFFERENT label, so the label
	// listing returns empty and the plugin proceeds to Create — which then
	// hits AlreadyExists.
	fc.pvcs["user-workspaces/group-eagle"] = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "group-eagle",
			Namespace: "user-workspaces",
			Labels:    map[string]string{"some/other": "label"},
		},
	}
	p := newTestPlugin(t, fc)

	if err := p.Apply(context.Background(), eventForGroup("eagle", SyncOpCreated)); err != nil {
		t.Errorf("AlreadyExists must be tolerated; got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Sanitization
// ---------------------------------------------------------------------------

func TestPVC_Sanitize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"eagle", "group-eagle"},
		{"unc:app:renci:eagle", "group-unc-app-renci-eagle"},
		{"Foo_Bar", "group-foo-bar"},
		{"--leading-trailing--", "group-leading-trailing"},
		{"", "group"},
	}
	for _, tc := range cases {
		got := sanitizePVCName("group-", tc.in)
		if got != tc.want {
			t.Errorf("sanitizePVCName(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// groupNameFromContent: cn attribute, slice forms, DN fallback
// ---------------------------------------------------------------------------

func TestPVC_GroupNameFromContent(t *testing.T) {
	cases := []struct {
		name    string
		dn      string
		content map[string]interface{}
		want    string
	}{
		{"cn-string", "cn=foo,ou=groups", map[string]interface{}{"cn": "eagle"}, "eagle"},
		{"cn-slice-string", "cn=foo,ou=groups", map[string]interface{}{"cn": []string{"eagle"}}, "eagle"},
		{"cn-slice-interface", "cn=foo,ou=groups", map[string]interface{}{"cn": []interface{}{"eagle"}}, "eagle"},
		{"dn-fallback", "cn=eagle,ou=groups", map[string]interface{}{}, "eagle"},
		{"empty", "", map[string]interface{}{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := groupNameFromContent(tc.dn, tc.content); got != tc.want {
				t.Errorf("got %q; want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Apply: list error is returned (not silently swallowed) so the registry
// retries.
// ---------------------------------------------------------------------------

func TestPVC_Apply_ListErrorPropagates(t *testing.T) {
	fc := newFakeClient()
	fc.listErr = errors.New("apiserver unreachable")
	p := newTestPlugin(t, fc)

	err := p.Apply(context.Background(), eventForGroup("eagle", SyncOpCreated))
	if err == nil {
		t.Fatal("expected error from list failure")
	}
	if !strings.Contains(err.Error(), "apiserver unreachable") {
		t.Errorf("error missing underlying cause: %v", err)
	}
}

// ---------------------------------------------------------------------------
// End-to-end: a SyncEvent dispatched through the registry creates a PVC
// exactly once even on repeated dispatch (label-based idempotency).
// ---------------------------------------------------------------------------

func TestPVC_Registry_DispatchCreatesOnce(t *testing.T) {
	resetState(t)
	fc := newFakeClient()
	p := newTestPlugin(t, fc)

	reg := NewRegistry(PluginRetry{MaxAttempts: 1, InitialDelayMs: 1, MaxDelayMs: 1})
	reg.Register(p)
	pluginRegistry = reg
	t.Cleanup(func() { pluginRegistry = nil })

	for i := 0; i < 3; i++ {
		dispatchSyncEvent(eventForGroup("eagle", SyncOpUpdated))
	}
	reg.Wait()

	if got := len(fc.snapshot()); got != 1 {
		t.Fatalf("expected exactly 1 PVC after repeated dispatch; got %d", got)
	}
}
