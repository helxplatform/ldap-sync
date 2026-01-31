package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "main/docs" // Replace with your actual module path.

	"github.com/go-ldap/ldap/v3"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	_ "github.com/lib/pq"
	echoSwagger "github.com/swaggo/echo-swagger"
	"gopkg.in/yaml.v2"
)

// LDAPConfig holds connection details for one LDAP server.
type LDAPConfig struct {
	URL          string `yaml:"url"`
	BindDN       string `yaml:"bind_dn"`
	BindPassword string `yaml:"bind_password"`
	BaseDN       string `yaml:"base_dn"`
}

// DatabaseConfig holds database connection details.
type DatabaseConfig struct {
	Enabled      bool   `yaml:"enabled"`
	Host         string `yaml:"host"`
	Port         int    `yaml:"port"`
	Username     string `yaml:"username"`
	Database     string `yaml:"database"`
	PasswordFile string `yaml:"password_file"`
	SSLMode      string `yaml:"sslmode"`
}

// HookRetryConfig holds retry configuration for hook requests.
type HookRetryConfig struct {
	MaxRetries     int `yaml:"max_retries"`
	InitialDelayMs int `yaml:"initial_delay_ms"`
	MaxDelayMs     int `yaml:"max_delay_ms"`
}

// Config holds the configuration for both source and target LDAP servers.
type Config struct {
	Source    LDAPConfig      `yaml:"source"`
	Target    LDAPConfig      `yaml:"target"`
	Hooks     []string        `yaml:"hooks"`
	Database  DatabaseConfig  `yaml:"database"`
	HookRetry HookRetryConfig `yaml:"hook_retry"`
}

// SearchSpec represents a running search instance.
type SearchSpec struct {
	Filter  string
	Refresh int
	Stop    chan struct{}
	BaseDN  string // The base DN to use for this search.
	Oneshot bool   // one-shot -- don't involve the hook
}

// LogLevelRequest represents the payload for updating the log level.
type LogLevelRequest struct {
	Level string `json:"level"`
}

// SearchInfo represents the JSON structure for a search.
type SearchInfo struct {
	ID      string `json:"id"`
	Filter  string `json:"filter"`
	Refresh int    `json:"refresh"`
	BaseDN  string
	Oneshot bool
}

// DerivedSearchSpec describes a search as provided via a hook response.
type DerivedSearchSpec struct {
	ID      string `json:"id"`
	Filter  string `json:"filter"`
	Refresh int    `json:"refresh"`
	BaseDN  string `json:"baseDN"`
	Oneshot bool   `json:"oneshot"`
}

// LDAPResult holds an LDAP entry in a structured way.
type LDAPResult struct {
	DN      string                 `json:"dn"`
	Content map[string]interface{} `json:"content"`
}

// Define two result types.
type ResultEntrySimple struct {
	DN string `json:"dn"`
}

type ResultEntryFull struct {
	DN      string                 `json:"dn"`
	Content map[string]interface{} `json:"content"`
}

type TransformedEntry struct {
	DN      string                 `json:"dn"`
	Content map[string]interface{} `json:"content"`
}

// HookResponse represents the hook response JSON.
type HookResponse struct {
	Transformed  []TransformedEntry  `json:"transformed"`
	Derived      []DerivedSearchSpec `json:"derived"`
	Reset        bool                `json:"reset"`
	Dependencies []string            `json:"dependencies"`
	Bindings     map[string]*string  `json:"bindings"`
}

var config Config
var logger *slog.Logger
var currentLogLevel string
var searches = make(map[string]*SearchSpec)
var searchResults = make(map[string]map[string]LDAPResult)
var searchesMu sync.RWMutex
var searchResultsMu sync.RWMutex
var dependencyTracker = newDependencyState()
var mergeAttributes = map[string]struct{}{
	"memberuid": {},
}
var dnLocks sync.Map
var bindings = make(map[string]string)
var nullBindings = make(map[string]struct{})
var bindingsMu sync.RWMutex
var bindingPattern = regexp.MustCompile(`\$[A-Za-z0-9_.]+`)
var db *sql.DB

type pendingEntry struct {
	entry   *TransformedEntry
	deps    map[string]struct{}
	rawDeps []string
}

type dependencyState struct {
	mu      sync.Mutex
	synced  map[string]struct{}
	pending map[string]*pendingEntry
	reverse map[string]map[string]struct{}
}

func newDependencyState() *dependencyState {
	return &dependencyState{
		synced:  make(map[string]struct{}),
		pending: make(map[string]*pendingEntry),
		reverse: make(map[string]map[string]struct{}),
	}
}

func normalizeDN(dn string) string {
	return strings.ToLower(strings.TrimSpace(dn))
}

