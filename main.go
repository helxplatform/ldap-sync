package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "main/docs" // Replace with your actual module path.

	"github.com/go-ldap/ldap/v3"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
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

// Config holds the configuration for both source and target LDAP servers.
type Config struct {
	Source LDAPConfig `yaml:"source"`
	Target LDAPConfig `yaml:"target"`
	Hooks  []string   `yaml:"hooks"`
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
	Transformed  *TransformedEntry   `json:"transformed"`
	Derived      []DerivedSearchSpec `json:"derived"`
	Reset        bool                `json:"reset"`
	Dependencies []string            `json:"dependencies"`
}

var config Config
var logger *slog.Logger
var currentLogLevel string
var searches = make(map[string]*SearchSpec)
var searchResults = make(map[string]map[string]LDAPResult)
var dependencyTracker = newDependencyState()

type pendingEntry struct {
	entry *TransformedEntry
	deps  map[string]struct{}
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

func (d *dependencyState) handleEntry(entry *TransformedEntry, deps []string) {
	parentKey := normalizeDN(entry.DN)
	if parentKey == "" {
		logger.Error("Transformed entry has empty DN; skipping dependency processing")
		return
	}

	depSet := make(map[string]struct{})
	for _, dep := range deps {
		depKey := normalizeDN(dep)
		if depKey == "" || depKey == parentKey {
			continue
		}
		depSet[depKey] = struct{}{}
	}

	d.mu.Lock()
	if existing, ok := d.pending[parentKey]; ok {
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

	missing := make(map[string]struct{})
	for depKey := range depSet {
		if _, ok := d.synced[depKey]; !ok {
			missing[depKey] = struct{}{}
		}
	}

	if len(missing) == 0 {
		d.mu.Unlock()
		if err := storeDestinationLDAP(entry); err != nil {
			logger.Error("Error storing entry in destination LDAP", "DN", entry.DN, "Err", err)
			return
		}
		d.markSyncedAndRelease(parentKey)
		return
	}

	d.pending[parentKey] = &pendingEntry{
		entry: entry,
		deps:  missing,
	}
	for depKey := range missing {
		parents := d.reverse[depKey]
		if parents == nil {
			parents = make(map[string]struct{})
			d.reverse[depKey] = parents
		}
		parents[parentKey] = struct{}{}
	}
	d.mu.Unlock()

	logger.Info("Deferred entry until dependencies are synced", "DN", entry.DN, "MissingDependencies", len(missing))
}

func (d *dependencyState) markSyncedAndRelease(dn string) {
	dnKey := normalizeDN(dn)
	if dnKey == "" {
		return
	}

	var ready []*TransformedEntry

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
		if !ok {
			continue
		}
		delete(pending.deps, dnKey)
		if len(pending.deps) == 0 {
			ready = append(ready, pending.entry)
			delete(d.pending, parentKey)
		}
	}
	d.mu.Unlock()

	for _, entry := range ready {
		if err := storeDestinationLDAP(entry); err != nil {
			logger.Error("Error storing deferred entry in destination LDAP", "DN", entry.DN, "Err", err)
			continue
		}
		d.markSyncedAndRelease(entry.DN)
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
	searchRequest := ldap.NewSearchRequest(
		entry.DN,
		ldap.ScopeBaseObject,
		ldap.NeverDerefAliases,
		0,
		0,
		false,
		"(objectClass=*)",
		[]string{"dn"},
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
	for attr, value := range entry.Content {
		switch v := value.(type) {
		case []interface{}:
			var vals []string
			for _, x := range v {
				vals = append(vals, fmt.Sprintf("%v", x))
			}
			attributes[attr] = vals
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

	// Process the transformed element (if present).
	if hookResp.Transformed != nil {
		logger.Debug("Processing transformed hook response for DN", "DN", hookResp.Transformed.DN)
		dependencyTracker.handleEntry(hookResp.Transformed, hookResp.Dependencies)
	} else {
		logger.Info("No transformed data in hook response")
	}

	// Process each derived search provided.
	for _, ds := range hookResp.Derived {
		if spec, exists := searches[ds.ID]; exists {
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
			searches[ds.ID] = spec
			// Initialize the structured results store for this search id.
			searchResults[ds.ID] = make(map[string]LDAPResult)
			go ldapSearchAndSync(ds.ID, ds.Filter, ds.BaseDN, ds.Refresh, ds.Oneshot, stopChan)
			logger.Info("Derived search created", "SearchId", ds.ID)
		}
	}
	// Process the reset directive.
	if hookResp.Reset {
		// TODO: Reset is a legacy workaround; dependency handling should eventually make this obsolete.
		logger.Info("Reset directive received. Discarding internal search results")
		// Clear all internal search results.
		for id := range searchResults {
			searchResults[id] = make(map[string]LDAPResult)
		}
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
			resp, err := http.Post(hookURL, "application/json", bytes.NewBuffer(payload))
			if err != nil {
				logger.Error("Error posting to hook", "URL", hookURL, "Err", err)
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

	if existing, exists := searchResults[id][dn]; !exists {
		searchResults[id][dn] = newResult
		logger.Info("New item retrieved", "DN", dn, "SearchId", id)
		if !oneshot {
			sendHooks(newResult)
		}
	} else {
		if !reflect.DeepEqual(existing.Content, attrMap) {
			searchResults[id][dn] = newResult
			logger.Info("Updated item search", "DN", dn, "SearchId", id)
			if !oneshot {
				sendHooks(newResult)
			}
		} else {
			logger.Debug("No change", "DN", dn, "SearchId", id)
		}
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
	if _, exists := searches[id]; exists {
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
	searches[id] = spec
	// Initialize the structured results store for this search id.
	searchResults[id] = make(map[string]LDAPResult)
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
		spec, exists := searches[id]
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
	for k, spec := range searches {
		results = append(results, SearchInfo{
			ID:      k,
			Filter:  spec.Filter,
			Refresh: spec.Refresh,
			BaseDN:  spec.BaseDN,
			Oneshot: spec.Oneshot,
		})
	}
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
	spec, exists := searches[id]
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
	spec, exists := searches[id]
	if !exists {
		return c.String(http.StatusNotFound, "Search not found")
	}
	// Cancel the running search.
	close(spec.Stop)
	// Remove from the map.
	delete(searches, id)
	// Remove the results too
	delete(searchResults, id)
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
	results, exists := searchResults[id]
	if !exists {
		return c.String(http.StatusNotFound, "Search results not found for id: "+id)
	}

	full, _ := strconv.ParseBool(c.QueryParam("full"))
	if full {
		var entries []ResultEntryFull
		for _, res := range results {
			entries = append(entries, ResultEntryFull(res))
		}
		return c.JSON(http.StatusOK, entries)
	}

	var entries []ResultEntrySimple
	for _, res := range results {
		entries = append(entries, ResultEntrySimple{
			DN: res.DN,
		})
	}
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
