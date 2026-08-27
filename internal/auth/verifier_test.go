package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

const testClientID = "okdp-ui"

// fakeIDP is an OIDC provider the tests own: it publishes a discovery document
// and a key set, and mints tokens signed by that key. Nothing is stubbed inside
// the verifier, so the tests exercise the real signature and issuer checks.
type fakeIDP struct {
	server *httptest.Server
	key    *rsa.PrivateKey
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("could not generate a signing key: %v", err)
	}
	idp := &fakeIDP{key: key}

	mux := http.NewServeMux()
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.server.URL,
			"authorization_endpoint":                idp.server.URL + "/auth",
			"token_endpoint":                        idp.server.URL + "/token",
			"jwks_uri":                              idp.server.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &key.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig",
		}}})
	})
	return idp
}

// mint signs the claims as they are given, so a test can produce a token no
// well-behaved provider would issue.
func (f *fakeIDP) mint(t *testing.T, claims map[string]any) string {
	t.Helper()
	return signWith(t, f.key, claims)
}

func signWith(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-key"),
	)
	if err != nil {
		t.Fatalf("could not build a signer: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("could not encode the claims: %v", err)
	}
	object, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("could not sign: %v", err)
	}
	raw, err := object.CompactSerialize()
	if err != nil {
		t.Fatalf("could not serialize: %v", err)
	}
	return raw
}

// claims returns what a healthy provider puts in a token for our console, with
// the audience left to the caller: which claim carries the client is exactly
// what varies between providers.
func (f *fakeIDP) claims() map[string]any {
	return map[string]any{
		"iss":                f.server.URL,
		"sub":                "e5a0-1234",
		"exp":                time.Now().Add(time.Hour).Unix(),
		"iat":                time.Now().Unix(),
		"preferred_username": "adm",
		"email":              "adm@okdp.local",
		"groups":             []string{"platform-admins"},
	}
}

type stubSource struct {
	cfg    *models.IdentityOidcConfig
	legacy string
	err    error
	read   int
}

func (s *stubSource) GetIdentityOidcConfig(context.Context) (*models.IdentityOidcConfig, error) {
	s.read++
	return s.cfg, s.err
}

func (s *stubSource) GetOidcIssuerURI(context.Context) (string, error) {
	return s.legacy, s.err
}

func sourceFor(idp *fakeIDP) *stubSource {
	return &stubSource{cfg: &models.IdentityOidcConfig{Authority: idp.server.URL, ClientID: testClientID}}
}

func TestVerifierAcceptsATokenIssuedForTheConsole(t *testing.T) {
	idp := newFakeIDP(t)
	v := NewVerifier(sourceFor(idp), Fallback{})

	c := idp.claims()
	c["aud"] = testClientID
	identity, err := v.Verify(context.Background(), idp.mint(t, c))
	if err != nil {
		t.Fatalf("a valid token was refused: %v", err)
	}
	if identity.Subject != "e5a0-1234" || identity.Username != "adm" || identity.Email != "adm@okdp.local" {
		t.Fatalf("the identity was not read from the token: %+v", identity)
	}
	if len(identity.Groups) != 1 || identity.Groups[0] != "platform-admins" {
		t.Fatalf("the groups were not read: %+v", identity.Groups)
	}
}

// Providers do not agree on where they name the client. Keycloak leaves aud at
// "account" and fills azp; RFC 9068 mandates aud and also defines client_id.
// One rule has to cover the three, or the check works on one platform only.
func TestVerifierReadsTheClientFromWhicheverClaimCarriesIt(t *testing.T) {
	idp := newFakeIDP(t)
	v := NewVerifier(sourceFor(idp), Fallback{})

	for name, extra := range map[string]map[string]any{
		"aud, as RFC 9068 requires":  {"aud": testClientID},
		"aud as a list":              {"aud": []string{"account", testClientID}},
		"azp, as Keycloak fills it":  {"aud": "account", "azp": testClientID},
		"client_id, also in RFC9068": {"aud": "account", "client_id": testClientID},
	} {
		t.Run(name, func(t *testing.T) {
			c := idp.claims()
			for k, val := range extra {
				c[k] = val
			}
			if _, err := v.Verify(context.Background(), idp.mint(t, c)); err != nil {
				t.Fatalf("the token was refused: %v", err)
			}
		})
	}
}

