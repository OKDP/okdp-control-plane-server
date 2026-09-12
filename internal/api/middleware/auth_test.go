package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// Accepts exactly one token, so a test can tell "no token" from "wrong token".
type stubVerifier struct {
	accepted string
}

func (s stubVerifier) Verify(_ context.Context, rawToken string) error {
	if rawToken != s.accepted {
		return errors.New("signature mismatch")
	}
	return nil
}

func newTestRouter(verifier stubVerifier) *gin.Engine {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	api := r.Group("/api")
	api.Use(RequireAuthentication(verifier))
	api.GET("/capabilities", func(c *gin.Context) { c.Status(http.StatusOK) })
	api.GET("/projects", func(c *gin.Context) { c.Status(http.StatusOK) })

	return r
}

func call(r *gin.Engine, method, path, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAProtectedRouteRefusesARequestWithoutAToken(t *testing.T) {
	r := newTestRouter(stubVerifier{accepted: "good"})

	response := call(r, http.MethodGet, "/api/projects", "")

	assert.Equal(t, http.StatusUnauthorized, response.Code)
	assert.Equal(t, `Bearer realm="okdp"`, response.Header().Get("WWW-Authenticate"))
}

func TestAProtectedRouteRefusesAnUnverifiableToken(t *testing.T) {
	r := newTestRouter(stubVerifier{accepted: "good"})

	response := call(r, http.MethodGet, "/api/projects", "Bearer forged")

	assert.Equal(t, http.StatusUnauthorized, response.Code)
	// The answer says the token was rejected and not why: which check failed is
	// the one thing worth knowing to get past the next one.
	assert.NotContains(t, response.Body.String(), "signature mismatch")
}

// A header that is not "Bearer <token>" is a client mistake, not an attack, but
// it must not reach the verifier and be reported as a bad signature.
func TestAMalformedAuthorizationHeaderIsRefused(t *testing.T) {
	r := newTestRouter(stubVerifier{accepted: "good"})

	for _, header := range []string{"good", "Basic good", "Bearer", "Bearer   "} {
		response := call(r, http.MethodGet, "/api/projects", header)
		assert.Equal(t, http.StatusUnauthorized, response.Code, "header %q", header)
	}
}

// The console fetches the capabilities before it knows which issuer to send the
// user to, so it cannot hold a token yet. Protecting this route makes the whole
// console unbootable, which no test elsewhere would catch.
func TestCapabilitiesStaysReachableWithoutAToken(t *testing.T) {
	r := newTestRouter(stubVerifier{accepted: "good"})

	response := call(r, http.MethodGet, "/api/capabilities", "")

	assert.Equal(t, http.StatusOK, response.Code)
}