func sortedKeys(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func getDNLock(dn string) *sync.Mutex {
	key := normalizeDN(dn)
	if key == "" {
		key = dn
	}
	lock, _ := dnLocks.LoadOrStore(key, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// initDB initializes the database connection and creates the searches table if it doesn't exist.
func initDB(dbConfig DatabaseConfig) error {
	// Read password from file
	passwordBytes, err := os.ReadFile(dbConfig.PasswordFile)
	if err != nil {
		return fmt.Errorf("failed to read database password file: %w", err)
	}
	password := strings.TrimSpace(string(passwordBytes))

	// Set default SSL mode if not specified
	sslMode := dbConfig.SSLMode
	if sslMode == "" {
		sslMode = "disable"
	}

	// Set default port if not specified
	port := dbConfig.Port
	if port == 0 {
		port = 5432
	}

	// Build connection string
	dbURL := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		dbConfig.Username,
		password,
		dbConfig.Host,
		port,
		dbConfig.Database,
		sslMode,
	)

	db, err = sql.Open("postgres", dbURL)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	// Test the connection
	if err = db.Ping(); err != nil {
		return fmt.Errorf("failed to ping database: %w", err)
	}

	logger.Info("Database connection established successfully")
	return nil
}

// saveSearchToDB saves a search specification to the database.
func saveSearchToDB(id string, spec *SearchSpec) error {
	if db == nil {
		return fmt.Errorf("database not initialized")
	}

	insertSQL := `
	INSERT INTO searches (id, filter, refresh, base_dn, oneshot, created_at, updated_at)
	VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
	ON CONFLICT (id) DO UPDATE
	SET filter = $2, refresh = $3, base_dn = $4, oneshot = $5, updated_at = NOW();`

	_, err := db.Exec(insertSQL, id, spec.Filter, spec.Refresh, spec.BaseDN, spec.Oneshot)
	if err != nil {
		return fmt.Errorf("failed to save search to database: %w", err)
	}

	logger.Debug("Search saved to database", "SearchId", id)
	return nil
}

// loadSearchesFromDB loads all saved searches from the database.
func loadSearchesFromDB() (map[string]*SearchSpec, error) {
	if db == nil {
		return nil, fmt.Errorf("database not initialized")
	}

	selectSQL := `SELECT id, filter, refresh, base_dn, oneshot FROM searches;`
	rows, err := db.Query(selectSQL)
	if err != nil {
		return nil, fmt.Errorf("failed to query searches: %w", err)
	}
	defer rows.Close()

	loadedSearches := make(map[string]*SearchSpec)
	for rows.Next() {
		var id, filter, baseDN string
		var refresh int
		var oneshot bool

		if err := rows.Scan(&id, &filter, &refresh, &baseDN, &oneshot); err != nil {
			logger.Error("Error scanning search row", "Err", err)
			continue
		}

		stopChan := make(chan struct{})
		spec := &SearchSpec{
			Filter:  filter,
			Refresh: refresh,
			BaseDN:  baseDN,
			Oneshot: oneshot,
			Stop:    stopChan,
		}
		loadedSearches[id] = spec
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating search rows: %w", err)
	}

	logger.Info("Loaded searches from database", "Count", len(loadedSearches))
	return loadedSearches, nil
}

// deleteSearchFromDB deletes a search from the database.
func deleteSearchFromDB(id string) error {
	if db == nil {
		return fmt.Errorf("database not initialized")
	}

	deleteSQL := `DELETE FROM searches WHERE id = $1;`
	_, err := db.Exec(deleteSQL, id)
	if err != nil {
		return fmt.Errorf("failed to delete search from database: %w", err)
	}

	logger.Debug("Search deleted from database", "SearchId", id)
	return nil
}

func isMergeAttr(attr string) bool {
	_, ok := mergeAttributes[strings.ToLower(attr)]
	return ok
}

func isSliceValue(val interface{}) bool {
	switch val.(type) {
	case []interface{}, []string:
		return true
	default:
		return false
	}
}

func toStringSlice(val interface{}) []string {
	switch v := val.(type) {
	case []interface{}:
		vals := make([]string, 0, len(v))
		for _, x := range v {
			vals = append(vals, fmt.Sprintf("%v", x))
		}
		return vals
	case []string:
		return append([]string{}, v...)
	case nil:
		return nil
	default:
		return []string{fmt.Sprintf("%v", v)}
	}
}

func mergeValue(existing, incoming interface{}) interface{} {
	if isSliceValue(existing) || isSliceValue(incoming) {
		merged := mergeUnique(toStringSlice(existing), toStringSlice(incoming))
		out := make([]interface{}, len(merged))
		for i, v := range merged {
			out[i] = v
		}
		return out
	}
	return incoming
}

func mergeEntryContent(existing, incoming map[string]interface{}) map[string]interface{} {
	if existing == nil {
		return incoming
	}
	if incoming == nil {
		return existing
	}
	merged := make(map[string]interface{}, len(existing)+len(incoming))
	for k, v := range existing {
		merged[k] = v
	}
	for k, v := range incoming {
		if cur, ok := merged[k]; ok {
			merged[k] = mergeValue(cur, v)
			continue
		}
		merged[k] = v
	}
	return merged
}

func getEntryAttributeValues(entry *ldap.Entry, attr string) []string {
	attrLower := strings.ToLower(attr)
	for _, a := range entry.Attributes {
		if strings.ToLower(a.Name) == attrLower {
			return append([]string{}, a.Values...)
		}
	}
	return nil
}

func mergeUnique(existing, incoming []string) []string {
	if len(existing) == 0 {
		return append([]string{}, incoming...)
	}
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	merged := make([]string, 0, len(existing)+len(incoming))
	for _, v := range existing {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		merged = append(merged, v)
	}
	for _, v := range incoming {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		merged = append(merged, v)
	}
	return merged
}

func getBindingsSnapshot() (map[string]string, map[string]struct{}) {
	bindingsMu.RLock()
	defer bindingsMu.RUnlock()
	snapshot := make(map[string]string, len(bindings))
	for k, v := range bindings {
		snapshot[k] = v
	}
	nullSnapshot := make(map[string]struct{}, len(nullBindings))
	for k := range nullBindings {
		nullSnapshot[k] = struct{}{}
	}
	return snapshot, nullSnapshot
}

func updateBindings(newBindings map[string]*string) {
	if len(newBindings) == 0 {
		return
	}
	bindingsMu.Lock()
	prevCount := len(bindings)
	prevNullCount := len(nullBindings)
	nullCount := 0
	for k, v := range newBindings {
		if v == nil {
			nullBindings[k] = struct{}{}
			delete(bindings, k)
			nullCount++
			continue
		}
		bindings[k] = *v
		delete(nullBindings, k)
	}
	total := len(bindings)
	totalNull := len(nullBindings)
	bindingsMu.Unlock()
	logger.Debug(
		"Bindings updated",
		"NewCount", len(newBindings),
		"NullCount", nullCount,
		"TotalCount", total,
		"TotalNullCount", totalNull,
		"PrevCount", prevCount,
		"PrevNullCount", prevNullCount,
	)
	dependencyTracker.reprocessPending()
}

func resolveString(input string, bindings map[string]string, nullBindings map[string]struct{}) (string, bool, bool) {
	locs := bindingPattern.FindAllStringIndex(input, -1)
	if len(locs) == 0 {
		return input, false, false
	}
	var b strings.Builder
	b.Grow(len(input))
	missing := false
	hasNull := false
	last := 0
	for _, loc := range locs {
		b.WriteString(input[last:loc[0]])
		key := input[loc[0]+1 : loc[1]]
		if val, ok := bindings[key]; ok {
			b.WriteString(val)
		} else if _, ok := nullBindings[key]; ok {
			hasNull = true
		} else {
			missing = true
			b.WriteString(input[loc[0]:loc[1]])
		}
		last = loc[1]
	}
	b.WriteString(input[last:])
	return b.String(), missing, hasNull
}

func resolveValue(val interface{}, bindings map[string]string, nullBindings map[string]struct{}) (interface{}, bool) {
	switch v := val.(type) {
	case string:
		resolved, missing, _ := resolveString(v, bindings, nullBindings)
		return resolved, missing
	case []interface{}:
		out := make([]interface{}, 0, len(v))
		missing := false
		for _, item := range v {
			if s, ok := item.(string); ok {
				resolved, miss, hasNull := resolveString(s, bindings, nullBindings)
				missing = missing || miss
				if hasNull {
					continue
				}
				out = append(out, resolved)
				continue
			}
			out = append(out, item)
		}
		return out, missing
	case []string:
		out := make([]string, 0, len(v))
		missing := false
		for _, item := range v {
			resolved, miss, hasNull := resolveString(item, bindings, nullBindings)
			missing = missing || miss
			if hasNull {
				continue
			}
			out = append(out, resolved)
		}
		return out, missing
	default:
		return val, false
	}
}

func resolveEntryTemplates(entry *TransformedEntry, bindings map[string]string, nullBindings map[string]struct{}) (*TransformedEntry, bool) {
	resolvedDN, missingDN, dnNull := resolveString(entry.DN, bindings, nullBindings)
	resolvedContent := make(map[string]interface{}, len(entry.Content))
	missingContent := false
	for k, v := range entry.Content {
		resolvedVal, missingVal := resolveValue(v, bindings, nullBindings)
		missingContent = missingContent || missingVal
		resolvedContent[k] = resolvedVal
	}
	if dnNull {
		missingDN = true
	}
	return &TransformedEntry{
		DN:      resolvedDN,
		Content: resolvedContent,
	}, missingDN || missingContent
}

func resolveDependencies(deps []string, bindings map[string]string, nullBindings map[string]struct{}) ([]string, bool) {
	resolved := make([]string, 0, len(deps))
	missing := false
	for _, dep := range deps {
		resolvedDep, miss, hasNull := resolveString(dep, bindings, nullBindings)
		if hasNull {
			continue
		}
		missing = missing || miss
		resolved = append(resolved, resolvedDep)
	}
	return resolved, missing
}

func collectMissingBindingsFromString(input string, bindings map[string]string, nullBindings map[string]struct{}, missing map[string]struct{}) {
	locs := bindingPattern.FindAllStringIndex(input, -1)
	if len(locs) == 0 {
		return
	}
	for _, loc := range locs {
		key := input[loc[0]+1 : loc[1]]
		if _, ok := bindings[key]; ok {
			continue
		}
		if _, ok := nullBindings[key]; ok {
			continue
		}
		missing[key] = struct{}{}
	}
}

func collectMissingBindingsFromValue(val interface{}, bindings map[string]string, nullBindings map[string]struct{}, missing map[string]struct{}) {
	switch v := val.(type) {
	case string:
		collectMissingBindingsFromString(v, bindings, nullBindings, missing)
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				collectMissingBindingsFromString(s, bindings, nullBindings, missing)
			}
		}
	case []string:
		for _, item := range v {
			collectMissingBindingsFromString(item, bindings, nullBindings, missing)
		}
	}
}