// The hole this whole change exists to close. Every service on the platform
// authenticates against the same issuer and is signed by the same key, so a
// token handed to Superset verifies perfectly. Only the client it names sets it
// apart from ours.
func TestVerifierRefusesATokenIssuedForAnotherApplication(t *testing.T) {
	idp := newFakeIDP(t)
	v := NewVerifier(sourceFor(idp), Fallback{})

	c := idp.claims()
	c["aud"] = "account"
	c["azp"] = "superset"

	_, err := v.Verify(context.Background(), idp.mint(t, c))
	if err == nil {
		t.Fatal("a token issued for superset opened the control plane")
	}
	if errors.Is(err, ErrNotConfigured) {
		t.Fatalf("the refusal was reported as a configuration fault: %v", err)
	}
}

func TestVerifierRefusesAnExpiredToken(t *testing.T) {
	idp := newFakeIDP(t)
	v := NewVerifier(sourceFor(idp), Fallback{})

	c := idp.claims()
	c["aud"] = testClientID
	c["exp"] = time.Now().Add(-time.Minute).Unix()

	if _, err := v.Verify(context.Background(), idp.mint(t, c)); err == nil {
		t.Fatal("an expired token was accepted")
	}
}

// A token signed by a key the issuer does not publish is a forgery, whatever it
// claims about itself.
func TestVerifierRefusesAForeignSignature(t *testing.T) {
	idp := newFakeIDP(t)
	v := NewVerifier(sourceFor(idp), Fallback{})

	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("could not generate a second key: %v", err)
	}
	c := idp.claims()
	c["aud"] = testClientID

	if _, err := v.Verify(context.Background(), signWith(t, other, c)); err == nil {
		t.Fatal("a token signed by an unknown key was accepted")
	}
}

func TestVerifierRefusesATokenFromAnotherIssuer(t *testing.T) {
	idp := newFakeIDP(t)
	v := NewVerifier(sourceFor(idp), Fallback{})

	c := idp.claims()
	c["iss"] = "https://elsewhere.example/realms/master"
	c["aud"] = testClientID

	if _, err := v.Verify(context.Background(), idp.mint(t, c)); err == nil {
		t.Fatal("a token claiming another issuer was accepted")
	}
}

func TestVerifierRefusesRubbish(t *testing.T) {
	idp := newFakeIDP(t)
	v := NewVerifier(sourceFor(idp), Fallback{})

	for _, raw := range []string{"", "not-a-jwt", "a.b.c", "Bearer something"} {
		if _, err := v.Verify(context.Background(), raw); err == nil {
			t.Fatalf("%q was accepted as a token", raw)
		}
	}
}

// An unconfigured platform is an operator's problem, not the caller's, and the
// two must not read the same on screen.
func TestVerifierSaysWhenThePlatformPublishesNoOidcClient(t *testing.T) {
	for name, src := range map[string]*stubSource{
		"no block at all": {cfg: nil},
		"no authority":    {cfg: &models.IdentityOidcConfig{ClientID: testClientID}},
		"no client id":    {cfg: &models.IdentityOidcConfig{Authority: "https://issuer.example"}},
		"blank authority": {cfg: &models.IdentityOidcConfig{Authority: "   ", ClientID: testClientID}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewVerifier(src, Fallback{}).Verify(context.Background(), "any.token.here")
			if !errors.Is(err, ErrNotConfigured) {
				t.Fatalf("wanted ErrNotConfigured, got %v", err)
			}
		})
	}
}

