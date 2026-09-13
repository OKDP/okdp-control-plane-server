// Package auth checks the bearer token of a request against the platform's
// issuer. It knows nothing about HTTP, so the verification is testable without
// a server.
package auth

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Bounds the discovery call at startup and each later key refresh.
const issuerTimeout = 30 * time.Second

// Verifier reports whether a raw bearer token is one this platform issued.
type Verifier interface {
	Verify(ctx context.Context, rawToken string) error
}

// Config describes the identity provider to trust.
type Config struct {
	Issuer string
	// InsecureSkipVerify takes the issuer's certificate on trust.
	InsecureSkipVerify bool
	ClientID           string
}

type oidcVerifier struct {
	verifier *oidc.IDTokenVerifier
	clientID string
}

// azpClaims reads the two claims providers use to name the client, beyond aud:
// RFC 9068 also defines client_id, and Keycloak fills azp instead.
type azpClaims struct {
	Azp      string `json:"azp"`
	ClientID string `json:"client_id"`
}

// NewVerifier resolves the issuer's discovery document.
func NewVerifier(ctx context.Context, cfg Config) (Verifier, error) {
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

	// The audience is not checked: Keycloak puts the resource server in `aud`
	// and the asking client in `azp`, so the library's check rejects tokens
	// that are legitimately ours. Signature, issuer and expiry are.
	return &oidcVerifier{
		verifier: provider.Verifier(&oidc.Config{SkipClientIDCheck: true}),
		clientID: cfg.ClientID,
	}, nil
}

func (v *oidcVerifier) Verify(ctx context.Context, rawToken string) error {
	token, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return err
	}
	if v.clientID == "" {
		return nil
	}

	for _, aud := range token.Audience {
		if aud == v.clientID {
			return nil
		}
	}
	var claims azpClaims
	if err := token.Claims(&claims); err != nil {
		return fmt.Errorf("could not read the token claims: %w", err)
	}
	if claims.Azp == v.clientID || claims.ClientID == v.clientID {
		return nil
	}
	return errors.New("the token was not issued for this client")
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

// ResolveClientID prefers the environment override, then the platform Context
func ResolveClientID(ctx context.Context, override string, fromContext func(context.Context) (string, error)) (string, error) {
	if override != "" {
		return override, nil
	}
	return fromContext(ctx)
}
