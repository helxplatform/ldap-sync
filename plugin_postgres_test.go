package main

import (
	"context"
	"sort"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakePGAdmin is an in-memory pgAdmin. It tracks role existence, the user's
// granted managed roles, and records grant/revoke calls for assertions.
type fakePGAdmin struct {
	mu          sync.Mutex
	roles       map[string]bool            // role -> exists
	memberships map[string]map[string]bool // user -> set of roles
	grants      []string
	revokes     []string
}

func newFakePGAdmin() *fakePGAdmin {
	return &fakePGAdmin{
		roles:       map[string]bool{},
		memberships: map[string]map[string]bool{},
	}
}

func (f *fakePGAdmin) EnsureRole(_ context.Context, role string, _ []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roles[role] = true
	return nil
}

func (f *fakePGAdmin) EnsureLogin(_ context.Context, user, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roles[user] = true
	if f.memberships[user] == nil {
		f.memberships[user] = map[string]bool{}
	}
	return nil
}

func (f *fakePGAdmin) GrantedManagedRoles(_ context.Context, user string, managed []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for _, role := range managed {
		if f.memberships[user][role] {
			out = append(out, role)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakePGAdmin) Grant(_ context.Context, role, user string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.memberships[user] == nil {
		f.memberships[user] = map[string]bool{}
	}
	f.memberships[user][role] = true
	f.grants = append(f.grants, role)
	return nil
}

func (f *fakePGAdmin) Revoke(_ context.Context, role, user string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.memberships[user], role)
	f.revokes = append(f.revokes, role)
	return nil
}

// fakeSecretClient is a minimal in-memory secretClient.
type fakeSecretClient struct {
	mu        sync.Mutex
	secrets   map[string]*corev1.Secret // keyed by ns/name
	createErr error
}

func newFakeSecretClient() *fakeSecretClient {
	return &fakeSecretClient{secrets: map[string]*corev1.Secret{}}
}

func (f *fakeSecretClient) key(ns, name string) string { return ns + "/" + name }

func (f *fakeSecretClient) Get(_ context.Context, ns, name string) (*corev1.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.secrets[f.key(ns, name)]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
	}
	return s.DeepCopy(), nil
}

func (f *fakeSecretClient) Create(_ context.Context, ns string, s *corev1.Secret) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	k := f.key(ns, s.Name)
	if _, exists := f.secrets[k]; exists {
		return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, s.Name)
	}
	cp := s.DeepCopy()
	cp.Namespace = ns
	f.secrets[k] = cp
	return nil
}

func (f *fakeSecretClient) Update(_ context.Context, ns string, s *corev1.Secret) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secrets[f.key(ns, s.Name)] = s.DeepCopy()
	return nil
}

func testPGConfig() postgresUserConfig {
	return postgresUserConfig{
		Host:     "pg",
		Port:     5432,
		Username: "postgres",
		Password: "admin",
		Database: "app",
		SSLMode:  "disable",
		Namespace: "ns",
		GroupRoles: map[string]pgRoleSpec{
			"users": {Role: "viewer", Privileges: []string{"GRANT CONNECT ON DATABASE app TO viewer"}},
			"admin": {Role: "db_admin", Privileges: []string{"GRANT ALL PRIVILEGES ON DATABASE app TO db_admin"}},
		},
	}
}

func newTestPGPlugin(t *testing.T, admin pgAdmin, secrets secretClient) *postgresUserPlugin {
	t.Helper()
	p, err := newPostgresUserPlugin(testPGConfig(), admin, secrets)
	if err != nil {
		t.Fatalf("newPostgresUserPlugin: %v", err)
	}
	return p
}

func userEvent(uid string, groups []interface{}) SyncEvent {
	return SyncEvent{
		Op:  SyncOpCreated,
		DN:  "uid=" + uid + ",ou=users,dc=example,dc=org",
		Content: map[string]interface{}{
			"uid":         uid,
			"objectClass": []string{"top", "inetOrgPerson", "posixAccount", "helxUser"},
			"groups":      groups,
		},
	}
}

func TestPG_RequiresUsersGroup(t *testing.T) {
	cfg := testPGConfig()
	delete(cfg.GroupRoles, "users")
	if _, err := newPostgresUserPlugin(cfg, newFakePGAdmin(), newFakeSecretClient()); err == nil {
		t.Fatal("expected error when groupRoles is missing the required users group")
	}
}