// Reaching the discovery document costs a network round trip, so it must happen
// once and not on every call.
func TestVerifierBuildsTheProviderOnceAndRebuildsWhenTheIssuerChanges(t *testing.T) {
	first := newFakeIDP(t)
	src := sourceFor(first)
	v := NewVerifier(src, Fallback{})

	c := first.claims()
	c["aud"] = testClientID
	raw := first.mint(t, c)

	for i := 0; i < 3; i++ {
		if _, err := v.Verify(context.Background(), raw); err != nil {
			t.Fatalf("call %d was refused: %v", i, err)
		}
	}
	if v.issuer != first.server.URL {
		t.Fatalf("the verifier kept another issuer: %q", v.issuer)
	}

	// The platform is repointed at another provider. A token from the first one
	// must stop being accepted, or a decommissioned issuer would keep opening
	// the API.
	second := newFakeIDP(t)
	src.cfg = &models.IdentityOidcConfig{Authority: second.server.URL, ClientID: testClientID}

	if _, err := v.Verify(context.Background(), raw); err == nil {
		t.Fatal("a token from the previous issuer was still accepted")
	}

	c2 := second.claims()
	c2["aud"] = testClientID
	if _, err := v.Verify(context.Background(), second.mint(t, c2)); err != nil {
		t.Fatalf("a token from the new issuer was refused: %v", err)
	}
}

// The issuer published with a trailing slash is the same issuer.
func TestVerifierToleratesATrailingSlashOnTheAuthority(t *testing.T) {
	idp := newFakeIDP(t)
	v := NewVerifier(&stubSource{cfg: &models.IdentityOidcConfig{
		Authority: idp.server.URL + "/", ClientID: testClientID,
	}}, Fallback{})

	c := idp.claims()
	c["aud"] = testClientID
	if _, err := v.Verify(context.Background(), idp.mint(t, c)); err != nil {
		t.Fatalf("an authority with a trailing slash was refused: %v", err)
	}
}

// The layout the running platform actually has. Its Context carries no
// identity block at all: the issuer lives in the older flat oidc.issuerUri, and
// no clientId is published anywhere, so the deployment supplies it. Reading
// only the new layout would have answered 401 to every call on that cluster.
func TestVerifierReadsTheIssuerFromTheOlderLayout(t *testing.T) {
	idp := newFakeIDP(t)
	v := NewVerifier(
		&stubSource{cfg: nil, legacy: idp.server.URL},
		Fallback{ClientID: testClientID},
	)

	c := idp.claims()
	c["aud"] = "account"
	c["azp"] = testClientID
	if _, err := v.Verify(context.Background(), idp.mint(t, c)); err != nil {
		t.Fatalf("a platform declaring its issuer the older way was refused: %v", err)
	}
}

// A Context that declares nothing at all leaves the deployment to name the
// provider, the way the console is already told which one to use.
func TestVerifierFallsBackToTheDeploymentWhenTheContextDeclaresNothing(t *testing.T) {
	idp := newFakeIDP(t)
	v := NewVerifier(
		&stubSource{},
		Fallback{Authority: idp.server.URL, ClientID: testClientID},
	)

	c := idp.claims()
	c["aud"] = testClientID
	if _, err := v.Verify(context.Background(), idp.mint(t, c)); err != nil {
		t.Fatalf("the deployment fallback was not used: %v", err)
	}
}

// What the platform declares outranks what the deployment was told, so a
// Context stays the one truth wherever it publishes one.
func TestWhatTheContextDeclaresOutranksTheDeployment(t *testing.T) {
	declared := newFakeIDP(t)
	other := newFakeIDP(t)

	v := NewVerifier(
		&stubSource{cfg: &models.IdentityOidcConfig{Authority: declared.server.URL, ClientID: testClientID}},
		Fallback{Authority: other.server.URL, ClientID: "someone-else"},
	)

	c := declared.claims()
	c["aud"] = testClientID
	if _, err := v.Verify(context.Background(), idpMintFor(t, declared, c)); err != nil {
		t.Fatalf("the declared issuer was not used: %v", err)
	}

	c2 := other.claims()
	c2["aud"] = "someone-else"
	if _, err := v.Verify(context.Background(), idpMintFor(t, other, c2)); err == nil {
		t.Fatal("the deployment fallback overrode what the Context declares")
	}
}

func idpMintFor(t *testing.T, idp *fakeIDP, claims map[string]any) string {
	t.Helper()
	return idp.mint(t, claims)
}

// Neither declared nor configured is an operator's problem, not a caller's.
func TestNothingAnywhereIsAConfigurationFault(t *testing.T) {
	_, err := NewVerifier(&stubSource{}, Fallback{}).Verify(context.Background(), "any.token.here")
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("wanted ErrNotConfigured, got %v", err)
	}
}
