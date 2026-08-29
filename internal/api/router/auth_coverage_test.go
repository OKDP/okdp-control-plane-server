package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/config"
	"github.com/okdp/okdp-control-plane-server/internal/models"
)

// noOidc stands for a platform that publishes no OIDC client.
type noOidc struct{}

func (noOidc) GetIdentityOidcConfig(context.Context) (*models.IdentityOidcConfig, error) {
	return nil, nil
}

func (noOidc) GetOidcIssuerURI(context.Context) (string, error) { return "", nil }

// Repeated from the middleware on purpose: one more exemption has to be added
// twice, so it cannot happen by accident.
var anonymousRoutes = map[string]bool{
	"/api/capabilities": true,
}

// Handlers are nil on purpose: a route slipping past the middleware panics,
// so a gap cannot read as a pass.
func testRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	return SetupRouter(&config.Config{}, auth.NewVerifier(noOidc{}, auth.Fallback{}), nil, nil, nil, nil, nil, nil, nil, nil)
}

// fillParams turns a route pattern into a callable path.
func fillParams(pattern string) string {
	parts := strings.Split(pattern, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, ":") {
			parts[i] = "x"
		}
		if strings.HasPrefix(p, "*") {
			parts[i] = "x"
		}
	}
	return strings.Join(parts, "/")
}

func TestEveryApiRouteRefusesAnAnonymousCaller(t *testing.T) {
	r := testRouter()

	checked := 0
	for _, route := range r.Routes() {
		if !strings.HasPrefix(route.Path, "/api") || anonymousRoutes[route.Path] {
			continue
		}
		checked++
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(route.Method, fillParams(route.Path), nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s answered %d to a request with no token, wanted 401", route.Method, route.Path, w.Code)
		}
	}

	// A pass because nothing was inspected would be worse than a failure.
	if checked < 50 {
		t.Fatalf("only %d API routes were checked, the router does not look fully registered", checked)
	}
	t.Logf("%d API routes refuse an anonymous caller", checked)
}

func TestTheConsoleBootstrapStaysAnonymous(t *testing.T) {
	r := testRouter()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))

	// Nil handler panics into a 500; anything but 401 proves the middleware let
	// the request through.
	if w.Code == http.StatusUnauthorized {
		t.Fatal("the console bootstrap requires a token, so the console can never sign in")
	}
}

func TestHealthAndSwaggerStayOutsideTheGuardedGroup(t *testing.T) {
	r := testRouter()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/health answered %d without a token, wanted 200", w.Code)
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/swagger/index.html", nil))
	if w.Code == http.StatusUnauthorized {
		t.Fatal("/swagger requires a token, which no browser will send")
	}
}

// A preflight OPTIONS carries no Authorization header by design.
func TestThePreflightIsNotRefused(t *testing.T) {
	r := testRouter()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/api/projects", nil)
	req.Header.Set("Origin", "http://localhost:4200")
	req.Header.Set("Access-Control-Request-Method", "GET")
	r.ServeHTTP(w, req)

	if w.Code == http.StatusUnauthorized {
		t.Fatal("the CORS preflight was refused, so every cross-origin call fails before it is sent")
	}
}
