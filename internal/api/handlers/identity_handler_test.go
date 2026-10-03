package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

// fakeIdentityService only answers Available; the embedded interface makes
// any other call panic, so a request leaking past the guard cannot pass.
type fakeIdentityService struct {
	service.IdentityService
	available bool
}

func (f fakeIdentityService) Available(context.Context) bool { return f.available }

// identityGuarded builds the identity group as the router does.
func identityGuarded(available bool) *gin.Engine {
	handler := NewIdentityHandler(fakeIdentityService{available: available})
	engine := gin.New()
	group := engine.Group("/api/v1/identity", handler.RequireAPI())
	group.GET("/users", func(c *gin.Context) { c.JSON(http.StatusOK, []string{"alice"}) })
	group.POST("/groups", func(c *gin.Context) { c.JSON(http.StatusCreated, gin.H{}) })
	return engine
}

// Without Keycloak admin credentials the identity API is absent from this
// installation, not broken: 501 with the FeatureUnavailable contract.
func TestIdentityWithoutKeycloakCredentialsAnswers501(t *testing.T) {
	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/identity/users"},
		{http.MethodPost, "/api/v1/identity/groups"},
	} {
		response := call(identityGuarded(false), req.method, req.path)
		if response.Code != http.StatusNotImplemented {
			t.Fatalf("%s %s: expected 501, got %d", req.method, req.path, response.Code)
		}
		var body FeatureUnavailable
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("expected a JSON body, got %q", response.Body.String())
		}
		if body.Reason != ReasonFeatureNotInstalled {
			t.Errorf("expected reason %q, got %q", ReasonFeatureNotInstalled, body.Reason)
		}
		if body.Feature != identityFeature {
			t.Errorf("expected feature %q, got %q", identityFeature, body.Feature)
		}
		if body.Error == "" {
			t.Error("expected a message naming the missing configuration")
		}
	}
}

func TestIdentityWithKeycloakCredentialsPassesThrough(t *testing.T) {
	response := call(identityGuarded(true), http.MethodGet, "/api/v1/identity/users")

	if response.Code != http.StatusOK {
		t.Fatalf("expected the request to reach the handler, got %d", response.Code)
	}
}

// captureUpdate records what the handler hands the service on PUT.
type captureUpdate struct {
	fakeIdentityService
	name string
	user *models.User
}

func (c *captureUpdate) UpdateUser(_ context.Context, name string, user *models.User) error {
	c.name, c.user = name, user
	return nil
}

// The path names the login. The display name in the body must survive an
// update: the console sends both, and overwriting one with the other renamed
// every edited user to their login.
func TestUpdateUserKeepsTheDisplayName(t *testing.T) {
	svc := &captureUpdate{fakeIdentityService: fakeIdentityService{available: true}}
	engine := gin.New()
	engine.PUT("/users/:name", NewIdentityHandler(svc).UpdateUser)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/users/jdoe", strings.NewReader(`{"username":"jdoe","name":"John Doe","groups":["team-a"]}`))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if svc.name != "jdoe" || svc.user.Username != "jdoe" {
		t.Errorf("expected the path to name the user, got %q / %q", svc.name, svc.user.Username)
	}
	if svc.user.Name != "John Doe" {
		t.Errorf("expected the display name to be kept, got %q", svc.user.Name)
	}
	var echoed models.User
	if err := json.Unmarshal(w.Body.Bytes(), &echoed); err != nil || echoed.Name != "John Doe" {
		t.Errorf("expected the response to echo the user, got %s", w.Body.String())
	}
}

// captureCreate accepts every user.
type captureCreate struct {
	fakeIdentityService
}

func (captureCreate) CreateUser(context.Context, *models.User) error { return nil }

// The password is write-only: neither create nor update may send it back.
func TestUserResponsesNeverCarryThePassword(t *testing.T) {
	engine := gin.New()
	engine.POST("/users", NewIdentityHandler(captureCreate{fakeIdentityService{available: true}}).CreateUser)
	engine.PUT("/users/:name", NewIdentityHandler(&captureUpdate{fakeIdentityService: fakeIdentityService{available: true}}).UpdateUser)

	for _, call := range []struct{ method, path string }{
		{http.MethodPost, "/users"},
		{http.MethodPut, "/users/jdoe"},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(call.method, call.path, strings.NewReader(`{"username":"jdoe","name":"John Doe","password":"s3cr3t-value"}`))
		req.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(w, req)

		if w.Code >= 300 {
			t.Fatalf("%s %s: %d %s", call.method, call.path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "s3cr3t-value") || strings.Contains(w.Body.String(), `"password"`) {
			t.Errorf("%s %s echoed the password: %s", call.method, call.path, w.Body.String())
		}
	}
}
