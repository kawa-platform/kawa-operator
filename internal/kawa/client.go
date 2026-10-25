// Package kawa is a client for the kawa gateway admin HTTP API, limited to what the operator manages.
package kawa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/oauth2"
)

// VirtualTopic is a virtual topic as the admin API stores it.
type VirtualTopic struct {
	// Topic is the physical topic.
	Topic               string
	Filter              *Filter
	ExposePhysicalTopic bool
	ValueFormat         *PayloadFormat
}

// Filter is a consume filter: Type is headerEquals, headerContains, headerStartsWith,
// headerMatches (Header and Value) or cel (Expression).
type Filter struct {
	Type       string `json:"type"`
	Header     string `json:"header,omitempty"`
	Value      string `json:"value,omitempty"`
	Expression string `json:"expression,omitempty"`
}

// PayloadFormat is the encoding of record values: Type json or avro.
type PayloadFormat struct {
	Type          string `json:"type"`
	OnDecodeError string `json:"onDecodeError,omitempty"`
}

// ListedVirtualTopic is a virtual topic in the GET /topics listing. The listing renders the filter
// as a display form ({kind, expression}), not the stored config, so it is compared through
// [Filter.View].
type ListedVirtualTopic struct {
	Name                string
	PhysicalTopic       string
	Filter              *FilterView
	ExposePhysicalTopic bool
	ValueFormat         *PayloadFormat
	ManagedBy           *ManagedBy
}

// ManagedBy marks a gateway resource as owned by Principal: the gateway refuses changes to it from
// anyone else. The gateway stamps it on writes by the operator; nil means unmanaged.
type ManagedBy struct {
	Principal string `json:"principal"`
}

// FilterView is the display form of a filter in the GET /topics listing.
type FilterView struct {
	Kind       string `json:"kind"`
	Expression string `json:"expression"`
}

// Role is an RBAC role as the admin API stores it.
type Role struct {
	ACLs []ACL `json:"acls"`
}

// Group is an RBAC group as the admin API stores it: clients, by authenticated name, and the
// roles whose ACLs they inherit.
type Group struct {
	Clients []string `json:"clients"`
	Roles   []string `json:"roles"`
}

// Equal is whether g and other list the same clients and roles in the same order. An empty list
// equals a missing one: the admin API lists a group without clients with an empty list.
func (g Group) Equal(other Group) bool {
	return slices.Equal(g.Clients, other.Clients) && slices.Equal(g.Roles, other.Roles)
}

// ListedRole is a role in the GET /rbac/roles listing. The marker is kept apart from the [Role]
// the operator writes, so comparing roles ignores it.
type ListedRole struct {
	Role
	ManagedBy *ManagedBy
}

// ListedGroup is a group in the GET /rbac/groups listing.
type ListedGroup struct {
	Group
	ManagedBy *ManagedBy
}

// AuthClient is a SASL client as the admin API lists it. The admin API never returns the password.
type AuthClient struct {
	Mechanism string
}

// ListedAuthClient is a client in the GET /auth/clients listing.
type ListedAuthClient struct {
	AuthClient
	ManagedBy *ManagedBy
}

// ACL allows or denies (Permission ALLOW or DENY) one Operation on Resource.
type ACL struct {
	Resource   Resource `json:"resource"`
	Operation  string   `json:"operation"`
	Permission string   `json:"permission"`
}

// Resource is what an ACL applies to: Type TOPIC, GROUP, TRANSACTIONAL_ID or CLUSTER, matched by
// Pattern as PatternType LITERAL or PREFIXED. Pattern is nil for CLUSTER.
type Resource struct {
	Type        string  `json:"type"`
	Pattern     *string `json:"pattern"`
	PatternType string  `json:"patternType"`
}

