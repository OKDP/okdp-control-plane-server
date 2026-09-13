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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTheEnvironmentIssuerWinsOverTheContext(t *testing.T) {
	issuer, err := ResolveIssuer(context.Background(), "https://override.example.org/realms/okdp",
		func(context.Context) (string, error) { return "https://context.example.org/realms/okdp", nil })

	require.NoError(t, err)
	assert.Equal(t, "https://override.example.org/realms/okdp", issuer)
}

// The normal deployment: nothing in the environment, the platform Context
// answers, and no chart or package carries the setting.
func TestTheIssuerComesFromTheContextWhenNothingOverridesIt(t *testing.T) {
	issuer, err := ResolveIssuer(context.Background(), "",
		func(context.Context) (string, error) { return "https://keycloak.okdp.sandbox/realms/master", nil })

	require.NoError(t, err)
	assert.Equal(t, "https://keycloak.okdp.sandbox/realms/master", issuer)
}

// Fail closed. Coming up with no issuer at all would mean serving the whole
// API to anyone who reaches the port, which is the state this is fixing.
func TestNoIssuerAnywhereIsAnError(t *testing.T) {
	_, err := ResolveIssuer(context.Background(), "", func(context.Context) (string, error) { return "", nil })

	require.Error(t, err)
	assert.Contains(t, err.Error(), "OIDC_ISSUER")
}

// An unreadable Context is not "no issuer": it must not be mistaken for a
// platform that declares none.
func TestAnUnreadableContextIsReported(t *testing.T) {
	_, err := ResolveIssuer(context.Background(), "",
		func(context.Context) (string, error) {
			return "", errors.New("contexts.kubocd.kubotal.io \"platform\" not found")
		})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// fakeIDP is a real OIDC provider the test owns: discovery document, key set
// and real RS256 signatures, so these tests exercise NewVerifier end to end
// rather than stub around it.
type fakeIDP struct {
	server *httptest.Server
	key    *rsa.PrivateKey
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	idp := &fakeIDP{key: key}

	mux := http.NewServeMux()
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.server.URL,
			"authorization_endpoint":                idp.server.URL + "/auth",
			"token_endpoint":                        idp.server.URL + "/token",
			"jwks_uri":                              idp.server.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &key.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig",
		}}})
	})
	return idp
}

func (f *fakeIDP) mint(t *testing.T, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: f.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-key"),
	)
	require.NoError(t, err)
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	object, err := signer.Sign(payload)
	require.NoError(t, err)
	raw, err := object.CompactSerialize()
	require.NoError(t, err)
	return raw
}

func (f *fakeIDP) claims() map[string]any {
	return map[string]any{
		"iss": f.server.URL,
		"sub": "e5a0-1234",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
}

// Every service on the platform shares the issuer, so the client check is
// what stops a token minted for a different one from working here.
func TestVerifyRejectsATokenIssuedForAnotherClient(t *testing.T) {
	idp := newFakeIDP(t)
	verifier, err := NewVerifier(context.Background(), Config{Issuer: idp.server.URL, ClientID: "okdp-ui"})
	require.NoError(t, err)

	c := idp.claims()
	c["aud"] = "some-other-app"

	err = verifier.Verify(context.Background(), idp.mint(t, c))
	assert.Error(t, err)
}

// Providers disagree on where they name the client: RFC 9068 puts it in aud
// and client_id, Keycloak fills azp. One rule has to cover the three.
func TestVerifyAcceptsTheClientFromWhicheverClaimNamesIt(t *testing.T) {
	idp := newFakeIDP(t)
	verifier, err := NewVerifier(context.Background(), Config{Issuer: idp.server.URL, ClientID: "okdp-ui"})
	require.NoError(t, err)

	for name, extra := range map[string]map[string]any{
		"aud":                       {"aud": "okdp-ui"},
		"aud as a list":             {"aud": []string{"account", "okdp-ui"}},
		"azp, as Keycloak fills it": {"aud": "account", "azp": "okdp-ui"},
		"client_id, also RFC 9068":  {"aud": "account", "client_id": "okdp-ui"},
	} {
		t.Run(name, func(t *testing.T) {
			c := idp.claims()
			for k, v := range extra {
				c[k] = v
			}
			err := verifier.Verify(context.Background(), idp.mint(t, c))
			assert.NoError(t, err)
		})
	}
}

// No client id configured means the check is skipped, not that every token
// fails: not every deployment publishes one yet.
func TestVerifySkipsTheClientCheckWhenNoneIsConfigured(t *testing.T) {
	idp := newFakeIDP(t)
	verifier, err := NewVerifier(context.Background(), Config{Issuer: idp.server.URL})
	require.NoError(t, err)

	c := idp.claims()
	c["aud"] = "anything-at-all"

	err = verifier.Verify(context.Background(), idp.mint(t, c))
	assert.NoError(t, err)
}

func TestResolveClientIDPrefersTheEnvironmentOverride(t *testing.T) {
	clientID, err := ResolveClientID(context.Background(), "okdp-ui",
		func(context.Context) (string, error) { return "from-context", nil })

	require.NoError(t, err)
	assert.Equal(t, "okdp-ui", clientID)
}

func TestResolveClientIDFallsBackToTheContext(t *testing.T) {
	clientID, err := ResolveClientID(context.Background(), "",
		func(context.Context) (string, error) { return "from-context", nil })

	require.NoError(t, err)
	assert.Equal(t, "from-context", clientID)
}

// Unlike the issuer, an empty client id is not an error: the caller treats it
// as "skip the check" rather than refusing to start.
func TestResolveClientIDIsEmptyWhenTheContextNamesNone(t *testing.T) {
	clientID, err := ResolveClientID(context.Background(), "",
		func(context.Context) (string, error) { return "", nil })

	require.NoError(t, err)
	assert.Empty(t, clientID)
}

// The rejection must not repeat the expected client id: it is exactly what an
// anonymous caller probing the API should not be handed.
func TestARejectedAudienceDoesNotNameTheExpectedClient(t *testing.T) {
	idp := newFakeIDP(t)
	verifier, err := NewVerifier(context.Background(), Config{Issuer: idp.server.URL, ClientID: "okdp-ui"})
	require.NoError(t, err)

	c := idp.claims()
	c["aud"] = "some-other-app"

	err = verifier.Verify(context.Background(), idp.mint(t, c))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "okdp-ui")
}