func collectMissingBindings(entry *TransformedEntry, deps []string, bindings map[string]string, nullBindings map[string]struct{}) []string {
	missing := make(map[string]struct{})
	if entry != nil {
		collectMissingBindingsFromString(entry.DN, bindings, nullBindings, missing)
		for _, v := range entry.Content {
			collectMissingBindingsFromValue(v, bindings, nullBindings, missing)
		}
	}
	for _, dep := range deps {
		collectMissingBindingsFromString(dep, bindings, nullBindings, missing)
	}
	if len(missing) == 0 {
		return nil
	}
	keys := make([]string, 0, len(missing))
	for key := range missing {
		keys = append(keys, key)
	}
	return keys
}

func (d *dependencyState) handleEntry(entry *TransformedEntry, deps []string) {
	parentKey := normalizeDN(entry.DN)
	if parentKey == "" {
		logger.Error("Transformed entry has empty DN; skipping dependency processing")
		return
	}

	rawDeps := append([]string{}, deps...)
	d.mu.Lock()
	if existing, ok := d.pending[parentKey]; ok {
		if existing.entry != nil {
			entry.Content = mergeEntryContent(existing.entry.Content, entry.Content)
		}
		if len(existing.rawDeps) > 0 {
			rawDeps = append(rawDeps, existing.rawDeps...)
		}
		for depKey := range existing.deps {
			parents := d.reverse[depKey]
			if parents != nil {
				delete(parents, parentKey)
				if len(parents) == 0 {
					delete(d.reverse, depKey)
				}
			}
		}
		delete(d.pending, parentKey)
	}
	d.mu.Unlock()

	bindingsSnapshot, nullSnapshot := getBindingsSnapshot()
	resolvedEntry, entryMissing := resolveEntryTemplates(entry, bindingsSnapshot, nullSnapshot)
	resolvedDeps, depsMissing := resolveDependencies(rawDeps, bindingsSnapshot, nullSnapshot)
	logger.Debug(
		"Resolved dependencies",
		"DN", entry.DN,
		"RawDeps", len(rawDeps),
		"ResolvedDeps", len(resolvedDeps),
		"EntryMissing", entryMissing,
		"DepsMissing", depsMissing,
		"BindingsCount", len(bindingsSnapshot),
		"NullBindingsCount", len(nullSnapshot),
	)

	depSet := make(map[string]struct{})
	for _, dep := range resolvedDeps {
		depKey := normalizeDN(dep)
		if depKey == "" || depKey == parentKey {
			continue
		}
		depSet[depKey] = struct{}{}
	}

	d.mu.Lock()

	missing := make(map[string]struct{})
	for depKey := range depSet {
		if _, ok := d.synced[depKey]; !ok {
			missing[depKey] = struct{}{}
		}
	}
	missingList := sortedKeys(missing)
	resolvedList := sortedKeys(depSet)
	logger.Debug(
		"Dependency state for entry",
		"DN", entry.DN,
		"ResolvedDependencies", resolvedList,
		"MissingDependencies", missingList,
		"MissingCount", len(missingList),
	)

	if len(missing) == 0 && !entryMissing && !depsMissing {
		d.mu.Unlock()
		if err := storeDestinationLDAP(resolvedEntry); err != nil {
			logger.Error("Error storing entry in destination LDAP", "DN", resolvedEntry.DN, "Err", err)
			return
		}
		d.markSyncedAndRelease(resolvedEntry.DN)
		return
	}

	d.pending[parentKey] = &pendingEntry{
		entry:   entry,
		deps:    missing,
		rawDeps: rawDeps,
	}
	for depKey := range missing {
		parents := d.reverse[depKey]
		if parents == nil {
			parents = make(map[string]struct{})
			d.reverse[depKey] = parents
		}
		parents[parentKey] = struct{}{}
		logger.Debug("Adding dependency", "DN", entry.DN, "Dependency", depKey)
	}
	logger.Debug(
		"Pending entry stored",
		"DN", entry.DN,
		"MissingDependencies", missingList,
		"MissingCount", len(missingList),
	)
	d.mu.Unlock()

	if entryMissing || depsMissing {
		missingKeys := collectMissingBindings(entry, rawDeps, bindingsSnapshot, nullSnapshot)
		logger.Info(
			"Deferred entry until bindings are resolved",
			"DN", entry.DN,
			"MissingDependencies", len(missing),
			"MissingBindings", missingKeys,
			"BindingsCount", len(bindingsSnapshot),
			"NullBindingsCount", len(nullSnapshot),
		)
	} else {
		logger.Info(
			"Deferred entry until dependencies are synced",
			"DN", entry.DN,
			"MissingDependencies", len(missing),
			"BindingsCount", len(bindingsSnapshot),
			"NullBindingsCount", len(nullSnapshot),
		)
	}
}

