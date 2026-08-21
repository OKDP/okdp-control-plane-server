package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-control-plane-server/internal/models"
)

type fakeCapabilities struct {
	enabled bool
	err     error
}

func (f fakeCapabilities) GetCapabilities(context.Context) (*models.Capabilities, error) {
	return &models.Capabilities{}, nil
}

func (f fakeCapabilities) IdentityAPIEnabled(context.Context) (bool, error) {
	return f.enabled, f.err
}

// identityGuarded builds the identity group as the router does.
func identityGuarded(providerIsKubauth, crdsInstalled bool) *gin.Engine {
	handler := NewCapabilitiesHandler(fakeCapabilities{enabled: providerIsKubauth})
	engine := gin.New()
	group := engine.Group("/api/v1/identity", handler.RequireIdentityAPI(
		func(c *gin.Context) bool { return crdsInstalled },
	))
	group.GET("/users", func(c *gin.Context) { c.JSON(http.StatusOK, []string{"alice"}) })
	return engine
}

func unavailableBody(t *testing.T, raw []byte) FeatureUnavailable {
	t.Helper()
	var body FeatureUnavailable
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("expected a JSON body, got %q", raw)
	}
	if body.Reason != ReasonFeatureNotInstalled {
		t.Errorf("expected reason %q, got %q", ReasonFeatureNotInstalled, body.Reason)
	}
	if body.Feature != identityFeature {
		t.Errorf("expected feature %q, got %q", identityFeature, body.Feature)
	}
	return body
}

func TestAnotherProviderAnswers501(t *testing.T) {
	response := call(identityGuarded(false, false), http.MethodGet, "/api/v1/identity/users")

	if response.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", response.Code)
	}
	body := unavailableBody(t, response.Body.Bytes())
	if !strings.Contains(body.Error, "identity.provider is not kubauth") {
		t.Errorf("expected the message to name the provider, got %q", body.Error)
	}
}

func TestKubauthWithoutCRDsNamesTheMisconfiguration(t *testing.T) {
	response := call(identityGuarded(true, false), http.MethodGet, "/api/v1/identity/users")

	if response.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", response.Code)
	}
	body := unavailableBody(t, response.Body.Bytes())
	if !strings.Contains(body.Error, "CRDs are not installed") {
		t.Errorf("expected the message to name the missing CRDs, got %q", body.Error)
	}
}

func TestKubauthWithCRDsPassesThrough(t *testing.T) {
	response := call(identityGuarded(true, true), http.MethodGet, "/api/v1/identity/users")

	if response.Code != http.StatusOK {
		t.Fatalf("expected the request to reach the handler, got %d", response.Code)
	}
}

func TestUnreadableContextStays500(t *testing.T) {
	handler := NewCapabilitiesHandler(fakeCapabilities{err: errors.New("context unreachable")})
	engine := gin.New()
	group := engine.Group("/api/v1/identity", handler.RequireIdentityAPI(
		func(c *gin.Context) bool { return true },
	))
	group.GET("/users", func(c *gin.Context) { c.JSON(http.StatusOK, nil) })

	response := call(engine, http.MethodGet, "/api/v1/identity/users")

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on an unreadable Context, got %d", response.Code)
	}
}
