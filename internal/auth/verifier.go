// Package auth verifies that a request carries a token the platform's own
// identity provider issued for the console. Authentication only: every
// verified caller is equal.
package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

// ErrNotConfigured is an operator fault, not a caller fault: no provider is
// declared, so no token can be verified.
var ErrNotConfigured = errors.New("this platform names no OIDC provider")

// Identity is what a verified token establishes about its bearer.
type Identity struct {
	Subject  string
	Username string
	Email    string
	Groups   []string
}

// OidcConfigSource is what the platform declares about its identity provider,
// read from the same Context the console bootstraps from. Two methods because
// two Context layouts exist: identity.oidc, and the older flat oidc.issuerUri.
type OidcConfigSource interface {
	GetIdentityOidcConfig(ctx context.Context) (*models.IdentityOidcConfig, error)
	GetOidcIssuerURI(ctx context.Context) (string, error)
}

// Fallback names the provider when the Context declares none.
type Fallback struct {
	Authority string
	ClientID  string
}

// Verifier checks tokens against the issuer the platform publishes. Built on
// first use and kept: building one reads the provider's discovery document
// over the network.
type Verifier struct {
	source   OidcConfigSource
	fallback Fallback

	// Keyed by issuer alone: the client a token names is compared per request,
	// the verifier cannot see it.
	mu       sync.Mutex
	issuer   string
	verifier *oidc.IDTokenVerifier
}

func NewVerifier(source OidcConfigSource, fallback Fallback) *Verifier {
	return &Verifier{source: source, fallback: fallback}
}

// Verify returns the identity a raw bearer token establishes, or an error
// naming why it establishes none.
func (v *Verifier) Verify(ctx context.Context, rawToken string) (*Identity, error) {
	issuer, clientID, err := v.resolve(ctx)
	if err != nil {
		return nil, err
	}
	verifier, err := v.verifierFor(ctx, issuer)
	if err != nil {
		return nil, err
	}

	token, err := verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, fmt.Errorf("the token was refused: %w", err)
	}

	var c tokenClaims
	if err := token.Claims(&c); err != nil {
		return nil, fmt.Errorf("the token carries no readable claims: %w", err)
	}

	// Every service on the platform shares the issuer and its key, so a token
	// handed to Superset verifies. It must also name this console.
	if !namesClient(token.Audience, c, clientID) {
		return nil, fmt.Errorf("the token was issued for another application, not for %q", clientID)
	}

	return &Identity{
		Subject:  token.Subject,
		Username: c.PreferredUsername,
		Email:    c.Email,
		Groups:   c.Groups,
	}, nil
}

// verifierFor returns the verifier for an issuer, building it the first time
// that issuer is seen and whenever the platform names another one.
func (v *Verifier) verifierFor(ctx context.Context, issuer string) (*oidc.IDTokenVerifier, error) {
	if verifier, ok := v.cached(issuer); ok {
		return verifier, nil
	}

	// Built outside the lock: discovery is network I/O, and holding the mutex
	// across it would stall every request on a slow issuer. Two requests on a
	// cold verifier both build one, which is the cheaper mistake.
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("the issuer %q could not be reached: %w", issuer, err)
	}
	// SkipClientIDCheck: go-oidc compares against aud alone, namesClient covers
	// the three claims providers actually use.
	verifier := provider.Verifier(&oidc.Config{SkipClientIDCheck: true})

	v.mu.Lock()
	defer v.mu.Unlock()
	// Keep one another request published for the same issuer meanwhile.
	if v.verifier != nil && v.issuer == issuer {
		return v.verifier, nil
	}
	v.verifier = verifier
	v.issuer = issuer
	return verifier, nil
}

// cached returns the verifier already published for this issuer.
func (v *Verifier) cached(issuer string) (*oidc.IDTokenVerifier, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.verifier != nil && v.issuer == issuer {
		return v.verifier, true
	}
	return nil, false
}

// resolve names the provider: the Context first, the deployment as fallback.
func (v *Verifier) resolve(ctx context.Context) (issuer, clientID string, err error) {
	cfg, err := v.source.GetIdentityOidcConfig(ctx)
	if err != nil {
		return "", "", fmt.Errorf("the platform OIDC configuration could not be read: %w", err)
	}
	if cfg != nil {
		issuer = strings.TrimSpace(cfg.Authority)
		clientID = strings.TrimSpace(cfg.ClientID)
	}

	if issuer == "" {
		legacy, err := v.source.GetOidcIssuerURI(ctx)
		if err != nil {
			return "", "", fmt.Errorf("the platform OIDC configuration could not be read: %w", err)
		}
		issuer = strings.TrimSpace(legacy)
	}
	if issuer == "" {
		issuer = strings.TrimSpace(v.fallback.Authority)
	}
	if clientID == "" {
		clientID = strings.TrimSpace(v.fallback.ClientID)
	}

	switch {
	case issuer == "" && clientID == "":
		return "", "", fmt.Errorf("%w, neither its issuer nor its client id is declared", ErrNotConfigured)
	case issuer == "":
		return "", "", fmt.Errorf("%w, its issuer is not declared", ErrNotConfigured)
	case clientID == "":
		return "", "", fmt.Errorf("%w, its client id is not declared", ErrNotConfigured)
	}
	return strings.TrimRight(issuer, "/"), clientID, nil
}

type tokenClaims struct {
	Azp               string   `json:"azp"`
	ClientID          string   `json:"client_id"`
	PreferredUsername string   `json:"preferred_username"`
	Email             string   `json:"email"`
	Groups            []string `json:"groups"`
}

// namesClient reports whether the token was issued for clientID. Providers
// disagree on the claim: RFC 9068 puts it in aud and client_id, Keycloak fills
// azp. One rule covers the three.
func namesClient(audience []string, c tokenClaims, clientID string) bool {
	for _, a := range audience {
		if a == clientID {
			return true
		}
	}
	return c.Azp == clientID || c.ClientID == clientID
}
