package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/models"
)

type noOidc struct{}

func (noOidc) GetIdentityOidcConfig(context.Context) (*models.IdentityOidcConfig, error) {
	return nil, nil
}

func (noOidc) GetOidcIssuerURI(context.Context) (string, error) { return "", nil }

type configured struct{}

func (configured) GetIdentityOidcConfig(context.Context) (*models.IdentityOidcConfig, error) {
	return &models.IdentityOidcConfig{Authority: "https://issuer.invalid", ClientID: "okdp-ui"}, nil
}

func (configured) GetOidcIssuerURI(context.Context) (string, error) { return "", nil }

func guarded(source auth.OidcConfigSource) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api", RequireAuth(auth.NewVerifier(source, auth.Fallback{})))
	api.GET("/capabilities", func(c *gin.Context) { c.String(http.StatusOK, "bootstrap") })
	api.GET("/projects", func(c *gin.Context) { c.String(http.StatusOK, "reached") })
	return r
}

func call(t *testing.T, r *gin.Engine, path, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func message(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal is not JSON: %s", w.Body.String())
	}
	return body.Error
}

func TestARequestWithNoUsableTokenIsRefused(t *testing.T) {
	r := guarded(configured{})

	for name, header := range map[string]string{
		"no header at all":     "",
		"another scheme":       "Basic dXNlcjpwYXNz",
		"the scheme alone":     "Bearer",
		"an empty credential":  "Bearer ",
		"only whitespace":      "Bearer    ",
		"the token unprefixed": "eyJhbGciOiJSUzI1NiJ9.e30.x",
	} {
		t.Run(name, func(t *testing.T) {
			w := call(t, r, "/api/projects", header)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("answered %d, wanted 401", w.Code)
			}
			if !strings.Contains(message(t, w), "no bearer token") {
				t.Fatalf("the reason was not named: %q", message(t, w))
			}
		})
	}
}

// RFC 7235 makes the scheme case-insensitive, and clients do send "bearer".
// Refusing it for its case would look like a rejected token and send the reader
// looking at their identity provider.
func TestTheSchemeIsReadWithoutRegardToCase(t *testing.T) {
	r := guarded(configured{})

	for _, header := range []string{"bearer abc.def.ghi", "BEARER abc.def.ghi", "BeArEr abc.def.ghi"} {
		w := call(t, r, "/api/projects", header)
		if strings.Contains(message(t, w), "no bearer token") {
			t.Fatalf("%q was read as carrying no token", header)
		}
	}
}

// A refusal a client can act on says so in the header, per RFC 6750.
func TestARefusalNamesTheScheme(t *testing.T) {
	w := call(t, guarded(configured{}), "/api/projects", "")
	if got := w.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer ") {
		t.Fatalf("WWW-Authenticate was %q", got)
	}
}

// An unconfigured platform is the operator's problem. Answering "invalid token"
// would send whoever reads it hunting a token that was never at fault.
func TestAnUnconfiguredPlatformSaysSoInsteadOfBlamingTheToken(t *testing.T) {
	w := call(t, guarded(noOidc{}), "/api/projects", "Bearer abc.def.ghi")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("answered %d, wanted 401", w.Code)
	}
	m := message(t, w)
	// Every place the provider can be named must appear, or the reader edits
	// the one the message happens to mention and the platform stays silent.
	for _, where := range []string{"identity.oidc.authority", "oidc.issuerUri", "OIDC_AUTHORITY", "OIDC_CLIENT_ID"} {
		if !strings.Contains(m, where) {
			t.Fatalf("the message never mentions %s: %q", where, m)
		}
	}
	// And it says which of the two is missing, so the reader edits that one.
	if !strings.Contains(m, "neither its issuer nor its client id") {
		t.Fatalf("the message does not name what is missing: %q", m)
	}
}

// The exemption is matched on the route pattern, not on where the middleware
// was declared: an exemption expressed by registration order would be undone,
// silently, by whoever moves a line.
func TestTheExemptionFollowsThePathNotTheRegistrationOrder(t *testing.T) {
	r := guarded(configured{})

	if w := call(t, r, "/api/capabilities", ""); w.Code != http.StatusOK {
		t.Fatalf("the bootstrap answered %d without a token, wanted 200", w.Code)
	}
	if w := call(t, r, "/api/projects", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("a guarded route answered %d without a token, wanted 401", w.Code)
	}
}

func TestIdentityIsAbsentWhenNoneWasEstablished(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if _, ok := Identity(c); ok {
		t.Fatal("an identity was reported on a request that carried none")
	}
}