// View is the display form GET /topics renders for this filter.
func (f *Filter) View() *FilterView {
	if f == nil {
		return nil
	}
	switch f.Type {
	case "headerEquals":
		return &FilterView{Kind: "header", Expression: f.Header + "=" + f.Value}
	case "headerContains":
		return &FilterView{Kind: "headerContains", Expression: f.Header + " contains " + f.Value}
	case "headerStartsWith":
		return &FilterView{Kind: "headerStartsWith", Expression: f.Header + " starts with " + f.Value}
	case "headerMatches":
		return &FilterView{Kind: "headerMatches", Expression: f.Header + " matches " + f.Value}
	default:
		return &FilterView{Kind: f.Type, Expression: f.Expression}
	}
}

// Topics is the GET /topics listing, split by kind.
type Topics struct {
	Virtual  map[string]ListedVirtualTopic
	Physical map[string]bool
}

// APIError is a non-2xx answer of the admin API.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("kawa admin API answered %d: %s", e.StatusCode, e.Message)
}

// StatusCode is the HTTP status of err when it is an [APIError], or 0.
func StatusCode(err error) int {
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		return apiErr.StatusCode
	}
	return 0
}

// IsAuthenticationError is whether err means the operator could not authenticate: the token
// endpoint refused the client credentials, or the admin API refused the token.
func IsAuthenticationError(err error) bool {
	var tokenErr *oauth2.RetrieveError
	return errors.As(err, &tokenErr) || StatusCode(err) == http.StatusUnauthorized
}

// Client calls one gateway's admin API. The HTTP client carries authentication (see [Clients.For]).
type Client struct {
	baseURL string
	http    *http.Client
	// principal is who the gateway authenticates the calls as: the operator's client id, or empty
	// without admin authentication.
	principal string
}

// NewClient is a client for the admin API at baseURL.
func NewClient(baseURL string, httpClient *http.Client) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: httpClient}
}

// Manages is whether a resource marked m is managed by the caller, so that the gateway refuses
// changes to it from anyone else. Without admin authentication the gateway marks nothing, so every
// resource counts as managed.
func (c *Client) Manages(m *ManagedBy) bool {
	return c.principal == "" || (m != nil && m.Principal == c.principal)
}

// ListTopics reads GET /topics.
func (c *Client) ListTopics(ctx context.Context) (*Topics, error) {
	var listing []struct {
		Type                string         `json:"type"`
		Name                string         `json:"name"`
		Filter              *FilterView    `json:"filter"`
		PhysicalTopic       string         `json:"physicalTopic"`
		ExposePhysicalTopic *bool          `json:"exposePhysicalTopic"`
		ValueFormat         *PayloadFormat `json:"valueFormat"`
		ManagedBy           *ManagedBy     `json:"managedBy"`
	}
	if err := c.do(ctx, http.MethodGet, "/topics", nil, &listing); err != nil {
		return nil, err
	}
	topics := &Topics{Virtual: map[string]ListedVirtualTopic{}, Physical: map[string]bool{}}
	for _, t := range listing {
		switch t.Type {
		case "virtual":
			topics.Virtual[t.Name] = ListedVirtualTopic{
				Name:                t.Name,
				PhysicalTopic:       t.PhysicalTopic,
				Filter:              t.Filter,
				ExposePhysicalTopic: t.ExposePhysicalTopic != nil && *t.ExposePhysicalTopic,
				ValueFormat:         t.ValueFormat,
				ManagedBy:           t.ManagedBy,
			}
		case "physical":
			topics.Physical[t.Name] = true
		}
	}
	return topics, nil
}

// PutVirtualTopic upserts the virtual topic name (PUT /topics/{name}) and waits until the gateway
// cluster applied it.
func (c *Client) PutVirtualTopic(ctx context.Context, name string, topic VirtualTopic) error {
	body := struct {
		Type                string         `json:"type"`
		Topic               string         `json:"topic"`
		Filter              *Filter        `json:"filter,omitempty"`
		ExposePhysicalTopic bool           `json:"exposePhysicalTopic"`
		ValueFormat         *PayloadFormat `json:"valueFormat,omitempty"`
	}{"virtual", topic.Topic, topic.Filter, topic.ExposePhysicalTopic, topic.ValueFormat}
	return c.do(ctx, http.MethodPut, "/topics/"+url.PathEscape(name)+"?consistency=applied", body, nil)
}

