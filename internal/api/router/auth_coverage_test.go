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

// noOidc stands for a platform that publishes no OIDC client. It is enough for
// this file: what is under test is whether a request without a token is stopped
// before it reaches a handler, not what a valid token establishes.
type noOidc struct{}

func (noOidc) GetIdentityOidcConfig(context.Context) (*models.IdentityOidcConfig, error) {
	return nil, nil
}

func (noOidc) GetOidcIssuerURI(context.Context) (string, error) { return "", nil }

// anonymousRoutes are the only paths served without a token, and the list is
// repeated here on purpose. Exempting one more route then has to be done twice,
// in the middleware and in this test, which is the point: it cannot happen by
// accident.
var anonymousRoutes = map[string]bool{
	"/api/capabilities": true,
}

// The handlers are nil. That is deliberate: a route that slips past the
// middleware calls a nil pointer and blows up, so a gap cannot be mistaken for
// a pass.
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

// The regression this guards, and the reason the middleware sits on the group
// rather than on each route: the API served 68 endpoints with no authentication
// at all, and any route added later would have had to be guarded by hand.
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

// The console reads the issuer and the client id from /api/capabilities before
// it can build its OIDC client. Requiring a token here would leave it unable to
// ever obtain one.
func TestTheConsoleBootstrapStaysAnonymous(t *testing.T) {
	r := testRouter()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))

	// The handler is nil here, so it panics and Recovery answers 500. Anything
	// but 401 proves the middleware let the request through, which is all this
	// test is about.
	if w.Code == http.StatusUnauthorized {
		t.Fatal("the console bootstrap requires a token, so the console can never sign in")
	}
}

// The kubelet probes carry no token, and the API description carries no data.
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

// A browser sends OPTIONS before a cross-origin call, without an Authorization
// header. Answering 401 to it would make every call fail at the preflight, and
// the browser would report a CORS error naming nothing.
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
