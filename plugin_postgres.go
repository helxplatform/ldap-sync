package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/lib/pq"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// pgRoleSpec maps an LDAP group to a Postgres role and the privilege
// statements that define what that role can do. Privileges are plain SQL run
// verbatim when the role is first ensured; they are operator-trusted config.
type pgRoleSpec struct {
	Role       string   `yaml:"role"`
	Privileges []string `yaml:"privileges"`
}

// postgresUserConfig is the YAML-decodable shape of the postgres-user plugin's
// options block.
type postgresUserConfig struct {
	// Postgres admin connection.
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	Database string `yaml:"database"`
	SSLMode  string `yaml:"sslmode"`

	// Behavior.
	MatchObjectClass string                `yaml:"matchObjectClass"`
	GroupRoles       map[string]pgRoleSpec `yaml:"groupRoles"`

	// Kubernetes secret.
	Namespace    string            `yaml:"namespace"`
	Kubeconfig   string            `yaml:"kubeconfig"`
	UserLabelKey string            `yaml:"userLabelKey"`
	NamePrefix   string            `yaml:"namePrefix"`
	ExtraLabels  map[string]string `yaml:"extraLabels"`
}

const (
	defaultUserObjectClass = "helxUser"
	defaultUserLabelKey    = "helx.renci.org/user-name"
	defaultUserNamePrefix  = "pg-creds-"
	defaultPGSSLMode       = "disable"
	defaultPGPort          = 5432
	defaultUsersGroup      = "users"
)

func (c postgresUserConfig) withDefaults() postgresUserConfig {
	if c.MatchObjectClass == "" {
		c.MatchObjectClass = defaultUserObjectClass
	}
	if c.UserLabelKey == "" {
		c.UserLabelKey = defaultUserLabelKey
	}
	if c.NamePrefix == "" {
		c.NamePrefix = defaultUserNamePrefix
	}
	if c.SSLMode == "" {
		c.SSLMode = defaultPGSSLMode
	}
	if c.Port == 0 {
		c.Port = defaultPGPort
	}
	return c
}

// pgAdmin is the slice of Postgres admin operations the plugin uses. Tests
// substitute a fake implementing this surface so no real database is needed.
type pgAdmin interface {
	// EnsureRole creates a NOLOGIN group role if it doesn't exist, then runs
	// its privilege statements. Idempotent.
	EnsureRole(ctx context.Context, role string, privileges []string) error
	// EnsureLogin creates or alters a LOGIN role with the given password.
	EnsureLogin(ctx context.Context, user, password string) error
	// GrantedManagedRoles returns the subset of managed roles currently
	// granted to the user.
	GrantedManagedRoles(ctx context.Context, user string, managed []string) ([]string, error)
	Grant(ctx context.Context, role, user string) error
	Revoke(ctx context.Context, role, user string) error
}

// secretClient is the slice of the Kubernetes secrets API the plugin uses.
type secretClient interface {
	Get(ctx context.Context, namespace, name string) (*corev1.Secret, error)
	Create(ctx context.Context, namespace string, s *corev1.Secret) error
	Update(ctx context.Context, namespace string, s *corev1.Secret) error
}

type clientGoSecretClient struct {
	c kubernetes.Interface
}

func (k clientGoSecretClient) Get(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	return k.c.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (k clientGoSecretClient) Create(ctx context.Context, namespace string, s *corev1.Secret) error {
	_, err := k.c.CoreV1().Secrets(namespace).Create(ctx, s, metav1.CreateOptions{})
	return err
}

func (k clientGoSecretClient) Update(ctx context.Context, namespace string, s *corev1.Secret) error {
	_, err := k.c.CoreV1().Secrets(namespace).Update(ctx, s, metav1.UpdateOptions{})
	return err
}

// sqlPGAdmin implements pgAdmin against a real *sql.DB opened with lib/pq.
// DDL (CREATE/ALTER/GRANT) can't use bind parameters, so identifiers are
// quoted with pq.QuoteIdentifier and the password with pq.QuoteLiteral.
type sqlPGAdmin struct {
	db sqlExecQuerier
}

// sqlExecQuerier is the minimal surface of *sql.DB the admin needs.
type sqlExecQuerier interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sqlResult, error)
	QueryRowScanText(ctx context.Context, query string, args ...interface{}) (string, bool, error)
}

type sqlResult interface{}

func (a sqlPGAdmin) EnsureRole(ctx context.Context, role string, privileges []string) error {
	if err := a.createRoleIfMissing(ctx, role, false); err != nil {
		return err
	}
	for _, stmt := range privileges {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := a.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply privilege for role %q: %w", role, err)
		}
	}
	return nil
}

