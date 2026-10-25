package kawa

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// requestTimeout bounds one admin API call, including waiting for consistency=applied.
const requestTimeout = 30 * time.Second

// Endpoint is a gateway's admin API: its URL, and the credentials and CA to call it with.
type Endpoint struct {
	BaseURL string
	// ClientID and ClientSecret authenticate with the client credentials grant; empty for a gateway
	// without admin authentication.
	ClientID     string
	ClientSecret string
	// CAPEM verifies an https admin API; empty for the system roots.
	CAPEM []byte
}

func (c Endpoint) key() string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		c.BaseURL,
		c.ClientID,
		c.ClientSecret,
		string(c.CAPEM),
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// Admin hands out the operator's admin API client for the endpoint it reads on every call, so a
// rotated secret or CA is picked up while the access token the client caches is reused and renewed
// across calls. A changed URL, credential or CA yields a new client and retires the previous one.
type Admin struct {
	endpoint func() (Endpoint, error)

	mu     sync.Mutex
	key    string
	client *Client
}

// NewAdmin is the admin API of the endpoint that endpoint reads.
func NewAdmin(endpoint func() (Endpoint, error)) *Admin {
	return &Admin{endpoint: endpoint}
}

// Client is the client for the current endpoint.
func (a *Admin) Client() (*Client, error) {
	endpoint, err := a.endpoint()
	if err != nil {
		return nil, err
	}
	key := endpoint.key()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil && a.key == key {
		return a.client, nil
	}
	client, err := ClientFor(endpoint)
	if err != nil {
		return nil, err
	}
	if a.client != nil {
		// Calls in flight finish; the retired client only stops keeping idle connections.
		a.client.http.CloseIdleConnections()
	}
	a.key, a.client = key, client
	return client, nil
}

// ClientFor is a client for endpoint. With client credentials it presents an access token for them,
// and without them (a gateway without admin authentication) it sends none.
func ClientFor(endpoint Endpoint) (*Client, error) {
	base := http.DefaultTransport.(*http.Transport).Clone()
	if len(endpoint.CAPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(endpoint.CAPEM) {
			return nil, errors.New("the gateway CA file holds no PEM certificate")
		}
		base.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}

	// plain calls a gateway without admin authentication, and the token endpoint.
	plain := &http.Client{Transport: base, Timeout: requestTimeout}
	httpClient := plain
	if endpoint.ClientID != "" {
		config := clientcredentials.Config{
			ClientID:     endpoint.ClientID,
			ClientSecret: endpoint.ClientSecret,
			TokenURL:     tokenURL(endpoint),
			AuthStyle:    oauth2.AuthStyleInHeader,
		}
		newSource := func() oauth2.TokenSource {
			return config.TokenSource(context.WithValue(context.Background(), oauth2.HTTPClient, plain))
		}
		httpClient = &http.Client{Transport: newRenewingTransport(base, newSource), Timeout: requestTimeout}
	}

	client := NewClient(endpoint.BaseURL, httpClient)
	client.principal = endpoint.ClientID
	return client, nil
}

func tokenURL(endpoint Endpoint) string {
	return strings.TrimRight(endpoint.BaseURL, "/") + "/oauth/token"
}

// renewingTransport authenticates requests with a cached token, and when the admin API answers 401
// (the gateway no longer accepts the token: restarted with another signing key or without the
// operator client), drops the cached token and retries the request once with a fresh one. A fresh
// token request then reports why it is refused. Every admin API call the operator makes is idempotent.
type renewingTransport struct {
	base      *http.Transport
	newSource func() oauth2.TokenSource

	mu     sync.Mutex
	source oauth2.TokenSource
}

func newRenewingTransport(base *http.Transport, newSource func() oauth2.TokenSource) *renewingTransport {
	return &renewingTransport{base: base, newSource: newSource, source: newSource()}
}

func (t *renewingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	source := t.source
	t.mu.Unlock()
	resp, err := t.send(req, source)
	if err != nil || resp.StatusCode != http.StatusUnauthorized || (req.Body != nil && req.GetBody == nil) {
		return resp, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// Requests that failed together renew once: only the first replaces the source they all used.
	t.mu.Lock()
	if t.source == source {
		t.source = t.newSource()
	}
	source = t.source
	t.mu.Unlock()

	retry := req.Clone(req.Context())
	if req.GetBody != nil {
		if retry.Body, err = req.GetBody(); err != nil {
			return nil, err
		}
	}
	return t.send(retry, source)
}

// CloseIdleConnections closes the idle connections of the base transport.
func (t *renewingTransport) CloseIdleConnections() {
	t.base.CloseIdleConnections()
}

func (t *renewingTransport) send(req *http.Request, source oauth2.TokenSource) (*http.Response, error) {
	return (&oauth2.Transport{Source: source, Base: t.base}).RoundTrip(req)
}
