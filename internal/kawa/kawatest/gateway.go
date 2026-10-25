// Package kawatest is an in-memory kawa gateway admin API for tests: the topic, RBAC role and group,
// and auth client endpoints the operator uses and the client credentials grant, with the gateway's
// semantics, including that a DELETE of a name that is not a virtual topic deletes the physical
// topic, and that a resource the operator wrote is managed by it: the gateway refuses changes to it
// from anyone else until the operator releases it.
package kawatest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"

	"github.com/kawa-platform/kawa-operator/internal/kawa"
)

// Gateway is a fake admin API. Its zero value is not usable; call [NewGateway].
type Gateway struct {
	*httptest.Server

	mu       sync.Mutex
	virtual  map[string]kawa.VirtualTopic
	physical map[string]bool
	// clients maps service client ids to secrets; empty means no admin authentication.
	clients map[string]string
	roles   map[string]kawa.Role
	groups  map[string]kawa.Group
	// authClients are the SASL clients, by username.
	authClients map[string]AuthClient
	// tokens maps issued access tokens to the principal they authenticate.
	tokens map[string]string
	// Reject, when set, answers PUT, PATCH and DELETE of the topic (or role, group or auth client,
	// keyed by [roleKey], [groupKey] or [authClientKey]) with this
	// status and message, like a governance rule (403) or a validation error (400).
	reject map[string]rejection
	// writes records the principal of every PUT, PATCH and DELETE call, per topic name (or [roleKey],
	// [groupKey] or [authClientKey]).
	writes map[string][]string
	// managed maps the key of a resource managed by a principal to that principal.
	managed map[string]string
}

// OperatorPrincipal is the principal whose writes the gateway marks as managing the resource.
const OperatorPrincipal = "kawa-operator"

type rejection struct {
	status  int
	message string
}

// NewGateway starts a fake admin API; clients (id, secret, id, secret...) enable authentication.
func NewGateway(clients ...string) *Gateway {
	g := &Gateway{
		virtual:     map[string]kawa.VirtualTopic{},
		physical:    map[string]bool{},
		clients:     map[string]string{},
		roles:       map[string]kawa.Role{},
		groups:      map[string]kawa.Group{},
		authClients: map[string]AuthClient{},
		tokens:      map[string]string{},
		reject:      map[string]rejection{},
		writes:      map[string][]string{},
		managed:     map[string]string{},
	}
	for i := 0; i+1 < len(clients); i += 2 {
		g.clients[clients[i]] = clients[i+1]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", g.token)
	mux.HandleFunc("GET /topics", g.authenticated(g.list))
	mux.HandleFunc("PUT /topics/{name}", g.authenticated(g.put))
	mux.HandleFunc("DELETE /topics/{name}", g.authenticated(g.delete))
	mux.HandleFunc("DELETE /topics/{name}/managed-by", g.authenticated(g.release(topicKey, func(name string) bool {
		_, ok := g.virtual[name]
		return ok
	})))
	mux.HandleFunc("GET /rbac/roles", g.authenticated(g.listRoles))
	mux.HandleFunc("PUT /rbac/roles/{name}", g.authenticated(g.putRole))
	mux.HandleFunc("DELETE /rbac/roles/{name}", g.authenticated(g.deleteRole))
	mux.HandleFunc("DELETE /rbac/roles/{name}/managed-by", g.authenticated(g.release(roleKey, func(name string) bool {
		_, ok := g.roles[name]
		return ok
	})))
	mux.HandleFunc("GET /rbac/groups", g.authenticated(g.listGroups))
	mux.HandleFunc("PUT /rbac/groups/{name}", g.authenticated(g.putGroup))
	mux.HandleFunc("DELETE /rbac/groups/{name}", g.authenticated(g.deleteGroup))
	mux.HandleFunc("DELETE /rbac/groups/{name}/managed-by", g.authenticated(g.release(groupKey, func(name string) bool {
		_, ok := g.groups[name]
		return ok
	})))
	mux.HandleFunc("GET /auth/clients", g.authenticated(g.listAuthClients))
	mux.HandleFunc("PUT /auth/clients/{name}", g.authenticated(g.putAuthClient))
	mux.HandleFunc("PATCH /auth/clients/{name}", g.authenticated(g.patchAuthClient))
	mux.HandleFunc("DELETE /auth/clients/{name}", g.authenticated(g.deleteAuthClient))
	mux.HandleFunc("DELETE /auth/clients/{name}/managed-by", g.authenticated(g.release(authClientKey, func(name string) bool {
		_, ok := g.authClients[name]
		return ok
	})))
	g.Server = httptest.NewServer(mux)
	return g
}

// SetVirtual stores a virtual topic as if it was made outside the operator, unmanaged.
func (g *Gateway) SetVirtual(name string, topic kawa.VirtualTopic) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.virtual[name] = topic
	delete(g.managed, topicKey(name))
}