// DeleteVirtualTopic removes the virtual topic name (DELETE /topics/{name}) and waits until the
// gateway cluster applied it.
//
// The admin API deletes the physical topic on the broker when name is not a virtual topic, so the
// caller must have seen name listed as virtual right before.
func (c *Client) DeleteVirtualTopic(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/topics/"+url.PathEscape(name)+"?consistency=applied", nil, nil)
}

// ReleaseVirtualTopic clears the caller's managed-by marker of the virtual topic name (DELETE
// /topics/{name}/managed-by), leaving the topic, and waits until the gateway cluster applied it.
func (c *Client) ReleaseVirtualTopic(ctx context.Context, name string) error {
	return c.release(ctx, "/topics/"+url.PathEscape(name))
}

// ListRoles reads GET /rbac/roles, by role name.
func (c *Client) ListRoles(ctx context.Context) (map[string]ListedRole, error) {
	var listing []struct {
		Name      string     `json:"name"`
		ACLs      []ACL      `json:"acls"`
		ManagedBy *ManagedBy `json:"managedBy"`
	}
	if err := c.do(ctx, http.MethodGet, "/rbac/roles", nil, &listing); err != nil {
		return nil, err
	}
	roles := make(map[string]ListedRole, len(listing))
	for _, r := range listing {
		roles[r.Name] = ListedRole{Role: Role{ACLs: r.ACLs}, ManagedBy: r.ManagedBy}
	}
	return roles, nil
}

// PutRole upserts the role name (PUT /rbac/roles/{name}) and waits until the gateway cluster
// applied it.
func (c *Client) PutRole(ctx context.Context, name string, role Role) error {
	return c.do(ctx, http.MethodPut, "/rbac/roles/"+url.PathEscape(name)+"?consistency=applied", role, nil)
}

// DeleteRole removes the role name (DELETE /rbac/roles/{name}), and with it the role from every
// group that lists it, and waits until the gateway cluster applied it.
func (c *Client) DeleteRole(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/rbac/roles/"+url.PathEscape(name)+"?consistency=applied", nil, nil)
}

// ReleaseRole clears the caller's managed-by marker of the role name (DELETE
// /rbac/roles/{name}/managed-by), leaving the role, and waits until the gateway cluster applied it.
func (c *Client) ReleaseRole(ctx context.Context, name string) error {
	return c.release(ctx, "/rbac/roles/"+url.PathEscape(name))
}

// ListGroups reads GET /rbac/groups, by group name.
func (c *Client) ListGroups(ctx context.Context) (map[string]ListedGroup, error) {
	var listing []struct {
		Name      string     `json:"name"`
		Clients   []string   `json:"clients"`
		Roles     []string   `json:"roles"`
		ManagedBy *ManagedBy `json:"managedBy"`
	}
	if err := c.do(ctx, http.MethodGet, "/rbac/groups", nil, &listing); err != nil {
		return nil, err
	}
	groups := make(map[string]ListedGroup, len(listing))
	for _, g := range listing {
		groups[g.Name] = ListedGroup{Group: Group{Clients: g.Clients, Roles: g.Roles}, ManagedBy: g.ManagedBy}
	}
	return groups, nil
}

// PutGroup upserts the group name (PUT /rbac/groups/{name}) and waits until the gateway cluster
// applied it.
func (c *Client) PutGroup(ctx context.Context, name string, group Group) error {
	return c.do(ctx, http.MethodPut, "/rbac/groups/"+url.PathEscape(name)+"?consistency=applied", group, nil)
}