func (a sqlPGAdmin) EnsureLogin(ctx context.Context, user, password string) error {
	exists, err := a.roleExists(ctx, user)
	if err != nil {
		return err
	}
	verb := "CREATE"
	if exists {
		verb = "ALTER"
	}
	stmt := fmt.Sprintf("%s ROLE %s WITH LOGIN PASSWORD %s",
		verb, pq.QuoteIdentifier(user), pq.QuoteLiteral(password))
	if _, err := a.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("ensure login role %q: %w", user, err)
	}
	return nil
}

func (a sqlPGAdmin) createRoleIfMissing(ctx context.Context, role string, login bool) error {
	exists, err := a.roleExists(ctx, role)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	opt := "NOLOGIN"
	if login {
		opt = "LOGIN"
	}
	stmt := fmt.Sprintf("CREATE ROLE %s WITH %s", pq.QuoteIdentifier(role), opt)
	if _, err := a.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("create role %q: %w", role, err)
	}
	return nil
}

func (a sqlPGAdmin) roleExists(ctx context.Context, role string) (bool, error) {
	_, found, err := a.db.QueryRowScanText(ctx,
		"SELECT rolname FROM pg_roles WHERE rolname = $1", role)
	if err != nil {
		return false, fmt.Errorf("check role %q exists: %w", role, err)
	}
	return found, nil
}

func (a sqlPGAdmin) GrantedManagedRoles(ctx context.Context, user string, managed []string) ([]string, error) {
	granted := make([]string, 0, len(managed))
	for _, role := range managed {
		_, found, err := a.db.QueryRowScanText(ctx,
			`SELECT 1 FROM pg_auth_members m
			   JOIN pg_roles mr ON mr.oid = m.member
			   JOIN pg_roles rr ON rr.oid = m.roleid
			  WHERE mr.rolname = $1 AND rr.rolname = $2`, user, role)
		if err != nil {
			return nil, fmt.Errorf("check membership %q in %q: %w", user, role, err)
		}
		if found {
			granted = append(granted, role)
		}
	}
	return granted, nil
}

func (a sqlPGAdmin) Grant(ctx context.Context, role, user string) error {
	stmt := fmt.Sprintf("GRANT %s TO %s", pq.QuoteIdentifier(role), pq.QuoteIdentifier(user))
	if _, err := a.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("grant %q to %q: %w", role, user, err)
	}
	return nil
}

func (a sqlPGAdmin) Revoke(ctx context.Context, role, user string) error {
	stmt := fmt.Sprintf("REVOKE %s FROM %s", pq.QuoteIdentifier(role), pq.QuoteIdentifier(user))
	if _, err := a.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("revoke %q from %q: %w", role, user, err)
	}
	return nil
}

type postgresUserPlugin struct {
	cfg     postgresUserConfig
	admin   pgAdmin
	secrets secretClient

	// adminMu serializes admin DDL; concurrent catalog updates raise
	// "tuple concurrently updated".
	adminMu sync.Mutex
}

func newPostgresUserPlugin(cfg postgresUserConfig, admin pgAdmin, secrets secretClient) (*postgresUserPlugin, error) {
	cfg = cfg.withDefaults()
	if cfg.Namespace == "" {
		return nil, fmt.Errorf("postgres-user: namespace is required")
	}
	if cfg.Host == "" {
		return nil, fmt.Errorf("postgres-user: host is required")
	}
	if cfg.Database == "" {
		return nil, fmt.Errorf("postgres-user: database is required")
	}
	if _, ok := cfg.GroupRoles[defaultUsersGroup]; !ok {
		return nil, fmt.Errorf("postgres-user: groupRoles must include the required %q group", defaultUsersGroup)
	}
	return &postgresUserPlugin{cfg: cfg, admin: admin, secrets: secrets}, nil
}

func (p *postgresUserPlugin) Name() string { return "postgres-user" }

func (p *postgresUserPlugin) Match(e SyncEvent) bool {
	if e.Op != SyncOpCreated && e.Op != SyncOpUpdated {
		return false
	}
	return objectClassContains(e.Content, p.cfg.MatchObjectClass)
}