func (d *dependencyState) reprocessPending() {
	var pendingEntries []*pendingEntry

	d.mu.Lock()
	pendingCount := len(d.pending)
	for parentKey, pending := range d.pending {
		if pending != nil {
			pendingEntries = append(pendingEntries, pending)
			for depKey := range pending.deps {
				parents := d.reverse[depKey]
				if parents != nil {
					delete(parents, parentKey)
					if len(parents) == 0 {
						delete(d.reverse, depKey)
					}
				}
			}
		}
		delete(d.pending, parentKey)
	}
	d.mu.Unlock()

	if pendingCount > 0 {
		logger.Debug("Reprocessing pending entries", "Count", pendingCount)
	}
	for _, pending := range pendingEntries {
		if pending.entry == nil {
			continue
		}
		logger.Debug("Reprocessing pending entry", "DN", pending.entry.DN, "RawDeps", len(pending.rawDeps))
		d.handleEntry(pending.entry, pending.rawDeps)
	}
}

func (d *dependencyState) markSyncedAndRelease(dn string) {
	dnKey := normalizeDN(dn)
	if dnKey == "" {
		return
	}

	var ready []*pendingEntry
	type depReleaseLog struct {
		parentDN       string
		resolvedDepDN  string
		remainingDeps  []string
		remainingCount int
	}
	var releaseLogs []depReleaseLog
	var parentDNs []string

	d.mu.Lock()
	if _, exists := d.synced[dnKey]; exists {
		d.mu.Unlock()
		return
	}
	d.synced[dnKey] = struct{}{}

	parents := d.reverse[dnKey]
	delete(d.reverse, dnKey)
	for parentKey := range parents {
		pending, ok := d.pending[parentKey]
		parentDN := parentKey
		if ok && pending != nil && pending.entry != nil && pending.entry.DN != "" {
			parentDN = pending.entry.DN
		}
		if parentDN != "" {
			parentDNs = append(parentDNs, parentDN)
		}
		if !ok {
			continue
		}
		delete(pending.deps, dnKey)
		remaining := sortedKeys(pending.deps)
		releaseLogs = append(releaseLogs, depReleaseLog{
			parentDN:       parentDN,
			resolvedDepDN:  dn,
			remainingDeps:  remaining,
			remainingCount: len(remaining),
		})
		if len(pending.deps) == 0 {
			ready = append(ready, pending)
			delete(d.pending, parentKey)
		}
	}
	d.mu.Unlock()

	if len(parentDNs) > 0 {
		sort.Strings(parentDNs)
		logger.Debug(
			"Dependency synced",
			"DN", dn,
			"Parents", len(parentDNs),
			"ParentDNs", parentDNs,
			"ReadyToRelease", len(ready),
		)
	}
	for _, logEntry := range releaseLogs {
		logger.Debug(
			"Dependency resolved for parent",
			"ParentDN", logEntry.parentDN,
			"ResolvedDependency", logEntry.resolvedDepDN,
			"RemainingDependencies", logEntry.remainingCount,
			"RemainingList", logEntry.remainingDeps,
		)
	}
	if len(ready) > 0 {
		bindingsSnapshot, nullSnapshot := getBindingsSnapshot()
		for _, pending := range ready {
			if pending == nil || pending.entry == nil {
				continue
			}
			resolvedEntry, missing := resolveEntryTemplates(pending.entry, bindingsSnapshot, nullSnapshot)
			if missing {
				logger.Info(
					"Deferred entry still missing bindings on release",
					"DN", pending.entry.DN,
				)
				d.handleEntry(pending.entry, pending.rawDeps)
				continue
			}
			if err := storeDestinationLDAP(resolvedEntry); err != nil {
				logger.Error("Error storing deferred entry in destination LDAP", "DN", resolvedEntry.DN, "Err", err)
				continue
			}
			logger.Info("Storing deferred entry in destination LDAP", "DN", resolvedEntry.DN)
			d.markSyncedAndRelease(resolvedEntry.DN)
		}
	}
}

// initLogger initializes the logger using log/slog.
// It checks the --loglevel flag first, then the LOG_LEVEL env variable,
// and defaults to "info" if neither is set.
func initLogger(loglevel string) {
	lvlStr := os.Getenv("LOG_LEVEL")
	if loglevel != "" {
		lvlStr = loglevel
	}
	if lvlStr == "" {
		lvlStr = "info"
	}
	var lvl slog.Level = slog.LevelInfo
	switch strings.ToLower(lvlStr) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	case "info":
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level:     lvl,
		AddSource: true,
	})
	logger = slog.New(h)
	logger.Info("Logger initialized", "level", lvlStr)
}

// setLogLevel updates the global logger to the new level.
func setLogLevel(newLevel string) {
	var lvl slog.Level = slog.LevelInfo
	switch strings.ToLower(newLevel) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	case "info":
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level:     lvl,
		AddSource: true,
	})
	logger = slog.New(h)
	logger.Info("Log level updated", "newLevel", newLevel)
}

// loadConfig reads the YAML config file
func loadConfig(path string) error {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, &config)
}

