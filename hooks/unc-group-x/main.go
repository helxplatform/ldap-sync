// main.go
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

// Global variables for the hook service.
var (
	// pidUidMap maintains the mapping from pid to uid
	pidUidMap = make(map[string]string)

	// baseGid is obtained from a flag and used when processing UNC Users.
	baseGid string

	// baseGroup is obtained from a flag and used for the shared posixGroup.
	baseGroup string
)

// HookRequest represents the input payload for the /hook endpoint.
type HookRequest struct {
	DN      string                 `json:"dn"`
	Content map[string]interface{} `json:"content"`
}

// DerivedSearch represents a derived search definition.
type DerivedSearch struct {
	ID      string `json:"id"`
	Filter  string `json:"filter"`
	Refresh int    `json:"refresh"`
	BaseDN  string `json:"baseDN"`
	Onesho  bool   `json:"oneshot"`
}

// HookResponse defines the response structure returned by the /hook endpoint.
type HookResponse struct {
	Transformed  []map[string]interface{} `json:"transformed"`
	Derived      []DerivedSearch          `json:"derived"`
	Dependencies []string                 `json:"dependencies"`
	Bindings     map[string]*string       `json:"bindings"`
	Reset        bool                     `json:"reset"`
}

// @Summary Process LDAP hook payload
// @Description Process and transform LDAP entries based on their type.
// @Accept  json
// @Produce  json
// @Param   payload  body  HookRequest  true  "LDAP Hook Payload"
// @Success 200 {object} HookResponse
// @Router /hook [post]
func hookHandler(c echo.Context) error {
	var req HookRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest,
			map[string]string{"error": "invalid request payload"})
	}

	var response HookResponse

	// Process based on DN pattern.
	// Replace the sample routing and handlers here for custom object types.
	switch {
	// Example1: ORDRD Group
	case strings.HasPrefix(req.DN, "cn=unc:app:renci:"):
		response = processORDRDGroup(req)
	// Example2: UNC User
	case strings.HasPrefix(req.DN, "pid="):
		response = processUNCUser(req)
	// Example3: Posix Group (detect via DN part "ou=PosixGroups")
	case strings.Contains(req.DN, "ou=PosixGroups"):
		response = processPosixGroup(req)
	// Unknown type - no transformation applied.
	default:
		log.Printf("Unknown DN format: %s", req.DN)
		response = HookResponse{
			Transformed:  nil,
			Derived:      []DerivedSearch{},
			Dependencies: []string{},
			Bindings:     map[string]*string{},
			Reset:        false,
		}
	}

	// Log transformation summary for debugging.
	summary, _ := json.MarshalIndent(response, "", "  ")
	log.Printf("Processing summary:\n%s", summary)

	return c.JSON(http.StatusOK, response)
}

