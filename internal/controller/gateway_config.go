package controller

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/kawa-platform/kawa-operator/internal/kawa"
)

// ConditionReady is the condition VirtualTopics, Roles, Groups and Clients report.
const ConditionReady = "Ready"

// Reasons of the Ready condition.
const (
	ReasonSynced               = "Synced"
	ReasonAuthenticationFailed = "AuthenticationFailed"
	ReasonGatewayUnavailable   = "GatewayUnavailable"
	ReasonGovernanceRejected   = "GovernanceRejected"
	ReasonInvalidSpec          = "InvalidSpec"
	ReasonAlreadyExists        = "AlreadyExists"
	ReasonConflict             = "Conflict"
	ReasonNamespaceNotAllowed  = "NamespaceNotAllowed"
	ReasonNamespaceUnreadable  = "NamespaceUnreadable"
	ReasonGatewayConfigInvalid = "GatewayConfigInvalid"
	ReasonAPIUnavailable       = "APIUnavailable"
	ReasonDeletionRejected     = "DeletionRejected"
	ReasonSecretUnavailable    = "SecretUnavailable"
)

// GatewayConfig is how the operator reaches the gateway's admin API, configured once for the whole
// operator. The secret and CA are files (mounted Secrets) read on every use, so a rotated Secret is
// picked up without a restart.
type GatewayConfig struct {
	// AdminURL is the base URL of the gateway's admin HTTP API.
	AdminURL string
	// ClientID and ClientSecretFile are the operator's service client on the gateway; both empty for a
	// gateway without admin authentication.
	ClientID         string
	ClientSecretFile string
	// CAFile holds PEM CA certificates for an https admin API; empty for the system roots.
	CAFile string
}

// Validate reports a configuration the operator cannot start with.
func (c GatewayConfig) Validate() error {
	u, err := url.Parse(c.AdminURL)
	if c.AdminURL == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("--gateway-admin-url must be an absolute http:// or https:// URL, got %q", c.AdminURL)
	}
	if (c.ClientID == "") != (c.ClientSecretFile == "") {
		return errors.New("--gateway-client-id and --gateway-client-secret-file must be set together")
	}
	return nil
}

// Endpoint reads the configured files into an endpoint.
func (c GatewayConfig) Endpoint() (kawa.Endpoint, error) {
	endpoint := kawa.Endpoint{BaseURL: c.AdminURL, ClientID: c.ClientID}
	if c.ClientSecretFile != "" {
		secret, err := os.ReadFile(c.ClientSecretFile)
		if err != nil {
			return kawa.Endpoint{}, fmt.Errorf("reading the gateway client secret: %w", err)
		}
		endpoint.ClientSecret = strings.TrimSpace(string(secret))
	}
	if c.CAFile != "" {
		ca, err := os.ReadFile(c.CAFile)
		if err != nil {
			return kawa.Endpoint{}, fmt.Errorf("reading the gateway CA: %w", err)
		}
		endpoint.CAPEM = ca
	}
	return endpoint, nil
}

// unavailableReason is the Ready reason for a failed admin API call that the spec cannot fix: a
// refused authentication, or the gateway being unreachable or failing.
func unavailableReason(err error) string {
	switch {
	case kawa.IsAuthenticationError(err):
		return ReasonAuthenticationFailed
	default:
		return ReasonGatewayUnavailable
	}
}
