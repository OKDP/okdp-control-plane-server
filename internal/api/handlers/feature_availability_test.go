package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// guarded builds a route group behind the feature guard, as the router does.
func guarded(available bool) *gin.Engine {
	engine := gin.New()
	group := engine.Group("/api/v1/identity", RequireFeature(
		func(c *gin.Context) bool { return available },
		"kubauth identity",
		"Identity management is not available on this cluster: the kubauth CRDs are not installed.",
	))
	group.GET("/users", func(c *gin.Context) { c.JSON(http.StatusOK, []string{"alice"}) })
	group.POST("/users", func(c *gin.Context) { c.JSON(http.StatusCreated, gin.H{}) })
	return engine
}

func call(engine *gin.Engine, method, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

// The point of the lot: a missing CRD is a feature that was never installed,
// not a server that broke. 500 sent readers hunting for a fault that was not
// there, and filled the logs with errors on every page load.
func TestMissingFeatureAnswers501(t *testing.T) {
	response := call(guarded(false), http.MethodGet, "/api/v1/identity/users")

	if response.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", response.Code)
	}

	var body FeatureUnavailable
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected a JSON body, got %q", response.Body.String())
	}
	// The console keys off the reason, not the prose, so the message can be
	// reworded without breaking it.
	if body.Reason != ReasonFeatureNotInstalled {
		t.Errorf("expected reason %q, got %q", ReasonFeatureNotInstalled, body.Reason)
	}
	if body.Feature != "kubauth identity" {
		t.Errorf("expected the feature to be named, got %q", body.Feature)
	}
	if body.Error == "" {
		t.Error("expected a message a human can act on")
	}
}

// Writes are guarded too: creating a user against absent CRDs must not reach
// the handler and fail halfway.
func TestMissingFeatureGuardsWritesToo(t *testing.T) {
	response := call(guarded(false), http.MethodPost, "/api/v1/identity/users")

	if response.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501 on a write, got %d", response.Code)
	}
}

// And the guard must be transparent once the CRDs are there.
func TestAvailableFeaturePassesThrough(t *testing.T) {
	response := call(guarded(true), http.MethodGet, "/api/v1/identity/users")

	if response.Code != http.StatusOK {
		t.Fatalf("expected the request to reach the handler, got %d", response.Code)
	}
	if response.Body.String() != `["alice"]` {
		t.Errorf("expected the handler's own body, got %q", response.Body.String())
	}
}