// processORDRDGroup handles transformation for ORDRD Groups.
// It applies the following logic:
//   - Extract groupname from DN.
//   - Replace the DN and content values accordingly.
//   - Iterate over each "member" entry, extract the pid, and build DN
//     templates that reference $pidUidMap.<pid>.
//   - Derived search filter is built using all member pids.
func processORDRDGroup(req HookRequest) HookResponse {
	// Extract the groupname from the DN.
	groupname := extractGroupName(req.DN)
	if groupname == "" {
		log.Printf("ORDRD Group: unable to extract groupname from DN %s", req.DN)
		return HookResponse{
			Transformed:  nil,
			Derived:      []DerivedSearch{},
			Dependencies: []string{},
			Bindings:     map[string]*string{},
			Reset:        false,
		}
	}
	newDN := fmt.Sprintf("cn=%s,ou=groups,dc=example,dc=org", groupname)

	// Retrieve the "member" array from the content.
	rawMembers, ok := req.Content["member"]
	if !ok {
		log.Println("ORDRD Group: no member field found")
		return HookResponse{
			Transformed:  nil,
			Derived:      []DerivedSearch{},
			Dependencies: []string{},
			Bindings:     map[string]*string{},
			Reset:        false,
		}
	}

	memberSlice, ok := rawMembers.([]interface{})
	if !ok {
		log.Println("ORDRD Group: invalid member field type")
		return HookResponse{
			Transformed:  nil,
			Derived:      []DerivedSearch{},
			Dependencies: []string{},
			Bindings:     map[string]*string{},
			Reset:        false,
		}
	}

	// Process members - build new member list and derive a filter.
	newMembers := []string{}
	filterParts := []string{}
	dependencies := []string{}

	for _, m := range memberSlice {
		memberStr, ok := m.(string)
		if !ok {
			continue
		}
		parts := strings.Split(memberStr, ",")
		if len(parts) < 1 || !strings.HasPrefix(parts[0], "pid=") {
			continue
		}
		pid := strings.TrimPrefix(parts[0], "pid=")
		filterParts = append(filterParts, fmt.Sprintf("(pid=%s)", pid))
		dnTemplate := fmt.Sprintf("uid=$pidUidMap.%s,ou=users,dc=example,dc=org", pid)
		newMembers = append(newMembers, dnTemplate)
		dependencies = append(dependencies, dnTemplate)
	}

	// Build the derived search specification.
	derived := []DerivedSearch{}
	if len(filterParts) > 0 {
		derivedID := fmt.Sprintf("%s-members", groupname)
		derived = []DerivedSearch{
			{
				ID:      derivedID,
				Filter:  "(|" + strings.Join(filterParts, "") + ")",
				Refresh: 10,
				BaseDN:  "ou=people,dc=unc,dc=edu",
				Onesho:  false,
			},
		}
	}

	// Build the new content.
	newContent := map[string]interface{}{
		"cn":          groupname,
		"member":      newMembers,
		"objectClass": []string{"top", "groupOfNames"},
	}

	transformed := map[string]interface{}{
		"dn":      newDN,
		"content": newContent,
	}

	return HookResponse{
		Transformed:  []map[string]interface{}{transformed},
		Derived:      derived,
		Dependencies: dependencies,
		Bindings:     map[string]*string{},
		Reset:        false,
	}
}

// processUNCUser handles transformation for UNC Users.
// It applies the following logic:
//   - Build a DN using the uid value.
//   - Use baseGid (obtained from flag) for all gidNumber values.
//   - Populate the transformed content and create a derived search based
//     on uidNumber.
//   - Update the global pidUidMap using the user's pid and uid.
func processUNCUser(req HookRequest) HookResponse {
	uid, ok := req.Content["uid"].(string)
	pid, _ := req.Content["pid"].(string)
	if !ok || uid == "" {
		if pid != "" {
			log.Printf("UNC User: uid not found or invalid; binding marked null for pid %s", pid)
			delete(pidUidMap, pid)
		} else {
			log.Println("UNC User: uid not found or invalid; pid missing")
		}
		bindings := map[string]*string{}
		if pid != "" {
			bindings[fmt.Sprintf("pidUidMap.%s", pid)] = nil
		}
		return HookResponse{
			Transformed:  nil,
			Derived:      []DerivedSearch{},
			Dependencies: []string{},
			Bindings:     bindings,
			Reset:        false,
		}
	}
	newDN := fmt.Sprintf("uid=%s,ou=users,dc=example,dc=org", uid)

	// Build the transformed content.
	newContent := map[string]interface{}{
		"cn":                 req.Content["cn"],
		"displayName":        req.Content["displayName"],
		"gidNumber":          baseGid, // Use the global baseGid.
		"givenName":          req.Content["givenName"],
		"homeDirectory":      fmt.Sprintf("/home/%s", uid),
		"objectClass":        []string{"top", "inetOrgPerson", "posixAccount", "helxUser"},
		"ou":                 "users",
		"sn":                 req.Content["sn"],
		"supplementalGroups": []interface{}{"0"},
		"uid":                uid,
		"uidNumber":          req.Content["uidNumber"],
	}

	transformed := map[string]interface{}{
		"dn":      newDN,
		"content": newContent,
	}

	transformedEntries := []map[string]interface{}{transformed}

	uidNumberStr, _ := req.Content["uidNumber"].(string)
	uidStr, _ := req.Content["uid"].(string)
	derived := []DerivedSearch{}
	if uidNumberStr != "" {
		derived = []DerivedSearch{
			{
				ID:      fmt.Sprintf("%s-posixGroups", uidNumberStr),
				Filter:  fmt.Sprintf("(&(objectClass=posixGroup)(memberUid=%s))", uidNumberStr),
				Refresh: 10,
				BaseDN:  "dc=unc,dc=edu",
				Onesho:  false,
			},
		}
	}

	if baseGroup != "" && uidStr != "" {
		baseGroupEntry := map[string]interface{}{
			"dn": fmt.Sprintf("cn=%s,ou=groups,dc=example,dc=org", baseGroup),
			"content": map[string]interface{}{
				"cn":          baseGroup,
				"gidNumber":   baseGid,
				"memberUid":   []interface{}{uidStr},
				"objectClass": []string{"top", "posixGroup"},
			},
		}
		transformedEntries = append(transformedEntries, baseGroupEntry)
	}

	// Update the pidUidMap based on the user's pid.
	bindings := map[string]*string{}
	if pid != "" {
		pidUidMap[pid] = uid
		bindings[fmt.Sprintf("pidUidMap.%s", pid)] = &uid
	}

	return HookResponse{
		Transformed:  transformedEntries,
		Derived:      derived,
		Dependencies: []string{},
		Bindings:     bindings,
		Reset:        false,
	}
}

