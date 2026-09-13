package router

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/okdp/okdp-control-plane-server/internal/config"
)

// Rejects every token: enough to prove the middleware gate itself, since a
// request with no Authorization header never reaches the verifier at all.
type refuseAllVerifier struct{}

func (refuseAllVerifier) Verify(context.Context, string) error {
	return errors.New("no token accepted in this test")
}

// Handlers are nil on purpose: a route that slipped past the middleware would
// dereference one and panic, so a gap in the guard cannot read as a pass.
func testRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	return SetupRouter(&config.Config{}, refuseAllVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil)
}

// Repeated from the middleware on purpose: one more exemption has to be added
// twice, so it cannot happen by accident.
var publicRoutesForTest = map[string]bool{
	"/api/capabilities": true,
}

// fillParams turns a route pattern into a callable path.
func fillParams(pattern string) string {
	parts := strings.Split(pattern, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, ":") || strings.HasPrefix(p, "*") {
			parts[i] = "x"
		}
	}
	return strings.Join(parts, "/")
}

func TestEveryApiRouteRefusesAnAnonymousCaller(t *testing.T) {
	r := testRouter()

	checked := 0
	for _, route := range r.Routes() {
		if !strings.HasPrefix(route.Path, "/api") || publicRoutesForTest[route.Path] {
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
		t.Fatal("the console bootstrap requires a token, so the console can never learn which issuer to use")
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

// A preflight carries no Authorization header by design.
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