func TestPG_Match(t *testing.T) {
	p := newTestPGPlugin(t, newFakePGAdmin(), newFakeSecretClient())

	cases := []struct {
		name string
		e    SyncEvent
		want bool
	}{
		{"user helxUser", userEvent("jdoe", nil), true},
		{"group", SyncEvent{Op: SyncOpCreated, Content: map[string]interface{}{"objectClass": []string{"top", "groupOfNames"}}}, false},
		{"plain person", SyncEvent{Op: SyncOpCreated, Content: map[string]interface{}{"objectClass": []string{"top", "inetOrgPerson"}}}, false},
		{"no op", SyncEvent{Op: SyncOp("deleted"), Content: map[string]interface{}{"objectClass": "helxUser"}}, false},
	}
	for _, tc := range cases {
		if got := p.Match(tc.e); got != tc.want {
			t.Errorf("%s: Match = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPG_FirstCreate(t *testing.T) {
	admin := newFakePGAdmin()
	secrets := newFakeSecretClient()
	p := newTestPGPlugin(t, admin, secrets)

	// jdoe in admin group -> should get viewer (from required users) + db_admin.
	err := p.Apply(context.Background(), userEvent("jdoe", []interface{}{"admin"}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	sort.Strings(admin.grants)
	want := []string{"db_admin", "viewer"}
	if !equalStrs(admin.grants, want) {
		t.Errorf("grants = %v, want %v", admin.grants, want)
	}
	if len(admin.revokes) != 0 {
		t.Errorf("unexpected revokes: %v", admin.revokes)
	}

	s, err := secrets.Get(context.Background(), "ns", "pg_creds_jdoe")
	if err != nil {
		t.Fatalf("expected secret created: %v", err)
	}
	if got := string(s.Data["user"]); got != "jdoe" {
		t.Errorf("secret user = %q, want jdoe", got)
	}
	if len(s.Data["password"]) == 0 {
		t.Error("secret password is empty")
	}
	wantURI := "postgres://jdoe:" + string(s.Data["password"]) + "@pg:5432/app?sslmode=disable"
	if got := string(s.Data["uri"]); got != wantURI {
		t.Errorf("uri = %q, want %q", got, wantURI)
	}
	for _, k := range []string{"dbname", "host", "password", "port", "uri", "user"} {
		if _, ok := s.Data[k]; !ok {
			t.Errorf("secret missing key %q", k)
		}
	}
}

func TestPG_DefaultUsersRoleAlways(t *testing.T) {
	admin := newFakePGAdmin()
	p := newTestPGPlugin(t, admin, newFakeSecretClient())

	// No groups at all -> still gets the default viewer role.
	if err := p.Apply(context.Background(), userEvent("nobody", nil)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !equalStrs(admin.grants, []string{"viewer"}) {
		t.Errorf("grants = %v, want [viewer]", admin.grants)
	}
}

func TestPG_PasswordReusedOnResync(t *testing.T) {
	admin := newFakePGAdmin()
	secrets := newFakeSecretClient()
	p := newTestPGPlugin(t, admin, secrets)

	if err := p.Apply(context.Background(), userEvent("jdoe", nil)); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	s1, _ := secrets.Get(context.Background(), "ns", "pg_creds_jdoe")
	pw1 := string(s1.Data["password"])

	if err := p.Apply(context.Background(), userEvent("jdoe", nil)); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	s2, _ := secrets.Get(context.Background(), "ns", "pg_creds_jdoe")
	pw2 := string(s2.Data["password"])

	if pw1 != pw2 {
		t.Errorf("password rotated on resync: %q -> %q", pw1, pw2)
	}
}

func TestPG_ReconcileRevokesDroppedGroup(t *testing.T) {
	admin := newFakePGAdmin()
	secrets := newFakeSecretClient()
	p := newTestPGPlugin(t, admin, secrets)

	// Start in admin group.
	if err := p.Apply(context.Background(), userEvent("jdoe", []interface{}{"admin"})); err != nil {
		t.Fatalf("Apply 1: %v", err)
	}
	admin.grants = nil // reset to observe the second pass only.

	// Re-sync without admin group -> db_admin must be revoked, viewer kept.
	if err := p.Apply(context.Background(), userEvent("jdoe", nil)); err != nil {
		t.Fatalf("Apply 2: %v", err)
	}
	if !equalStrs(admin.revokes, []string{"db_admin"}) {
		t.Errorf("revokes = %v, want [db_admin]", admin.revokes)
	}
	if len(admin.grants) != 0 {
		t.Errorf("unexpected grants on reconcile: %v", admin.grants)
	}
	if !admin.memberships["jdoe"]["viewer"] {
		t.Error("viewer should remain granted")
	}
	if admin.memberships["jdoe"]["db_admin"] {
		t.Error("db_admin should have been revoked")
	}
}

func TestPG_CreateAlreadyExistsBenign(t *testing.T) {
	admin := newFakePGAdmin()
	secrets := newFakeSecretClient()
	secrets.createErr = apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, "pg_creds_jdoe")
	p := newTestPGPlugin(t, admin, secrets)

	if err := p.Apply(context.Background(), userEvent("jdoe", nil)); err != nil {
		t.Fatalf("AlreadyExists should be benign, got: %v", err)
	}
}

func TestPG_SanitizeIdentifier(t *testing.T) {
	cases := map[string]string{
		"jdoe":        "jdoe",
		"J.Doe":       "j_doe",
		"a-b-c":       "a_b_c",
		"  spaced  ":  "spaced",
		"weird!!name": "weird_name",
		"":            "",
	}
	for in, want := range cases {
		if got := sanitizePGIdentifier(in); got != want {
			t.Errorf("sanitizePGIdentifier(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPG_UserNameFromContent(t *testing.T) {
	if got := userNameFromContent("uid=jdoe,ou=users", map[string]interface{}{"uid": "jdoe"}); got != "jdoe" {
		t.Errorf("uid attr: got %q", got)
	}
	// Fallback to DN RDN when uid attribute absent.
	if got := userNameFromContent("uid=fromdn,ou=users", map[string]interface{}{}); got != "fromdn" {
		t.Errorf("dn fallback: got %q", got)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