// connectAndBindLDAP connects to the LDAP server using the source configuration and binds using the credentials.
// Returns an established connection or an error.
func connectAndBindLDAP() (*ldap.Conn, error) {
	l, err := ldap.DialURL(config.Source.URL)
	if err != nil {
		return nil, err
	}
	if err = l.Bind(config.Source.BindDN, config.Source.BindPassword); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// performLDAPSearch performs an LDAP search using the provided connection, baseDN, and filter.
func performLDAPSearch(l *ldap.Conn, baseDN, filter string) (*ldap.SearchResult, error) {
	searchRequest := ldap.NewSearchRequest(
		baseDN,
		ldap.ScopeWholeSubtree,
		ldap.NeverDerefAliases,
		0,
		0,
		false,
		filter,
		[]string{"*"},
		nil,
	)
	return l.Search(searchRequest)
}

func storeDestinationLDAP(entry *TransformedEntry) error {
	lock := getDNLock(entry.DN)
	lock.Lock()
	defer lock.Unlock()

	// Connect to destination LDAP.
	l, err := ldap.DialURL(config.Target.URL)
	if err != nil {
		return err
	}
	defer l.Close()

	// Bind with destination credentials.
	if err = l.Bind(config.Target.BindDN, config.Target.BindPassword); err != nil {
		return err
	}

	// Check if the entry exists.
	searchAttrs := []string{"dn"}
	if len(mergeAttributes) > 0 {
		for attr := range mergeAttributes {
			searchAttrs = append(searchAttrs, attr)
		}
	}
	searchRequest := ldap.NewSearchRequest(
		entry.DN,
		ldap.ScopeBaseObject,
		ldap.NeverDerefAliases,
		0,
		0,
		false,
		"(objectClass=*)",
		searchAttrs,
		nil,
	)
	sr, err := l.Search(searchRequest)
	if err != nil {
		// Check if the error is LDAP error code 32 ("No Such Object")
		if ldapErr, ok := err.(*ldap.Error); ok && ldapErr.ResultCode == ldap.LDAPResultNoSuchObject {
			// Treat it as if no entry was found.
			sr = &ldap.SearchResult{Entries: []*ldap.Entry{}}
		} else {
			return err
		}
	}

	// Prepare attributes conversion: each attribute becomes a slice of strings.
	attributes := make(map[string][]string)
	aggregateAttrs := make(map[string]struct{})
	for attr, value := range entry.Content {
		switch v := value.(type) {
		case []interface{}:
			aggregateAttrs[attr] = struct{}{}
			var vals []string
			for _, x := range v {
				vals = append(vals, fmt.Sprintf("%v", x))
			}
			attributes[attr] = vals
		case []string:
			aggregateAttrs[attr] = struct{}{}
			attributes[attr] = append([]string{}, v...)
		default:
			attributes[attr] = []string{fmt.Sprintf("%v", v)}
		}
	}

	// If the entry doesn't exist, add it.
	if len(sr.Entries) == 0 {
		addReq := ldap.NewAddRequest(entry.DN, nil)
		for attr, values := range attributes {
			addReq.Attribute(attr, values)
		}
		// Optionally, ensure an objectClass is set.
		if _, exists := attributes["objectClass"]; !exists {
			addReq.Attribute("objectClass", []string{"top", "inetOrgPerson"})
		}
		if err = l.Add(addReq); err != nil {
			return err
		}
		logger.Info("Added entry to destination LDAP", "DN", entry.DN)
	} else {
		entryData := sr.Entries[0]
		for attr, values := range attributes {
			if !isMergeAttr(attr) {
				if _, ok := aggregateAttrs[attr]; !ok {
					continue
				}
			}
			if len(values) == 0 {
				continue
			}
			existing := getEntryAttributeValues(entryData, attr)
			if len(existing) == 0 {
				continue
			}
			attributes[attr] = mergeUnique(existing, values)
		}
		// If the entry exists, update it.
		modReq := ldap.NewModifyRequest(entry.DN, nil)
		for attr, values := range attributes {
			modReq.Replace(attr, values)
		}
		if err = l.Modify(modReq); err != nil {
			return err
		}
		logger.Info("Modified entry in destination LDAP", "DN", entry.DN)
	}
	return nil
}

// ldapSearchAndSync performs the LDAP search on the source server and synchronizes the results.
func ldapSearchAndSync(id, filter, baseDN string, refresh int, oneshot bool, stopChan chan struct{}) {
	for {
		select {
		case <-stopChan:
			logger.Info("Search cancelled", "SearchId", id)
			return
		default:
		}

		logger.Debug("Performing LDAP search with filter", "Filter", filter, "SearchId", id, "BaseDN", baseDN)
		l, err := connectAndBindLDAP()
		if err != nil {
			logger.Error("Error connecting and binding to LDAP", "Err", err)
			select {
			case <-stopChan:
				return
			case <-time.After(time.Duration(refresh) * time.Second):
			}
			continue
		}

		sr, err := performLDAPSearch(l, baseDN, filter)
		if err != nil {
			logger.Error("Error performing search", "Err", err)
			l.Close()
			select {
			case <-stopChan:
				return
			case <-time.After(time.Duration(refresh) * time.Second):
			}
			continue
		}
		l.Close()

		for _, entry := range sr.Entries {
			processLDAPEntry(id, entry, oneshot)
		}

		// If one-shot mode is active, exit after one iteration.
		if oneshot {
			logger.Info("One-shot search completed", "SearchId", id)
			return
		}

		select {
		case <-stopChan:
			logger.Debug("Search cancelled", "SearchId", id)
			return
		case <-time.After(time.Duration(refresh) * time.Second):
		}
	}
}

// processHookResponse is a stub for processing the hook response.
func processHookResponse(hookResp HookResponse) {
	// Log the parsed hook response values.
	logger.Debug("Processing Hook response", "Transformed", hookResp.Transformed, "Derived", hookResp.Derived, "Reset", hookResp.Reset)

	if len(hookResp.Bindings) > 0 {
		logger.Debug("Hook bindings received", "Count", len(hookResp.Bindings))
		updateBindings(hookResp.Bindings)
	}

	// Process the transformed element (if present).
	if len(hookResp.Transformed) > 0 {
		for i := range hookResp.Transformed {
			transformed := hookResp.Transformed[i]
			logger.Debug("Processing transformed hook response for DN", "DN", transformed.DN)
			dependencyTracker.handleEntry(&transformed, hookResp.Dependencies)
		}
	} else {
		logger.Info("No transformed data in hook response")
	}

	// Process each derived search provided.
	for _, ds := range hookResp.Derived {
		searchesMu.RLock()
		spec, exists := searches[ds.ID]
		searchesMu.RUnlock()
		if exists {
			// Update existing search.
			close(spec.Stop)
			stopChan := make(chan struct{})
			spec.Filter = ds.Filter
			spec.Refresh = ds.Refresh
			spec.BaseDN = ds.BaseDN
			spec.Oneshot = ds.Oneshot
			spec.Stop = stopChan
			go ldapSearchAndSync(ds.ID, ds.Filter, ds.BaseDN, ds.Refresh, ds.Oneshot, stopChan)
			logger.Info("Derived search updated", "SearchId", ds.ID)
		} else {
			// Create a new search.
			stopChan := make(chan struct{})
			spec := &SearchSpec{
				Filter:  ds.Filter,
				Refresh: ds.Refresh,
				BaseDN:  ds.BaseDN,
				Oneshot: ds.Oneshot,
				Stop:    stopChan,
			}
			searchesMu.Lock()
			searches[ds.ID] = spec
			searchesMu.Unlock()
			// Initialize the structured results store for this search id.
			searchResultsMu.Lock()
			searchResults[ds.ID] = make(map[string]LDAPResult)
			searchResultsMu.Unlock()
			go ldapSearchAndSync(ds.ID, ds.Filter, ds.BaseDN, ds.Refresh, ds.Oneshot, stopChan)
			logger.Info("Derived search created", "SearchId", ds.ID)
		}
	}
	// Process the reset directive.
	if hookResp.Reset {
		// TODO: Reset is a legacy workaround; dependency handling should eventually make this obsolete.
		logger.Info("Reset directive received. Discarding internal search results")
		// Clear all internal search results.
		searchResultsMu.Lock()
		for id := range searchResults {
			searchResults[id] = make(map[string]LDAPResult)
		}
		searchResultsMu.Unlock()
	}
}

func decodeHookResponses(body []byte) ([]HookResponse, error) {
	var responses []HookResponse
	if err := json.Unmarshal(body, &responses); err == nil {
		return responses, nil
	}

	var single HookResponse
	if err := json.Unmarshal(body, &single); err == nil {
		return []HookResponse{single}, nil
	}

	return nil, fmt.Errorf("invalid hook response: expected object or array")
}

// postToHookWithRetry posts to a hook URL with exponential backoff retry logic.
func postToHookWithRetry(hookURL string, payload []byte) (*http.Response, error) {
	const backoffFactor = 2.0

	// Get retry configuration with defaults
	maxRetries := config.HookRetry.MaxRetries
	if maxRetries == 0 {
		maxRetries = 10
	}
	initialDelayMs := config.HookRetry.InitialDelayMs
	if initialDelayMs == 0 {
		initialDelayMs = 100
	}
	maxDelayMs := config.HookRetry.MaxDelayMs
	if maxDelayMs == 0 {
		maxDelayMs = 30000
	}

	initialDelay := time.Duration(initialDelayMs) * time.Millisecond
	maxDelay := time.Duration(maxDelayMs) * time.Millisecond

	var lastErr error
	delay := initialDelay

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Add jitter to prevent thundering herd (±10%)
			jitter := time.Duration(float64(delay) * 0.1)
			sleepTime := delay + time.Duration(float64(jitter)*(2.0*float64(time.Now().UnixNano()%1000)/1000.0-1.0))
			logger.Debug("Retrying hook request", "URL", hookURL, "Attempt", attempt+1, "Delay", sleepTime)
			time.Sleep(sleepTime)

			// Exponential backoff with cap
			delay = time.Duration(float64(delay) * backoffFactor)
			if delay > maxDelay {
				delay = maxDelay
			}
		}

		resp, err := http.Post(hookURL, "application/json", bytes.NewBuffer(payload))
		if err == nil {
			return resp, nil
		}

		lastErr = err
		if attempt < maxRetries {
			logger.Warn("Hook request failed, will retry", "URL", hookURL, "Attempt", attempt+1, "Err", err)
		}
	}

	return nil, fmt.Errorf("failed after %d attempts: %w", maxRetries+1, lastErr)
}

