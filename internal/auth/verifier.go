// Package auth establishes who is calling the API.
//
// It answers one question, "is this request carried by a token the platform's
// own identity provider issued for this console", and nothing more. No role,
// no permission: every verified caller is equal until an authorization model
// exists.
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

// ErrNotConfigured means the platform publishes no OIDC client, so no token can
// be verified. Kept apart from a rejection: nothing is wrong with the caller,
// the platform simply has not been told which provider to trust, and the
// message must send the operator to the Context rather than to the user.
var ErrNotConfigured = errors.New("the platform publishes no OIDC configuration")

// Identity is what a verified token establishes about its bearer.
type Identity struct {
	Subject  string
	Username string
	Email    string
	Groups   []string
}

// OidcConfigSource publishes what the platform declares about its identity
// provider. The Context repository satisfies it, and the console reads the same
// object through /api/capabilities: server and console therefore trust one
// issuer by construction, and cannot drift apart.
//
// Two methods because two layouts describe the same thing. A Context written
// today carries identity.oidc.authority; one written before that block existed
// carries oidc.issuerUri and nothing else. Reading both means a running
// platform does not have to be edited before its API can be authenticated.
type OidcConfigSource interface {
	GetIdentityOidcConfig(ctx context.Context) (*models.IdentityOidcConfig, error)
	GetOidcIssuerURI(ctx context.Context) (string, error)
}

// Fallback names the provider when the Context declares none. It carries the
// same variable names as the console's own configuration, because the two must
// name one provider.
type Fallback struct {
	Authority string
	ClientID  string
}

// Verifier checks tokens against the issuer the platform publishes.
//
// The issuer is not known at boot: it lives in the Context, which the server
// reads at request time. The verifier is therefore built on first use and kept,
// and rebuilt only when the published issuer changes. Building it reaches the
// provider's discovery document, so doing it per request would put a network
// round trip on every call.
type Verifier struct {
	source   OidcConfigSource
	fallback Fallback

	mu       sync.Mutex
	issuer   string
	clientID string
	verifier *oidc.IDTokenVerifier
}

func NewVerifier(source OidcConfigSource, fallback Fallback) *Verifier {
	return &Verifier{source: source, fallback: fallback}
}

// Verify returns the identity a raw bearer token establishes, or an error
// naming why it establishes none.
func (v *Verifier) Verify(ctx context.Context, rawToken string) (*Identity, error) {
	verifier, clientID, err := v.current(ctx)
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

	// The signature only proves the issuer minted it, not that it minted it for
	// this API. Every service on the platform authenticates against the same
	// issuer and is signed by the same key, so without this a token handed to
	// Superset or Trino would open the control plane.
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

// current returns the verifier for the issuer the platform publishes now,
// building it when the issuer is seen for the first time or has changed.
func (v *Verifier) current(ctx context.Context) (*oidc.IDTokenVerifier, string, error) {
	issuer, clientID, err := v.resolve(ctx)
	if err != nil {
		return nil, "", err
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	if v.verifier != nil && v.issuer == issuer && v.clientID == clientID {
		return v.verifier, v.clientID, nil
	}

	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, "", fmt.Errorf("the issuer %q could not be reached: %w", issuer, err)
	}
	// SkipClientIDCheck because go-oidc would compare the configured id against
	// aud alone. Which claim carries it depends on the provider, so the
	// comparison is done here, over the three places it can appear.
	v.verifier = provider.Verifier(&oidc.Config{SkipClientIDCheck: true})
	v.issuer = issuer
	v.clientID = clientID
	return v.verifier, v.clientID, nil
}

// resolve names the provider, preferring what the platform declares over what
// the deployment was told, so a Context that publishes it stays the one truth.
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

	if issuer == "" || clientID == "" {
		return "", "", ErrNotConfigured
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

// namesClient reports whether the token says it was issued for clientID.
//
// Providers disagree on where they write it. RFC 9068 requires aud on a JWT
// access token and also defines client_id; OIDC Core defines azp for ID tokens
// and Keycloak fills it. Reading the three keeps one rule working across
// providers, instead of asking an operator to declare which field theirs uses.
func namesClient(audience []string, c tokenClaims, clientID string) bool {
	for _, a := range audience {
		if a == clientID {
			return true
		}
	}
	return c.Azp == clientID || c.ClientID == clientID
}