// DeleteGroup removes the group name (DELETE /rbac/groups/{name}) and waits until the gateway
// cluster applied it. The gateway refuses (409) to delete a group that still has clients.
func (c *Client) DeleteGroup(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/rbac/groups/"+url.PathEscape(name)+"?consistency=applied", nil, nil)
}

// ReleaseGroup clears the caller's managed-by marker of the group name (DELETE
// /rbac/groups/{name}/managed-by), leaving the group, and waits until the gateway cluster applied it.
func (c *Client) ReleaseGroup(ctx context.Context, name string) error {
	return c.release(ctx, "/rbac/groups/"+url.PathEscape(name))
}

// ListAuthClients reads GET /auth/clients, by username.
func (c *Client) ListAuthClients(ctx context.Context) (map[string]ListedAuthClient, error) {
	var listing []struct {
		Username  string     `json:"username"`
		Mechanism string     `json:"mechanism"`
		ManagedBy *ManagedBy `json:"managedBy"`
	}
	if err := c.do(ctx, http.MethodGet, "/auth/clients", nil, &listing); err != nil {
		return nil, err
	}
	clients := make(map[string]ListedAuthClient, len(listing))
	for _, l := range listing {
		clients[l.Username] = ListedAuthClient{AuthClient: AuthClient{Mechanism: l.Mechanism}, ManagedBy: l.ManagedBy}
	}
	return clients, nil
}

// PutAuthClient creates or replaces the client name (PUT /auth/clients/{name}) with password, and
// waits until the gateway cluster applied it. The client ends up in exactly groups: the admin API
// takes it out of every other group.
func (c *Client) PutAuthClient(ctx context.Context, name string, client AuthClient, password string, groups []string) error {
	body := struct {
		Mechanism string   `json:"mechanism"`
		Password  string   `json:"password"`
		Groups    []string `json:"groups"`
	}{client.Mechanism, password, append([]string{}, groups...)}
	return c.do(ctx, http.MethodPut, "/auth/clients/"+url.PathEscape(name)+"?consistency=applied", body, nil)
}

// PatchAuthClient sets the mechanism and password of the existing client name (PATCH
// /auth/clients/{name}), keeping its groups, and waits until the gateway cluster applied it.
func (c *Client) PatchAuthClient(ctx context.Context, name string, client AuthClient, password string) error {
	body := struct {
		Mechanism string `json:"mechanism"`
		Password  string `json:"password"`
	}{client.Mechanism, password}
	return c.do(ctx, http.MethodPatch, "/auth/clients/"+url.PathEscape(name)+"?consistency=applied", body, nil)
}

// DeleteAuthClient removes the client name (DELETE /auth/clients/{name}), and with it the client
// from every group that lists it, and waits until the gateway cluster applied it.
func (c *Client) DeleteAuthClient(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/auth/clients/"+url.PathEscape(name)+"?consistency=applied", nil, nil)
}

// ReleaseAuthClient clears the caller's managed-by marker of the client name (DELETE
// /auth/clients/{name}/managed-by), leaving the client, and waits until the gateway cluster applied
// it.
func (c *Client) ReleaseAuthClient(ctx context.Context, name string) error {
	return c.release(ctx, "/auth/clients/"+url.PathEscape(name))
}

// release clears the managed-by marker of the resource at path. The gateway answers 204 also for a
// resource already unmanaged, 404 for one missing, and 409 for one managed by someone else.
func (c *Client) release(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodDelete, path+"/managed-by?consistency=applied", nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("%s %s: reading the response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{StatusCode: resp.StatusCode, Message: errorMessage(payload)}
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("%s %s: decoding the response: %w", method, path, err)
	}
	return nil
}

// errorMessage is the admin API's {"error": "..."} message, or the raw body.
func errorMessage(payload []byte) string {
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(payload, &body) == nil && body.Error != "" {
		return body.Error
	}
	return strings.TrimSpace(string(payload))
}