// RemoveVirtual deletes a virtual topic as if it was removed outside the operator.
func (g *Gateway) RemoveVirtual(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.virtual, name)
	delete(g.managed, topicKey(name))
}

// AddPhysical adds a physical topic.
func (g *Gateway) AddPhysical(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.physical[name] = true
}

// Virtual is the stored virtual topic name.
func (g *Gateway) Virtual(name string) (kawa.VirtualTopic, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	topic, ok := g.virtual[name]
	return topic, ok
}

// HasPhysical is whether the physical topic name exists.
func (g *Gateway) HasPhysical(name string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.physical[name]
}

// ManagedBy is the principal managing the virtual topic name, or "" when it is unmanaged.
func (g *Gateway) ManagedBy(name string) string {
	return g.managedBy(topicKey(name))
}

// RoleManagedBy is the principal managing the role name, or "" when it is unmanaged.
func (g *Gateway) RoleManagedBy(name string) string {
	return g.managedBy(roleKey(name))
}

// GroupManagedBy is the principal managing the group name, or "" when it is unmanaged.
func (g *Gateway) GroupManagedBy(name string) string {
	return g.managedBy(groupKey(name))
}

// AuthClientManagedBy is the principal managing the client name, or "" when it is unmanaged.
func (g *Gateway) AuthClientManagedBy(name string) string {
	return g.managedBy(authClientKey(name))
}

func (g *Gateway) managedBy(key string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.managed[key]
}

// Writes is how many PUT and DELETE calls named the topic.
func (g *Gateway) Writes(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.writes[name])
}

// WritesBy are the principals of the PUT and DELETE calls that named the topic, in order.
func (g *Gateway) WritesBy(name string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.writes[name]...)
}

// ForgetTokens makes the gateway forget every token it issued, as a gateway restarted does.
func (g *Gateway) ForgetTokens() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tokens = map[string]string{}
}

// RemoveClient makes the gateway forget every token and stop knowing the client clientID, as a gateway
// restarted without that client does.
func (g *Gateway) RemoveClient(clientID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tokens = map[string]string{}
	delete(g.clients, clientID)
}

// Reject makes PUT and DELETE of name fail with status and message; status 0 lifts it.
func (g *Gateway) Reject(name string, status int, message string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if status == 0 {
		delete(g.reject, name)
		return
	}
	g.reject[name] = rejection{status, message}
}

func (g *Gateway) token(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !ok || g.clients[id] == "" || g.clients[id] != secret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	if r.PostForm.Get("grant_type") != "client_credentials" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	token := fmt.Sprintf("token-%s-%d", id, len(g.tokens))
	g.tokens[token] = id
	writeJSON(w, http.StatusOK, map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": 900})
}

// principalHeader carries the authenticated principal from authenticated to the handlers.
const principalHeader = "X-Kawatest-Principal"

func (g *Gateway) authenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		authRequired := len(g.clients) > 0
		principal := g.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		g.mu.Unlock()
		if !authRequired {
			principal = "admin"
		}
		if principal == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		r.Header.Set(principalHeader, principal)
		next(w, r)
	}
}

