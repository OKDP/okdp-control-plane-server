// Package auth checks the bearer token of a request against the platform's
// issuer. It knows nothing about HTTP, so the verification is testable without
// a server.
package auth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Bounds the discovery call at startup and each later key refresh.
const issuerTimeout = 30 * time.Second

// DefaultGroupsClaim is where Keycloak, Okta and Entra ID put group membership.
const DefaultGroupsClaim = "groups"

// Verifier validates a bearer token and returns the identity it represents.
type Verifier interface {
	Verify(ctx context.Context, rawToken string) (*Identity, error)
}

// Identity represents the user identified by a verified token.
type Identity struct {
	Subject  string
	Username string
	Groups   []string
}

// Config describes the identity provider to trust.
type Config struct {
	Issuer string
	// InsecureSkipVerify takes the issuer's certificate on trust.
	InsecureSkipVerify bool
	ClientID           string
	// GroupsClaim names the claim listing the caller's groups. Empty means
	// DefaultGroupsClaim.
	GroupsClaim string
}

type oidcVerifier struct {
	verifier    *oidc.IDTokenVerifier
	clientID    string
	groupsClaim string
}

// idClaims contains the claims needed by Verify to identify the client and user.
// RFC 9068 uses client_id, while Keycloak uses azp.
type idClaims struct {
	Azp               string `json:"azp"`
	ClientID          string `json:"client_id"`
	PreferredUsername string `json:"preferred_username"`
	Email             string `json:"email"`
}

// NewVerifier resolves the issuer's discovery document.
func NewVerifier(ctx context.Context, cfg Config) (Verifier, error) {
	if cfg.ClientID == "" {
		return nil, errors.New("no OIDC client id: a token would authenticate without proving it was issued for this API")
	}

	// go-oidc otherwise falls back to http.DefaultClient, which has no timeout.
	client := &http.Client{Timeout: issuerTimeout}
	if cfg.InsecureSkipVerify {
		// Cloned, not built from scratch: a bare Transport has no Proxy, and
		// the chart passes the egress proxy the pod must go through.
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		client.Transport = transport
	}

	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("could not reach the OIDC issuer %q: %w", cfg.Issuer, err)
	}

	groupsClaim := cfg.GroupsClaim
	if groupsClaim == "" {
		groupsClaim = DefaultGroupsClaim
	}

	// SkipClientIDCheck: Verify checks aud/azp/client_id itself, below.
	return &oidcVerifier{
		verifier:    provider.Verifier(&oidc.Config{SkipClientIDCheck: true}),
		clientID:    cfg.ClientID,
		groupsClaim: groupsClaim,
	}, nil
}

func (v *oidcVerifier) Verify(ctx context.Context, rawToken string) (*Identity, error) {
	token, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, err
	}

	var claims idClaims
	if err := token.Claims(&claims); err != nil {
		return nil, fmt.Errorf("could not read the token claims: %w", err)
	}

	named := claims.ClientID == v.clientID || claims.Azp == v.clientID
	for _, aud := range token.Audience {
		named = named || aud == v.clientID
	}
	if !named {
		return nil, errors.New("the token was not issued for this client")
	}

	username := claims.PreferredUsername
	if username == "" {
		username = claims.Email
	}

	groups, err := v.groups(token)
	if err != nil {
		return nil, err
	}
	return &Identity{Subject: token.Subject, Username: username, Groups: groups}, nil
}

// groups reads the groups claim, a list or a single string depending on the
// provider. An absent claim is no groups, not an error.
func (v *oidcVerifier) groups(token *oidc.IDToken) ([]string, error) {
	var all map[string]json.RawMessage
	if err := token.Claims(&all); err != nil {
		return nil, fmt.Errorf("could not read the token claims: %w", err)
	}
	raw, found := all[v.groupsClaim]
	if !found || string(raw) == "null" {
		return nil, nil
	}

	var groups []string
	if err := json.Unmarshal(raw, &groups); err == nil {
		return groups, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}, nil
	}
	return nil, fmt.Errorf("the %q claim is neither a string nor a list of strings", v.groupsClaim)
}

// ResolveIssuer prefers the environment override, then what the platform
// Context declares. Empty is an error: no issuer means no way to tell a real
// token from a forged one.
func ResolveIssuer(ctx context.Context, override string, fromContext func(context.Context) (string, error)) (string, error) {
	if override != "" {
		return override, nil
	}

	issuer, err := fromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("could not read the OIDC issuer from the platform Context: %w", err)
	}
	if issuer == "" {
		return "", errors.New("no OIDC issuer: the platform Context declares none and OIDC_ISSUER is unset")
	}
	return issuer, nil
}

// ResolveClientID mirrors ResolveIssuer.
func ResolveClientID(ctx context.Context, override string, fromContext func(context.Context) (string, error)) (string, error) {
	if override != "" {
		return override, nil
	}

	clientID, err := fromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("could not read the OIDC client id from the platform Context: %w", err)
	}
	if clientID == "" {
		return "", errors.New("no OIDC client id: the platform Context declares none and OIDC_CLIENT_ID is unset")
	}
	return clientID, nil
}