// sendHooks posts the LDAP result to each URL specified in config.Hooks.
func sendHooks(result LDAPResult) {
	payload, err := json.Marshal(result)
	if err != nil {
		logger.Error("Error marshalling hook payload for DN", "DN", result.DN, "Err", err)
		return
	}
	for _, url := range config.Hooks {
		// Launch each hook call concurrently.
		go func(hookURL string) {
			resp, err := postToHookWithRetry(hookURL, payload)
			if err != nil {
				logger.Error("Error posting to hook after retries", "URL", hookURL, "Err", err)
				return
			}
			defer resp.Body.Close()
			body, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				logger.Error("Error reading hook response", "URL", hookURL, "Err", err)
				return
			}

			hookResps, err := decodeHookResponses(body)
			if err != nil {
				logger.Error("Hook response decode failed", "URL", hookURL, "Err", err)
				return
			}

			for _, hookResp := range hookResps {
				processHookResponse(hookResp)
			}
		}(url)
	}
}

// processLDAPEntry processes a single LDAP entry, updating the searchResults
// for the given search id. It builds a structured attribute map, and logs whether
// the entry is new, updated, or unchanged.
func processLDAPEntry(id string, entry *ldap.Entry, oneshot bool) {
	dn := entry.DN
	attrMap := make(map[string]interface{})
	for _, attr := range entry.Attributes {
		if len(attr.Values) == 1 {
			attrMap[attr.Name] = attr.Values[0]
		} else {
			attrMap[attr.Name] = attr.Values
		}
	}

	newResult := LDAPResult{
		DN:      dn,
		Content: attrMap,
	}

	var shouldSend bool
	var logMsg string

	searchResultsMu.Lock()
	results, ok := searchResults[id]
	if !ok {
		searchResultsMu.Unlock()
		logger.Warn("Search results missing for id", "SearchId", id, "DN", dn)
		return
	}

	if existing, exists := results[dn]; !exists {
		results[dn] = newResult
		logMsg = "New item retrieved"
		shouldSend = !oneshot
	} else {
		if !reflect.DeepEqual(existing.Content, attrMap) {
			results[dn] = newResult
			logMsg = "Updated item search"
			shouldSend = !oneshot
		} else {
			logMsg = "No change"
		}
	}
	searchResultsMu.Unlock()

	switch logMsg {
	case "New item retrieved", "Updated item search":
		logger.Info(logMsg, "DN", dn, "SearchId", id)
	default:
		logger.Debug(logMsg, "DN", dn, "SearchId", id)
	}

	if shouldSend {
		sendHooks(newResult)
	}
}