// processPosixGroup handles transformation for Posix Groups.
// It applies the following logic:
//   - Transform the DN to the new location.
//   - In content, remove the "UNCGroup" type and update objectClass.
//   - If a "memberuid" field exists, promote it out of the content.
//   - No derived searches are generated.
func processPosixGroup(req HookRequest) HookResponse {
	cn := extractCN(req.DN)
	newDN := fmt.Sprintf("cn=%s,ou=groups,dc=example,dc=org", cn)

	// Copy the content for safe modification.
	newContent := copyMap(req.Content)

	// Remove memberuid from content, if it exists.
	memberUID, hasMemberUID := newContent["memberuid"]
	delete(newContent, "memberuid")

	// Modify objectClass: retain only "posixGroup".
	if rawOC, ok := newContent["objectClass"]; ok {
		if ocSlice, ok := rawOC.([]interface{}); ok {
			newOC := []string{}
			for _, v := range ocSlice {
				if s, ok := v.(string); ok && s == "posixGroup" {
					newOC = append(newOC, s)
				}
			}
			newContent["objectClass"] = newOC
		}
	}

	// Build the transformed object.
	transformed := map[string]interface{}{
		"dn":      newDN,
		"content": newContent,
	}
	// Promote memberuid to the top level if it exists.
	if hasMemberUID {
		transformed["memberuid"] = memberUID
	}

	return HookResponse{
		Transformed:  []map[string]interface{}{transformed},
		Derived:      []DerivedSearch{},
		Dependencies: []string{},
		Bindings:     map[string]*string{},
		Reset:        false,
	}
}

// extractGroupName extracts the groupname from the CN portion of a DN.
// If the CN contains colon-delimited segments, it returns the segment
// after the last ":" (e.g., "unc:app:renci:users" -> "users").
func extractGroupName(dn string) string {
	cn := extractCN(dn)
	if cn == "" {
		return ""
	}
	lastIdx := strings.LastIndex(cn, ":")
	if lastIdx >= 0 {
		if lastIdx+1 >= len(cn) {
			return ""
		}
		return cn[lastIdx+1:]
	}
	return cn
}

// extractCN extracts the common name (cn) from a DN.
func extractCN(dn string) string {
	if strings.HasPrefix(dn, "cn=") {
		withoutPrefix := dn[3:]
		parts := strings.Split(withoutPrefix, ",")
		if len(parts) > 0 {
			return parts[0]
		}
	}
	return ""
}

// copyMap creates a shallow copy of a map.
func copyMap(orig map[string]interface{}) map[string]interface{} {
	newMap := make(map[string]interface{})
	for k, v := range orig {
		newMap[k] = v
	}
	return newMap
}

func main() {
	// Accept the baseGid flag. Default value is "200" (adjust as needed).
	flag.StringVar(&baseGid, "baseGid", "200", "Base gidNumber to use for UNC Users")
	flag.StringVar(&baseGroup, "baseGroup", "users", "Base posixGroup CN for all UNC Users")
	flag.Parse()

	e := echo.New()

	// Register the /hook POST endpoint.
	e.POST("/hook", hookHandler)

	// The application listens on port 5001.
	port := "5001"
	log.Printf("Starting unc-group-x on port %s", port)
	if err := e.Start(":" + port); err != nil {
		log.Fatal(err)
	}
}