func (p *postgresUserPlugin) Apply(ctx context.Context, e SyncEvent) error {
	rawUser := userNameFromContent(e.DN, e.Content)
	user := sanitizePGIdentifier(rawUser)
	if user == "" {
		return fmt.Errorf("postgres-user: cannot determine username for DN %q", e.DN)
	}

	desiredRoles := p.desiredRoles(e.Content)

	// Resolve the password before touching the role so the role password
	// always matches what's stored in the secret. Reuse the existing secret's
	// password if present; otherwise generate a fresh one.
	secretName := sanitizeK8sName(p.cfg.NamePrefix + user)
	existing, err := p.secrets.Get(ctx, p.cfg.Namespace, secretName)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("postgres-user: get secret %q: %w", secretName, err)
	}
	secretExists := err == nil

	var password string
	if secretExists {
		password = string(existing.Data["password"])
	}
	if password == "" {
		password, err = genPassword()
		if err != nil {
			return fmt.Errorf("postgres-user: generate password: %w", err)
		}
	}

	// Ensure all configured group roles exist with their privileges, then
	// ensure the user login role, then reconcile memberships. Serialized so
	// concurrent events don't collide on shared catalog rows.
	if err := func() error {
		p.adminMu.Lock()
		defer p.adminMu.Unlock()
		for _, spec := range p.cfg.GroupRoles {
			if err := p.admin.EnsureRole(ctx, spec.Role, spec.Privileges); err != nil {
				return fmt.Errorf("postgres-user: %w", err)
			}
		}
		if err := p.admin.EnsureLogin(ctx, user, password); err != nil {
			return fmt.Errorf("postgres-user: %w", err)
		}
		return p.reconcileRoles(ctx, user, desiredRoles)
	}(); err != nil {
		return err
	}

	// Provision/refresh the credentials secret.
	if err := p.writeSecret(ctx, secretName, user, password, secretExists, existing); err != nil {
		return err
	}

	if logger != nil {
		logger.Info("postgres-user: provisioned credentials",
			"User", user,
			"Namespace", p.cfg.Namespace,
			"Secret", secretName,
			"Roles", desiredRoles,
		)
	}
	return nil
}

// desiredRoles returns the sorted, de-duplicated set of Postgres roles the
// user should hold, derived from their group memberships. The base "users"
// group role is always included.
func (p *postgresUserPlugin) desiredRoles(content map[string]interface{}) []string {
	groups := userGroupsFromContent(content)
	groups[defaultUsersGroup] = struct{}{}

	set := map[string]struct{}{}
	for g := range groups {
		if spec, ok := p.cfg.GroupRoles[g]; ok && spec.Role != "" {
			set[spec.Role] = struct{}{}
		}
	}
	return sortedKeys(set)
}

// managedRoles returns every role named in GroupRoles — the universe the
// reconcile is allowed to grant or revoke. Roles outside this set are never
// touched.
func (p *postgresUserPlugin) managedRoles() []string {
	set := map[string]struct{}{}
	for _, spec := range p.cfg.GroupRoles {
		if spec.Role != "" {
			set[spec.Role] = struct{}{}
		}
	}
	return sortedKeys(set)
}

func (p *postgresUserPlugin) reconcileRoles(ctx context.Context, user string, desired []string) error {
	managed := p.managedRoles()
	current, err := p.admin.GrantedManagedRoles(ctx, user, managed)
	if err != nil {
		return fmt.Errorf("postgres-user: %w", err)
	}

	desiredSet := toSet(desired)
	currentSet := toSet(current)

	for _, role := range desired {
		if _, ok := currentSet[role]; !ok {
			if err := p.admin.Grant(ctx, role, user); err != nil {
				return fmt.Errorf("postgres-user: %w", err)
			}
		}
	}
	for _, role := range current {
		if _, ok := desiredSet[role]; !ok {
			if err := p.admin.Revoke(ctx, role, user); err != nil {
				return fmt.Errorf("postgres-user: %w", err)
			}
		}
	}
	return nil
}

func (p *postgresUserPlugin) writeSecret(ctx context.Context, name, user, password string, exists bool, existing *corev1.Secret) error {
	data := p.secretData(user, password)

	if !exists {
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: p.cfg.Namespace,
				Labels:    p.secretLabels(user),
			},
			Type: corev1.SecretTypeOpaque,
			Data: data,
		}
		if err := p.secrets.Create(ctx, p.cfg.Namespace, s); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Benign race: another dispatch created it. Leave it be; the
				// next sync reconciles against the now-existing secret.
				if logger != nil {
					logger.Debug("postgres-user: secret already exists (benign race)",
						"User", user, "Name", name)
				}
				return nil
			}
			return fmt.Errorf("postgres-user: create secret %q: %w", name, err)
		}
		return nil
	}

	// Update only if data drifted (e.g. host/port/dbname changed in config).
	if secretDataEqual(existing.Data, data) {
		return nil
	}
	updated := existing.DeepCopy()
	updated.Data = data
	if updated.Labels == nil {
		updated.Labels = map[string]string{}
	}
	for k, v := range p.secretLabels(user) {
		updated.Labels[k] = v
	}
	if err := p.secrets.Update(ctx, p.cfg.Namespace, updated); err != nil {
		return fmt.Errorf("postgres-user: update secret %q: %w", name, err)
	}
	return nil
}