// createSearchHandler godoc
// @Summary Create new search
// @Description Creates a new search with a unique id. Returns an error if the id already exists.
// @Tags search
// @Accept application/x-www-form-urlencoded
// @Produce json
// @Param id formData string true "Unique search id"
// @Param filter formData string true "LDAP search filter"
// @Param refresh formData int true "Refresh interval in seconds"
// @Param baseDN formData string false "Optional base DN for the search; defaults to global config if omitted"
// @Param oneShot formData bool false "If set to true, the search will run in one-shot mode (hook subsystem will not be engaged). Defaults to true."
// @Success 200 {string} string "Search created"
// @Failure 400 {string} string "Invalid parameters or search already exists"
// @Router /search [post]
func createSearchHandler(c echo.Context) error {
	id := c.FormValue("id")
	filter := strings.TrimSpace(c.FormValue("filter"))
	refreshStr := c.FormValue("refresh")
	baseDN := c.FormValue("baseDN")
	if baseDN == "" {
		baseDN = config.Source.BaseDN
	}
	if id == "" || filter == "" || refreshStr == "" {
		return c.String(http.StatusBadRequest, "Missing required parameters (id, filter, refresh)")
	}
	searchesMu.RLock()
	_, exists := searches[id]
	searchesMu.RUnlock()
	if exists {
		return c.String(http.StatusBadRequest, "Search with this id already exists")
	}
	refresh, err := strconv.Atoi(refreshStr)
	if err != nil {
		return c.String(http.StatusBadRequest, "Invalid refresh parameter")
	}

	// Parse oneShot parameter; default to true if not provided.
	oneShotStr := c.FormValue("oneShot")
	oneshot := true
	if oneShotStr != "" {
		parsed, err := strconv.ParseBool(oneShotStr)
		if err != nil {
			return c.String(http.StatusBadRequest, "Invalid oneShot parameter")
		}
		oneshot = parsed
	}

	stopChan := make(chan struct{})
	spec := &SearchSpec{
		Filter:  filter,
		Refresh: refresh,
		Stop:    stopChan,
		BaseDN:  baseDN,
		Oneshot: oneshot,
	}
	searchesMu.Lock()
	searches[id] = spec
	searchesMu.Unlock()
	// Initialize the structured results store for this search id.
	searchResultsMu.Lock()
	searchResults[id] = make(map[string]LDAPResult)
	searchResultsMu.Unlock()

	// Save to database
	if err := saveSearchToDB(id, spec); err != nil {
		logger.Error("Failed to save search to database", "SearchId", id, "Err", err)
		// Continue anyway - the search will still work, just won't persist
	}

	// Pass the oneshot flag to the search routine.
	go ldapSearchAndSync(id, filter, baseDN, refresh, oneshot, stopChan)
	return c.String(http.StatusOK, "Search created")
}

// getSearchHandler godoc
// @Summary Get search(s)
// @Description Retrieves a specific search by id if provided, or all searches if no id is specified.
// @Tags search
// @Accept json
// @Produce json
// @Param id query string false "Search ID"
// @Success 200 {object} SearchInfo "When id is provided" or {array} SearchInfo "When id is not provided"
// @Failure 404 {string} string "Search not found"
// @Router /search [get]
func getSearchHandler(c echo.Context) error {
	id := c.QueryParam("id")
	if id != "" {
		searchesMu.RLock()
		spec, exists := searches[id]
		searchesMu.RUnlock()
		if !exists {
			return c.String(http.StatusNotFound, "Search with given id not found")
		}
		result := SearchInfo{
			ID:      id,
			Filter:  spec.Filter,
			Refresh: spec.Refresh,
			BaseDN:  spec.BaseDN,
			Oneshot: spec.Oneshot,
		}
		return c.JSON(http.StatusOK, result)
	}

	// No id provided; return all searches.
	var results []SearchInfo
	searchesMu.RLock()
	for k, spec := range searches {
		results = append(results, SearchInfo{
			ID:      k,
			Filter:  spec.Filter,
			Refresh: spec.Refresh,
			BaseDN:  spec.BaseDN,
			Oneshot: spec.Oneshot,
		})
	}
	searchesMu.RUnlock()
	return c.JSON(http.StatusOK, results)
}

// updateSearchHandler godoc
// @Summary Update existing search
// @Description Updates an existing search (complete replacement) with new filter, refresh, and optionally baseDN. If baseDN is omitted, the global config's BaseDN is used.
// @Tags search
// @Accept application/x-www-form-urlencoded
// @Produce json
// @Param id path string true "Unique search id"
// @Param filter formData string true "LDAP search filter"
// @Param refresh formData int true "Refresh interval in seconds"
// @Param baseDN formData string false "Optional base DN for the search; defaults to global config if omitted"
// @Param oneShot formData bool false "If set to true, the search will run in one-shot mode (hook subsystem will not be engaged). Defaults to true."
// @Success 200 {string} string "Search updated"
// @Failure 400 {string} string "Invalid parameters or search does not exist"
// @Router /search/{id} [put]
func updateSearchHandler(c echo.Context) error {
	id := c.Param("id")
	filter := strings.TrimSpace(c.FormValue("filter"))
	refreshStr := c.FormValue("refresh")
	baseDN := c.FormValue("baseDN")
	if baseDN == "" {
		baseDN = config.Source.BaseDN
	}
	if id == "" || filter == "" || refreshStr == "" {
		return c.String(http.StatusBadRequest, "Missing required parameters (id, filter, refresh)")
	}
	searchesMu.RLock()
	spec, exists := searches[id]
	searchesMu.RUnlock()
	if !exists {
		return c.String(http.StatusBadRequest, "Search with this id does not exist")
	}
	refresh, err := strconv.Atoi(refreshStr)
	if err != nil {
		return c.String(http.StatusBadRequest, "Invalid refresh parameter")
	}

	// Parse oneShot parameter; default to true if not provided.
	oneShotStr := c.FormValue("oneShot")
	oneshot := true
	if oneShotStr != "" {
		parsed, err := strconv.ParseBool(oneShotStr)
		if err != nil {
			return c.String(http.StatusBadRequest, "Invalid oneShot parameter")
		}
		oneshot = parsed
	}

	// Cancel the current search.
	close(spec.Stop)
	stopChan := make(chan struct{})
	// Update the search spec.
	spec.Filter = filter
	spec.Refresh = refresh
	spec.BaseDN = baseDN
	spec.Oneshot = oneshot
	spec.Stop = stopChan

	// Update in database
	if err := saveSearchToDB(id, spec); err != nil {
		logger.Error("Failed to update search in database", "SearchId", id, "Err", err)
		// Continue anyway
	}

	// Restart the search goroutine with the new oneshot flag.
	go ldapSearchAndSync(id, filter, baseDN, refresh, oneshot, stopChan)
	return c.String(http.StatusOK, "Search updated")
}

// deleteSearchHandler godoc
// @Summary Delete search
// @Description Deletes an existing search by its unique id.
// @Tags search
// @Produce json
// @Param id path string true "Unique search id"
// @Success 200 {string} string "Search deleted"
// @Failure 404 {string} string "Search not found"
// @Router /search/{id} [delete]
func deleteSearchHandler(c echo.Context) error {
	id := c.Param("id")
	searchesMu.RLock()
	spec, exists := searches[id]
	searchesMu.RUnlock()
	if !exists {
		return c.String(http.StatusNotFound, "Search not found")
	}
	// Cancel the running search.
	close(spec.Stop)
	// Remove from the map.
	searchesMu.Lock()
	delete(searches, id)
	searchesMu.Unlock()
	// Remove the results too
	searchResultsMu.Lock()
	delete(searchResults, id)
	searchResultsMu.Unlock()

	// Delete from database
	if err := deleteSearchFromDB(id); err != nil {
		logger.Error("Failed to delete search from database", "SearchId", id, "Err", err)
		// Continue anyway - the search is already stopped and removed from memory
	}

	return c.String(http.StatusOK, "Search deleted")
}