func (g *Gateway) list(w http.ResponseWriter, _ *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	listing := make([]map[string]any, 0, len(g.virtual)+len(g.physical))
	for name, topic := range g.virtual {
		listing = append(listing, map[string]any{
			"type": "virtual", "name": name, "partitions": 0, "replicationFactor": 0,
			"filter": topic.Filter.View(), "physicalTopic": topic.Topic,
			"exposePhysicalTopic": topic.ExposePhysicalTopic, "valueFormat": topic.ValueFormat,
			"managedBy": g.marker(topicKey(name)),
		})
	}
	for name := range g.physical {
		listing = append(listing, map[string]any{
			"type": "physical", "name": name, "partitions": 1, "replicationFactor": 1,
			"filter": nil, "physicalTopic": nil, "exposePhysicalTopic": nil, "valueFormat": nil, "managedBy": nil,
		})
	}
	writeJSON(w, http.StatusOK, listing)
}

func (g *Gateway) put(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Type                string              `json:"type"`
		Topic               string              `json:"topic"`
		Filter              *kawa.Filter        `json:"filter"`
		ExposePhysicalTopic bool                `json:"exposePhysicalTopic"`
		ValueFormat         *kawa.PayloadFormat `json:"valueFormat"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid topic body: " + err.Error()})
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.recordWrite(w, r, topicKey(name)) {
		return
	}
	if body.Type != "virtual" || body.Topic == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "virtual topic requires a physical 'topic'"})
		return
	}
	g.virtual[name] = kawa.VirtualTopic{
		Topic: body.Topic, Filter: body.Filter, ExposePhysicalTopic: body.ExposePhysicalTopic, ValueFormat: body.ValueFormat,
	}
	g.stamp(r, topicKey(name))
	writeJSON(w, http.StatusOK, body)
}

func (g *Gateway) delete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.recordWrite(w, r, topicKey(name)) {
		return
	}
	_, isVirtual := g.virtual[name]
	switch {
	case isVirtual:
		delete(g.virtual, name)
		delete(g.managed, topicKey(name))
	case g.physical[name]:
		delete(g.physical, name)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "topic '" + name + "' not found"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// topicKey keys a topic in reject, writes and managed.
func topicKey(name string) string {
	return name
}

// describe is the kind and name of the resource key names, for error messages.
func describe(key string) (kind, name string) {
	if kind, name, ok := strings.Cut(key, ":"); ok {
		return kind, name
	}
	return "topic", key
}

// roleKey keys a role in reject and writes, apart from the topic of the same name.
func roleKey(name string) string {
	return "role:" + name
}

// SetRole stores a role as if it was made outside the operator, unmanaged.
func (g *Gateway) SetRole(name string, role kawa.Role) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.roles[name] = role
	delete(g.managed, roleKey(name))
}

// RemoveRole deletes a role as if it was removed outside the operator.
func (g *Gateway) RemoveRole(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.roles, name)
	delete(g.managed, roleKey(name))
}

// Role is the stored role name.
func (g *Gateway) Role(name string) (kawa.Role, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	role, ok := g.roles[name]
	return role, ok
}

// RoleWrites is how many PUT and DELETE calls named the role.
func (g *Gateway) RoleWrites(name string) int {
	return g.Writes(roleKey(name))
}

// RoleWritesBy are the principals of the PUT and DELETE calls that named the role, in order.
func (g *Gateway) RoleWritesBy(name string) []string {
	return g.WritesBy(roleKey(name))
}

// RejectRole makes PUT and DELETE of the role name fail with status and message; status 0 lifts it.
func (g *Gateway) RejectRole(name string, status int, message string) {
	g.Reject(roleKey(name), status, message)
}

func (g *Gateway) listRoles(w http.ResponseWriter, _ *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	listing := make([]map[string]any, 0, len(g.roles))
	for name, role := range g.roles {
		listing = append(listing, map[string]any{
			"name": name, "acls": append([]kawa.ACL{}, role.ACLs...), "managedBy": g.marker(roleKey(name)),
		})
	}
	writeJSON(w, http.StatusOK, listing)
}

// putRole stores the role with the gateway's defaults: patternType LITERAL, permission ALLOW, and
// no pattern for CLUSTER.
func (g *Gateway) putRole(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var role kawa.Role
	if err := json.NewDecoder(r.Body).Decode(&role); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid role body: " + err.Error()})
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	key := roleKey(name)
	if !g.recordWrite(w, r, key) {
		return
	}
	for i := range role.ACLs {
		acl := &role.ACLs[i]
		if acl.Resource.PatternType == "" {
			acl.Resource.PatternType = "LITERAL"
		}
		if acl.Permission == "" {
			acl.Permission = "ALLOW"
		}
		if acl.Resource.Type == "CLUSTER" {
			acl.Resource.Pattern = nil
		} else if acl.Resource.Pattern == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "invalid role body: pattern must not be null or blank for resource type " + acl.Resource.Type,
			})
			return
		}
	}
	g.roles[name] = role
	g.stamp(r, key)
	writeJSON(w, http.StatusOK, role)
}

func (g *Gateway) deleteRole(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	g.mu.Lock()
	defer g.mu.Unlock()
	key := roleKey(name)
	if !g.recordWrite(w, r, key) {
		return
	}
	if _, ok := g.roles[name]; !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "role '" + name + "' not found"})
		return
	}
	// Deleting the role strips it from every group: refused when that changes a group managed by
	// someone else.
	var listing []string
	for groupName, group := range g.groups {
		if slices.Contains(group.Roles, name) {
			listing = append(listing, groupKey(groupName))
		}
	}
	if !g.allowChange(w, r, listing...) {
		return
	}
	delete(g.roles, name)
	delete(g.managed, key)
	for groupName, group := range g.groups {
		group.Roles = slices.DeleteFunc(slices.Clone(group.Roles), func(role string) bool { return role == name })
		g.groups[groupName] = group
	}
	w.WriteHeader(http.StatusNoContent)
}

// groupKey keys a group in reject and writes, apart from the topic or role of the same name.
func groupKey(name string) string {
	return "group:" + name
}

// SetGroup stores a group as if it was made outside the operator, unmanaged.
func (g *Gateway) SetGroup(name string, group kawa.Group) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.groups[name] = group
	delete(g.managed, groupKey(name))
}

// RemoveGroup deletes a group as if it was removed outside the operator.
func (g *Gateway) RemoveGroup(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.groups, name)
	delete(g.managed, groupKey(name))
}

// Group is the stored group name.
func (g *Gateway) Group(name string) (kawa.Group, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	group, ok := g.groups[name]
	return group, ok
}

// GroupWrites is how many PUT and DELETE calls named the group.
func (g *Gateway) GroupWrites(name string) int {
	return g.Writes(groupKey(name))
}

// GroupWritesBy are the principals of the PUT and DELETE calls that named the group, in order.
func (g *Gateway) GroupWritesBy(name string) []string {
	return g.WritesBy(groupKey(name))
}

// RejectGroup makes PUT and DELETE of the group name fail with status and message; status 0 lifts it.
func (g *Gateway) RejectGroup(name string, status int, message string) {
	g.Reject(groupKey(name), status, message)
}

// listGroups lists every group with its clients and roles, as empty lists when it has none.
func (g *Gateway) listGroups(w http.ResponseWriter, _ *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	listing := make([]map[string]any, 0, len(g.groups))
	for name, group := range g.groups {
		listing = append(listing, map[string]any{
			"name":      name,
			"clients":   append([]string{}, group.Clients...),
			"roles":     append([]string{}, group.Roles...),
			"managedBy": g.marker(groupKey(name)),
		})
	}
	writeJSON(w, http.StatusOK, listing)
}

func (g *Gateway) putGroup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var group kawa.Group
	if err := json.NewDecoder(r.Body).Decode(&group); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid group body: " + err.Error()})
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	key := groupKey(name)
	if !g.recordWrite(w, r, key) {
		return
	}
	g.groups[name] = group
	g.stamp(r, key)
	writeJSON(w, http.StatusOK, group)
}

// deleteGroup refuses, like the gateway, to delete a group that still has clients.
func (g *Gateway) deleteGroup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	g.mu.Lock()
	defer g.mu.Unlock()
	key := groupKey(name)
	if !g.recordWrite(w, r, key) {
		return
	}
	group, ok := g.groups[name]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "group '" + name + "' not found"})
		return
	}
	if len(group.Clients) > 0 {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": fmt.Sprintf("group '%s' still has clients %v", name, group.Clients),
		})
		return
	}
	delete(g.groups, name)
	delete(g.managed, key)
	w.WriteHeader(http.StatusNoContent)
}

// AuthClient is a stored SASL client. Unlike the gateway, which hashes it, the fake keeps the
// password as written, so tests can check it.
type AuthClient struct {
	Mechanism string
	Password  string
}

// authClientKey keys an auth client in reject and writes, apart from a topic, role or group of the
// same name.
func authClientKey(name string) string {
	return "client:" + name
}

// SetAuthClient stores a client as if it was made outside the operator, unmanaged.
func (g *Gateway) SetAuthClient(name string, client AuthClient) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.authClients[name] = client
	delete(g.managed, authClientKey(name))
}

// RemoveAuthClient deletes a client as if it was removed outside the operator, and takes it out of
// every group, as the gateway does.
func (g *Gateway) RemoveAuthClient(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.removeAuthClient(name)
}

// AuthClient is the stored client name.
func (g *Gateway) AuthClient(name string) (AuthClient, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	client, ok := g.authClients[name]
	return client, ok
}

// AuthClientWrites is how many PUT, PATCH and DELETE calls named the client.
func (g *Gateway) AuthClientWrites(name string) int {
	return g.Writes(authClientKey(name))
}

// AuthClientWritesBy are the principals of the PUT, PATCH and DELETE calls that named the client, in
// order.
func (g *Gateway) AuthClientWritesBy(name string) []string {
	return g.WritesBy(authClientKey(name))
}

// RejectAuthClient makes PUT, PATCH and DELETE of the client name fail with status and message;
// status 0 lifts it.
func (g *Gateway) RejectAuthClient(name string, status int, message string) {
	g.Reject(authClientKey(name), status, message)
}

// listAuthClients lists every client with its username and mechanism, never its password.
func (g *Gateway) listAuthClients(w http.ResponseWriter, _ *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	listing := make([]map[string]any, 0, len(g.authClients))
	for name, client := range g.authClients {
		listing = append(listing, map[string]any{
			"username": name, "mechanism": client.Mechanism, "managedBy": g.marker(authClientKey(name)),
		})
	}
	writeJSON(w, http.StatusOK, listing)
}

// authClientBody is the body of PUT and PATCH /auth/clients/{name}.
type authClientBody struct {
	Mechanism *string   `json:"mechanism"`
	Password  *string   `json:"password"`
	Groups    *[]string `json:"groups"`
}

// putAuthClient stores the client, which needs a mechanism and a password, and puts it in exactly
// the given groups, which must exist: none when groups is omitted.
func (g *Gateway) putAuthClient(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body authClientBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid client body: " + err.Error()})
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.recordWrite(w, r, authClientKey(name)) {
		return
	}
	if body.Mechanism == nil || body.Password == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mechanism and password are required"})
		return
	}
	groups := []string{}
	if body.Groups != nil {
		groups = *body.Groups
	}
	if !g.setMemberships(w, r, name, groups) {
		return
	}
	g.authClients[name] = AuthClient{Mechanism: *body.Mechanism, Password: *body.Password}
	g.stamp(r, authClientKey(name))
	writeJSON(w, http.StatusOK, map[string]string{"username": name, "mechanism": *body.Mechanism})
}

// patchAuthClient updates the fields given of an existing client; omitted groups keep its groups.
func (g *Gateway) patchAuthClient(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body authClientBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid client body: " + err.Error()})
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.recordWrite(w, r, authClientKey(name)) {
		return
	}
	client, ok := g.authClients[name]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "client '" + name + "' not found"})
		return
	}
	if body.Groups != nil && !g.setMemberships(w, r, name, *body.Groups) {
		return
	}
	if body.Mechanism != nil {
		client.Mechanism = *body.Mechanism
	}
	if body.Password != nil {
		client.Password = *body.Password
	}
	g.authClients[name] = client
	g.stamp(r, authClientKey(name))
	writeJSON(w, http.StatusOK, map[string]string{"username": name, "mechanism": client.Mechanism})
}

// deleteAuthClient removes the client and takes it out of every group.
func (g *Gateway) deleteAuthClient(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.recordWrite(w, r, authClientKey(name)) {
		return
	}
	if _, ok := g.authClients[name]; !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "client '" + name + "' not found"})
		return
	}
	// Deleting the client takes it out of every group: refused when that changes a group managed by
	// someone else.
	var listing []string
	for groupName, group := range g.groups {
		if slices.Contains(group.Clients, name) {
			listing = append(listing, groupKey(groupName))
		}
	}
	if !g.allowChange(w, r, listing...) {
		return
	}
	g.removeAuthClient(name)
	w.WriteHeader(http.StatusNoContent)
}

// recordWrite records the principal of a write to key and answers it with the rejection set for
// key, if any, or 409 when key is managed by another principal. It reports whether the write may go
// on. g.mu must be held.
func (g *Gateway) recordWrite(w http.ResponseWriter, r *http.Request, key string) bool {
	g.writes[key] = append(g.writes[key], r.Header.Get(principalHeader))
	if rej, ok := g.reject[key]; ok {
		writeJSON(w, rej.status, map[string]string{"error": rej.message})
		return false
	}
	return g.allowChange(w, r, key)
}

// allowChange answers 409, like the gateway, when one of keys is managed by a principal other than
// the caller's. It reports whether the change may go on. g.mu must be held.
func (g *Gateway) allowChange(w http.ResponseWriter, r *http.Request, keys ...string) bool {
	for _, key := range keys {
		if owner := g.managed[key]; owner != "" && owner != r.Header.Get(principalHeader) {
			kind, name := describe(key)
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": fmt.Sprintf("%s '%s' is managed by %s; change it through Kubernetes", kind, name, owner),
			})
			return false
		}
	}
	return true
}

// stamp marks key as managed by the operator when the operator wrote it; another principal's
// write leaves the marker as it is. g.mu must be held.
func (g *Gateway) stamp(r *http.Request, key string) {
	if r.Header.Get(principalHeader) == OperatorPrincipal {
		g.managed[key] = OperatorPrincipal
	}
}

// marker is the managedBy field the listings render for key. g.mu must be held.
func (g *Gateway) marker(key string) any {
	if owner := g.managed[key]; owner != "" {
		return map[string]string{"principal": owner}
	}
	return nil
}

// release serves DELETE .../managed-by of the resource whose key is key(name) and that exists when
// exists(name): only its managing principal may clear the marker. g.mu must not be held.
func (g *Gateway) release(key func(string) string, exists func(string) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		g.mu.Lock()
		defer g.mu.Unlock()
		if !exists(name) {
			kind, _ := describe(key(name))
			writeJSON(w, http.StatusNotFound, map[string]string{"error": kind + " '" + name + "' not found"})
			return
		}
		if !g.allowChange(w, r, key(name)) {
			return
		}
		delete(g.managed, key(name))
		w.WriteHeader(http.StatusNoContent)
	}
}

// setMemberships puts the client name in exactly groups, which must all exist; otherwise it answers
// 400 and changes nothing. It answers 409 and changes nothing when that changes a group managed by
// another principal. g.mu must be held.
func (g *Gateway) setMemberships(w http.ResponseWriter, r *http.Request, name string, groups []string) bool {
	for _, group := range groups {
		if _, ok := g.groups[group]; !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "group '" + group + "' not found"})
			return false
		}
	}
	var changed []string
	for groupName, group := range g.groups {
		if slices.Contains(group.Clients, name) != slices.Contains(groups, groupName) {
			changed = append(changed, groupKey(groupName))
		}
	}
	if !g.allowChange(w, r, changed...) {
		return false
	}
	for groupName, group := range g.groups {
		clients := slices.DeleteFunc(slices.Clone(group.Clients), func(client string) bool { return client == name })
		if slices.Contains(groups, groupName) {
			clients = append(clients, name)
		}
		group.Clients = clients
		g.groups[groupName] = group
	}
	return true
}

// removeAuthClient deletes the client name and takes it out of every group. g.mu must be held.
func (g *Gateway) removeAuthClient(name string) {
	delete(g.authClients, name)
	delete(g.managed, authClientKey(name))
	for groupName, group := range g.groups {
		group.Clients = slices.DeleteFunc(slices.Clone(group.Clients), func(client string) bool { return client == name })
		g.groups[groupName] = group
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