func (p *postgresUserPlugin) secretData(user, password string) map[string][]byte {
	port := fmt.Sprintf("%d", p.cfg.Port)
	uri := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		user, password, p.cfg.Host, port, p.cfg.Database, p.cfg.SSLMode)
	return map[string][]byte{
		"dbname":   []byte(p.cfg.Database),
		"host":     []byte(p.cfg.Host),
		"password": []byte(password),
		"port":     []byte(port),
		"uri":      []byte(uri),
		"user":     []byte(user),
	}
}

func (p *postgresUserPlugin) secretLabels(user string) map[string]string {
	labels := map[string]string{p.cfg.UserLabelKey: user}
	for k, v := range p.cfg.ExtraLabels {
		labels[k] = v
	}
	return labels
}

// objectClassContains reports whether content["objectClass"] includes want.
// The attribute may arrive as string, []string, or []interface{} after JSON
// decoding; all three shapes are handled.
func objectClassContains(content map[string]interface{}, want string) bool {
	raw, ok := content["objectClass"]
	if !ok {
		return false
	}
	switch v := raw.(type) {
	case string:
		return strings.EqualFold(v, want)
	case []string:
		for _, oc := range v {
			if strings.EqualFold(oc, want) {
				return true
			}
		}
	case []interface{}:
		for _, oc := range v {
			if s, ok := oc.(string); ok && strings.EqualFold(s, want) {
				return true
			}
		}
	}
	return false
}

// userNameFromContent extracts the login name. Prefers the uid attribute;
// falls back to the leading uid=<value> RDN of the DN.
func userNameFromContent(dn string, content map[string]interface{}) string {
	if s := firstStringValue(content["uid"]); s != "" {
		return s
	}
	first := strings.SplitN(dn, ",", 2)[0]
	if eq := strings.Index(first, "="); eq != -1 {
		return strings.TrimSpace(first[eq+1:])
	}
	return ""
}

// userGroupsFromContent returns the set of short group names from the merged
// "groups" attribute.
func userGroupsFromContent(content map[string]interface{}) map[string]struct{} {
	out := map[string]struct{}{}
	switch v := content["groups"].(type) {
	case string:
		if v != "" {
			out[v] = struct{}{}
		}
	case []string:
		for _, g := range v {
			if g != "" {
				out[g] = struct{}{}
			}
		}
	case []interface{}:
		for _, g := range v {
			if s, ok := g.(string); ok && s != "" {
				out[s] = struct{}{}
			}
		}
	}
	return out
}

func firstStringValue(raw interface{}) string {
	switch v := raw.(type) {
	case string:
		return v
	case []string:
		if len(v) > 0 {
			return v[0]
		}
	case []interface{}:
		if len(v) > 0 {
			if s, ok := v[0].(string); ok {
				return s
			}
		}
	}
	return ""
}

// pgInvalidRun matches runs of characters not allowed in our conservative
// identifier charset. Names are LDAP-derived, so we constrain them to
// lowercase alphanumerics and underscores before they ever reach SQL.
var pgInvalidRun = regexp.MustCompile(`[^a-z0-9_]+`)

func sanitizePGIdentifier(name string) string {
	s := strings.ToLower(name)
	s = pgInvalidRun.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if len(s) > 63 { // Postgres NAMEDATALEN limit.
		s = strings.TrimRight(s[:63], "_")
	}
	return s
}

// k8sInvalidRun matches runs of characters not allowed in a Kubernetes
// resource name. Names must be RFC-1123 DNS subdomains: lowercase
// alphanumerics, '-' and '.' only.
var k8sInvalidRun = regexp.MustCompile(`[^a-z0-9.-]+`)

// sanitizeK8sName coerces an arbitrary string into a valid RFC-1123 DNS
// subdomain usable as a resource name. Unlike sanitizePGIdentifier it
// preserves hyphens (and dots).
func sanitizeK8sName(name string) string {
	s := strings.ToLower(name)
	s = k8sInvalidRun.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-.")
	if len(s) > 253 { // DNS subdomain max length.
		s = strings.Trim(s[:253], "-.")
	}
	return s
}

func genPassword() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func toSet(items []string) map[string]struct{} {
	out := make(map[string]struct{}, len(items))
	for _, i := range items {
		out[i] = struct{}{}
	}
	return out
}

func secretDataEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if string(b[k]) != string(v) {
			return false
		}
	}
	return true
}