// getResultsHandler godoc
// @Summary Get search results
// @Description Retrieves all LDAP objects for a given search id.
//
//	If the optional query parameter "full" is true, returns both DN and content; otherwise, only DN is returned.
//
// @Tags results
// @Produce json
// @Param id path string true "Unique search id"
// @Param full query boolean false "Return full result (DN and content) if true, else only DN"
// @Success 200 {array} ResultEntrySimple "When full is false"
// @Success 200 {array} ResultEntryFull "When full is true"
// @Failure 404 {string} string "Search results not found"
// @Router /results/{id} [get]
func getResultsHandler(c echo.Context) error {
	id := c.Param("id")
	searchResultsMu.RLock()
	results, exists := searchResults[id]
	if !exists {
		searchResultsMu.RUnlock()
		return c.String(http.StatusNotFound, "Search results not found for id: "+id)
	}

	full, _ := strconv.ParseBool(c.QueryParam("full"))
	if full {
		var entries []ResultEntryFull
		for _, res := range results {
			entries = append(entries, ResultEntryFull(res))
		}
		searchResultsMu.RUnlock()
		return c.JSON(http.StatusOK, entries)
	}

	var entries []ResultEntrySimple
	for _, res := range results {
		entries = append(entries, ResultEntrySimple{
			DN: res.DN,
		})
	}
	searchResultsMu.RUnlock()
	return c.JSON(http.StatusOK, entries)
}

// getLogLevelHandler is a REST endpoint that reports the current log level.
// @Summary Get current log level
// @Description Returns the current log level.
// @Tags log
// @Produce json
// @Success 200 {object} map[string]string "current log level"
// @Router /loglevel [get]
func getLogLevelHandler(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]string{
		"level": currentLogLevel,
	})
}

// logLevelHandler is a REST endpoint to update log level at runtime.
// @Summary Update log level
// @Description Update the logging level at runtime.
// @Tags log
// @Accept json
// @Produce json
// @Param level body LogLevelRequest true "New log level"
// @Success 200 {object} map[string]string "Updated log level"
// @Failure 400 {object} map[string]string "Invalid payload or log level"
// @Router /loglevel [put]
func logLevelHandler(c echo.Context) error {
	type reqBody struct {
		Level string `json:"level"`
	}
	var req reqBody
	if err := c.Bind(&req); err != nil {
		logger.Error("Failed to bind log level request", "Err", err)
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "Invalid payload",
		})
	}
	newLevel := strings.ToLower(req.Level)
	switch newLevel {
	case "debug", "info", "warn", "error":
		setLogLevel(newLevel)
	default:
		logger.Error("Invalid log level provided", "Level", req.Level)
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "Invalid log level",
		})
	}
	return c.JSON(http.StatusOK, map[string]string{
		"message": "Log level updated",
		"level":   req.Level,
	})
}

// healthzHandler handles the liveness probe.
// @Summary Liveness Probe
// @Description Returns OK if the application is running.
// @Tags probes
// @Produce json
// @Success 200 {object} map[string]string "status: ok"
// @Router /healthz [get]
func healthzHandler(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// readyzHandler handles the readiness probe.
// @Summary Readiness Probe
// @Description Returns OK if the application is ready to serve traffic.
// @Tags probes
// @Produce json
// @Success 200 {object} map[string]string "status: ready"
// @Router /readyz [get]
func readyzHandler(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]string{"status": "ready"})
}

// @title ldap-sync API
// @version 1.0
// @description API for synchronizing LDAP entries between two servers.
// @host localhost:5500
// @BasePath /
func main() {
	var loglevel string

	flag.StringVar(&loglevel, "loglevel", "", "Set the log level (debug, info, warn, error)")
	flag.Parse()
	initLogger(loglevel)

	// Load configuration from /etc/ldap-sync/config.yaml.
	if err := loadConfig("/etc/ldap-sync/config.yaml"); err != nil {
		logger.Error("Error loading config", "Err", err)
		os.Exit(1)
	}

	// Initialize database if enabled in config
	if config.Database.Enabled {
		if err := initDB(config.Database); err != nil {
			logger.Error("Error initializing database", "Err", err)
			os.Exit(1)
		}
		defer db.Close()

		// Load saved searches from database
		loadedSearches, err := loadSearchesFromDB()
		if err != nil {
			logger.Error("Error loading searches from database", "Err", err)
			// Don't exit - continue with empty searches
		} else {
			// Restore searches and start their goroutines
			searchesMu.Lock()
			for id, spec := range loadedSearches {
				searches[id] = spec
				// Initialize results store for this search
				searchResultsMu.Lock()
				searchResults[id] = make(map[string]LDAPResult)
				searchResultsMu.Unlock()
				// Start the search goroutine
				go ldapSearchAndSync(id, spec.Filter, spec.BaseDN, spec.Refresh, spec.Oneshot, spec.Stop)
				logger.Info("Restored search from database", "SearchId", id)
			}
			searchesMu.Unlock()
		}
	} else {
		logger.Info("Database persistence disabled, searches will not be persisted")
	}

	// Initialize Echo.
	e := echo.New()
	e.Use(middleware.Recover())
	e.Use(middleware.LoggerWithConfig(middleware.LoggerConfig{
		Skipper: func(c echo.Context) bool {
			path := c.Request().URL.Path
			return path == "/healthz" || path == "/readyz"
		},
	}))

	// Register endpoints.
	e.POST("/search", createSearchHandler)
	e.GET("/search", getSearchHandler)
	e.PUT("/search/:id", updateSearchHandler)
	e.DELETE("/search/:id", deleteSearchHandler)
	e.GET("/results/:id", getResultsHandler)
	e.PUT("/loglevel", logLevelHandler)
	e.GET("/loglevel", getLogLevelHandler)
	e.GET("/healthz", healthzHandler)
	e.GET("/readyz", readyzHandler)

	// Redirect /swagger to /swagger/index.html
	e.GET("/swagger", func(c echo.Context) error {
		return c.Redirect(http.StatusMovedPermanently, "/swagger/index.html")
	})

	// Register the Swagger documentation endpoint.
	e.GET("/swagger/*", echoSwagger.WrapHandler)

	e.GET("/", func(c echo.Context) error {
		return c.Redirect(http.StatusFound, "/swagger/index.html")
	})

	logger.Info("Server started on :5500")
	e.Logger.Fatal(e.Start(":5500"))
}
